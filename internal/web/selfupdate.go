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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"starcode/internal/domain"
	"starcode/internal/web/views"
)

// SelfUpdate notices when there is a binary to restart into. `make build`
// renames a fresh binary over the home path (the one the service was
// started with), so a size or mtime change there means a new version is
// waiting. A `make build` in one of the project's worktrees leaves a
// binary in that worktree instead; those are watched too, and the banner
// offers a restart into each, named by its branch. While a branch binary
// runs, the bar stays up with a way back to the home binary. Nothing
// restarts on its own: a restart kills running turns, so the reader picks
// the moment.
//
// HomeExeEnv carries the home path across a re-exec into a branch
// binary, since os.Executable then names the worktree's file.
const HomeExeEnv = "STARCODE_HOME_EXE"

type SelfUpdate struct {
	// Home is the binary the service was started with; Running the one
	// this process is (the same unless a branch binary was chosen).
	Home    string
	Running string
	// Lister names the worktrees of the project the home binary lives in,
	// with their branch and thread title; set by the server.
	Lister func(ctx context.Context) []Worktree

	size    int64
	modTime time.Time
	started time.Time
	changed atomic.Bool
	log     *slog.Logger

	mu     sync.Mutex
	builds []Build
	target string
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

// Worktree is one checkout of the project, as the Lister reports it.
type Worktree struct {
	Path, Branch, Title string
}

// Build is a binary in a worktree that is newer than this process.
type Build struct {
	Path, Branch, Title string
	ModTime             time.Time
}

func NewSelfUpdate(log *slog.Logger) *SelfUpdate {
	su := &SelfUpdate{log: log, started: time.Now()}
	path, err := os.Executable()
	if err != nil {
		return su
	}
	su.Running = strings.TrimSuffix(path, " (deleted)")
	su.Home = su.Running
	if h := os.Getenv(HomeExeEnv); h != "" {
		su.Home = h
	}
	if info, err := os.Stat(su.Home); err == nil {
		su.size, su.modTime = info.Size(), info.ModTime()
	}
	return su
}

// Changed reports whether a newer home binary has been seen on disk.
func (su *SelfUpdate) Changed() bool { return su != nil && su.changed.Load() }

// OnBranch is the branch this process was built from, "" on the home
// binary; the title is the thread that owns the worktree.
func (su *SelfUpdate) OnBranch(ctx context.Context) (branch, title string) {
	if su == nil || su.Running == su.Home || su.Lister == nil {
		return "", ""
	}
	dir := filepath.Dir(su.Running)
	for _, w := range su.Lister(ctx) {
		if w.Path == dir {
			return w.Branch, w.Title
		}
	}
	return filepath.Base(dir), ""
}

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

// Builds are the worktree binaries newer than the home one, newest first.
func (su *SelfUpdate) Builds() []Build {
	if su == nil {
		return nil
	}
	su.mu.Lock()
	defer su.mu.Unlock()
	return append([]Build(nil), su.builds...)
}

// Target is the binary the next restart runs: what the reader chose, or
// the home binary.
func (su *SelfUpdate) Target() string {
	if su == nil {
		return ""
	}
	su.mu.Lock()
	defer su.mu.Unlock()
	if su.target != "" {
		return su.target
	}
	return su.Home
}

// choose records the binary the reader wants next; only the home binary
// and a listed build qualify.
func (su *SelfUpdate) choose(path string) bool {
	su.mu.Lock()
	defer su.mu.Unlock()
	if path == "" || path == su.Home {
		su.target = su.Home
		return true
	}
	for _, b := range su.builds {
		if b.Path == path {
			su.target = path
			return true
		}
	}
	return false
}

// Watch polls until ctx ends: the home binary every five seconds, the
// worktrees every fifteen. Each finding publishes BinaryUpdated once.
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
			if n++; n%3 == 0 && su.scanWorktrees(ctx) {
				publish(domain.BinaryUpdated{})
			}
			// The home checkout against origin: at the second tick, then
			// every five minutes, the PR poll's pace.
			if n == 2 || n%60 == 0 {
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

// scanHome fetches origin for the home checkout and counts how far
// behind it is. Only a checkout on its default branch, clean, with no
// local commits ahead, is reported: that is the one a fast-forward pull
// brings up without a merge. True when the count changed.
func (su *SelfUpdate) scanHome(ctx context.Context) bool {
	behind := 0
	if _, err := su.gitHome(ctx, 60*time.Second, "fetch", "--quiet", "origin"); err == nil {
		branch, _ := su.gitHome(ctx, 10*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
		def, _ := su.gitHome(ctx, 10*time.Second, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		status, _ := su.gitHome(ctx, 10*time.Second, "status", "--porcelain")
		counts, err := su.gitHome(ctx, 10*time.Second, "rev-list", "--left-right", "--count", "HEAD...@{u}")
		if err == nil && branch != "" && def == "origin/"+branch && status == "" {
			var ahead int
			if _, err := fmt.Sscanf(counts, "%d\t%d", &ahead, &behind); err != nil || ahead > 0 {
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
		out, err := su.gitHome(ctx, 2*time.Minute, "pull", "--ff-only", "--quiet", "origin")
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

// scanWorktrees lists the binaries built in the project's worktrees since
// this process started; true when the list changed.
func (su *SelfUpdate) scanWorktrees(ctx context.Context) bool {
	if su.Lister == nil {
		return false
	}
	name := filepath.Base(su.Home)
	// A branch build counts while it is newer than the home binary; a
	// home build after it (the merge, usually) makes it stale. The one
	// running now is not a change to offer.
	homeMod := su.modTime
	if info, err := os.Stat(su.Home); err == nil {
		homeMod = info.ModTime()
	}
	var found []Build
	for _, w := range su.Lister(ctx) {
		p := filepath.Join(w.Path, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if !info.ModTime().After(homeMod) || p == su.Running {
			continue
		}
		found = append(found, Build{Path: p, Branch: w.Branch, Title: w.Title, ModTime: info.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ModTime.After(found[j].ModTime) })
	su.mu.Lock()
	defer su.mu.Unlock()
	changed := len(found) != len(su.builds)
	for i := range found {
		if changed {
			break
		}
		if found[i].Path != su.builds[i].Path || !found[i].ModTime.Equal(su.builds[i].ModTime) {
			changed = true
		}
	}
	if changed {
		for _, b := range found {
			su.log.Info("branch binary on disk, restart into it from the banner", "path", b.Path, "branch", b.Branch)
		}
		su.builds = found
	}
	return changed
}

// restart answers first, then hands over to main through OnRestart; the
// stream reconnects on its own once the new process listens. ?into=
// names a branch build, or the home binary when empty.
func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	if s.OnRestart == nil {
		s.fail(w, r, errRestartUnavailable)
		return
	}
	into := r.URL.Query().Get("into")
	if !s.Update.choose(into) {
		s.fail(w, r, errUnknownBuild)
		return
	}
	s.Log.Info("restart requested", "from", r.RemoteAddr, "exe", s.Update.Target())
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

// selfWorktrees lists the worktrees of the project the home binary lives
// in (starcode developing itself), from the threads that own them.
func (s *Server) selfWorktrees(ctx context.Context) []Worktree {
	if s.Update == nil || s.Update.Home == "" {
		return nil
	}
	home := filepath.Dir(s.Update.Home)
	ps, err := s.App.Store.Projects(ctx)
	if err != nil {
		return nil
	}
	var pid string
	for _, p := range ps {
		if p.Path == home {
			pid = p.ID
		}
	}
	if pid == "" {
		return nil
	}
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Worktree
	for _, t := range ts {
		if t.ProjectID != pid || t.Worktree == "" || seen[t.Worktree] {
			continue
		}
		seen[t.Worktree] = true
		out = append(out, Worktree{Path: t.Worktree, Branch: t.WorktreeBranch, Title: t.Title})
	}
	return out
}

// bannerData is what the update banner shows for this connection.
func (s *Server) bannerData(ctx context.Context) views.BannerData {
	d := views.BannerData{HomeChanged: s.Update.Changed(), Behind: s.Update.Behind()}
	d.PullRunning, d.PullErr, d.PullLog = s.Update.Pull()
	d.Branch, d.BranchTitle = s.Update.OnBranch(ctx)
	for _, b := range s.Update.Builds() {
		d.Builds = append(d.Builds, views.BranchBuild{Path: b.Path, Branch: b.Branch, Title: b.Title, At: b.ModTime})
	}
	if d.HomeChanged || d.Branch != "" || len(d.Builds) > 0 || d.Behind > 0 {
		d.Running = s.runningThreads(ctx)
	}
	return d
}
