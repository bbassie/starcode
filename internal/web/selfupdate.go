package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/domain"
	"starcode/internal/web/views"
)

// SelfUpdate notices when there is a binary to restart into. `make build`
// renames a fresh binary over the home path (the one the service was
// started with), so a size or mtime change there means a new version is
// waiting. Nothing restarts on its own: a restart kills running turns,
// so the reader picks the moment.
type SelfUpdate struct {
	// Home is the binary the service was started with, and the one a
	// restart runs.
	Home string

	size    int64
	modTime time.Time
	started time.Time
	changed atomic.Bool
	log     *slog.Logger

	// rescan asks the watcher to check the home checkout now (a merge
	// the PR poll saw) instead of at the next interval.
	rescan chan struct{}

	mu sync.Mutex
	// behind is how many commits the home checkout is behind its upstream
	// (0 when it is not on its default branch, not clean, ahead, or has
	// no upstream); pull is the state of the last "pull and build".
	behind int
	pull   pullState
}

// pullState is one "pull and build" of the home checkout.
type pullState struct {
	running bool
	err     string
	log     string
}

func NewSelfUpdate(log *slog.Logger) *SelfUpdate {
	su := &SelfUpdate{log: log, started: time.Now(), rescan: make(chan struct{}, 1)}
	path, err := os.Executable()
	if err != nil {
		return su
	}
	su.Home = strings.TrimSuffix(path, " (deleted)")
	if info, err := os.Stat(su.Home); err == nil {
		su.size, su.modTime = info.Size(), info.ModTime()
	}
	return su
}

// HomeStamp names the home binary as it is on disk now, so a dismissed
// "rebuilt" row comes back for the next build.
func (su *SelfUpdate) HomeStamp() string {
	if su == nil {
		return ""
	}
	if info, err := os.Stat(su.Home); err == nil {
		return info.ModTime().UTC().Format(time.RFC3339)
	}
	return ""
}

// Changed reports whether a newer home binary has been seen on disk.
func (su *SelfUpdate) Changed() bool { return su != nil && su.changed.Load() }

// Behind is how many commits the home checkout is behind origin, when a
// fast-forward pull would bring it up; 0 otherwise.
func (su *SelfUpdate) Behind() int {
	if su == nil {
		return 0
	}
	su.mu.Lock()
	defer su.mu.Unlock()
	return su.behind
}

// Pull is the state of the last "pull and build".
func (su *SelfUpdate) Pull() (running bool, errText, logTail string) {
	if su == nil {
		return false, "", ""
	}
	su.mu.Lock()
	defer su.mu.Unlock()
	return su.pull.running, su.pull.err, su.pull.log
}

// ScanHomeSoon has the watcher check the home checkout against origin
// at its next tick; the PR poll calls it when a pull request merged.
func (su *SelfUpdate) ScanHomeSoon() {
	if su == nil {
		return
	}
	select {
	case su.rescan <- struct{}{}:
	default:
	}
}

// Watch polls until ctx ends: the home binary every five seconds, the
// home checkout against origin every two minutes or when asked. Each finding publishes BinaryUpdated once.
func (su *SelfUpdate) Watch(ctx context.Context, publish func(...any)) {
	if su.Home == "" {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !su.modTime.IsZero() {
				info, err := os.Stat(su.Home)
				if err == nil && (info.Size() != su.size || !info.ModTime().Equal(su.modTime)) && su.changed.CompareAndSwap(false, true) {
					su.log.Info("new binary on disk, restart to use it", "path", su.Home)
					publish(domain.BinaryUpdated{})
				}
			}
			n++
			asked := false
			select {
			case <-su.rescan:
				asked = true
			default:
			}
			if asked || n == 2 || n%24 == 0 {
				if su.scanHome(ctx) {
					publish(domain.BinaryUpdated{})
				}
			}
		}
	}
}

// gitHome runs git in the home checkout with a bound on its time.
func (su *SelfUpdate) gitHome(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// --no-optional-locks: the status check must not take index.lock
	// under a git command the user is running in the same checkout.
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = filepath.Dir(su.Home)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// scanHome fetches the home checkout's upstream and counts how far
// behind it is. Only a clean checkout on a branch with an upstream and
// no local commits ahead is reported: that is the one a fast-forward
// pull brings up without a merge. Anything else is logged at debug and
// shown as nothing. True when the count changed.
func (su *SelfUpdate) scanHome(ctx context.Context) bool {
	behind := 0
	if _, err := su.gitHome(ctx, 60*time.Second, "fetch", "--quiet"); err != nil {
		su.log.Debug("home checkout fetch failed", "err", err)
	} else {
		status, _ := su.gitHome(ctx, 10*time.Second, "status", "--porcelain")
		counts, err := su.gitHome(ctx, 10*time.Second, "rev-list", "--left-right", "--count", "HEAD...@{u}")
		var ahead int
		switch {
		case err != nil:
			su.log.Debug("home checkout has no upstream", "out", counts)
		case status != "":
			su.log.Debug("home checkout has local changes, no pull offered")
		default:
			if _, err := fmt.Sscanf(counts, "%d %d", &ahead, &behind); err != nil || ahead > 0 {
				if ahead > 0 {
					su.log.Debug("home checkout is ahead of its upstream, no pull offered", "ahead", ahead)
				}
				behind = 0
			}
		}
	}
	su.mu.Lock()
	defer su.mu.Unlock()
	if behind == su.behind {
		return false
	}
	if behind > 0 {
		su.log.Info("home checkout is behind origin, pull and build from the banner", "behind", behind)
	}
	su.behind = behind
	return true
}

// PullAndBuild fast-forwards the home checkout and runs make build in it,
// in the background; the watcher then sees the new binary. One at a time.
func (su *SelfUpdate) PullAndBuild(publish func(...any)) error {
	su.mu.Lock()
	if su.pull.running {
		su.mu.Unlock()
		return errors.New("a pull and build is already running")
	}
	su.pull = pullState{running: true}
	su.mu.Unlock()
	publish(domain.BinaryUpdated{})
	go func() {
		ctx := context.Background()
		out, err := su.gitHome(ctx, 2*time.Minute, "pull", "--ff-only", "--quiet")
		if err == nil {
			// templ lives in ~/go/bin, which the service's PATH may lack.
			ctx2, cancel := context.WithTimeout(ctx, 5*time.Minute)
			cmd := exec.CommandContext(ctx2, "make", "build")
			cmd.Dir = filepath.Dir(su.Home)
			cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH")+":"+filepath.Join(os.Getenv("HOME"), "go", "bin"))
			var b []byte
			b, err = cmd.CombinedOutput()
			cancel()
			out = strings.TrimSpace(string(b))
		}
		su.mu.Lock()
		su.pull = pullState{}
		if err != nil {
			su.pull.err = err.Error()
			su.pull.log = tailLines(out, 8)
			su.log.Warn("pull and build failed", "err", err, "out", su.pull.log)
		} else {
			su.behind = 0
			su.log.Info("pulled and built the home checkout")
		}
		su.mu.Unlock()
		publish(domain.BinaryUpdated{})
	}()
	return nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// restart answers first, then hands over to main through OnRestart; the
// stream reconnects on its own once the new process listens.
func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	if s.OnRestart == nil {
		s.fail(w, r, errRestartUnavailable)
		return
	}
	// A tap on a banner the page drew before the last restart: this
	// process already runs the newest home binary, so there is nothing
	// to restart into. The page reconnects instead, which clears the row.
	if !s.Update.Changed() {
		s.Log.Info("restart request with nothing new to run, reconnecting the page", "from", r.RemoteAddr)
		datastar.NewSSE(w, r).ExecuteScript("window.dispatchEvent(new Event('stream'))")
		return
	}
	s.Log.Info("restart requested", "from", r.RemoteAddr, "exe", s.Update.Home)
	s.ok(w, r)
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.OnRestart()
	}()
}

// pullMain is the banner's "pull and build" button.
func (s *Server) pullMain(w http.ResponseWriter, r *http.Request) {
	if err := s.Update.PullAndBuild(s.App.Bus.Publish); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Log.Info("pull and build requested", "from", r.RemoteAddr)
	s.ok(w, r)
}

// runningThreads counts turns a restart would cut off.
func (s *Server) runningThreads(ctx context.Context) int {
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, t := range ts {
		if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
			n++
		}
	}
	return n
}

// bannerData is what the update banner shows.
func (s *Server) bannerData(ctx context.Context) views.BannerData {
	d := views.BannerData{HomeChanged: s.Update.Changed(), Behind: s.Update.Behind(), HomeKey: s.Update.HomeStamp()}
	d.PullRunning, d.PullErr, d.PullLog = s.Update.Pull()
	if d.HomeChanged || d.Behind > 0 {
		d.Running = s.runningThreads(ctx)
		d.Resume = s.App.Store.ResumeAfterRestart(ctx)
	}
	return d
}
