package web

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"starcode/internal/domain"
)

// SelfUpdate notices when the executable on disk is no longer the one that
// is running. `make build` renames a fresh binary over the old path, so a
// size or mtime change means a new version is waiting; every open page
// then gets a banner with a restart button. Nothing restarts on its own:
// a restart kills running turns, so the reader picks the moment.
type SelfUpdate struct {
	// Path is the executable as it was at startup. os.Executable reports
	// "(deleted)" once the file is replaced, so it is captured early and
	// also used by main for the re-exec.
	Path    string
	size    int64
	modTime time.Time
	changed atomic.Bool
	log     *slog.Logger
}

func NewSelfUpdate(log *slog.Logger) *SelfUpdate {
	su := &SelfUpdate{log: log}
	path, err := os.Executable()
	if err != nil {
		return su
	}
	su.Path = strings.TrimSuffix(path, " (deleted)")
	if info, err := os.Stat(su.Path); err == nil {
		su.size, su.modTime = info.Size(), info.ModTime()
	}
	return su
}

// Changed reports whether a newer binary has been seen on disk.
func (su *SelfUpdate) Changed() bool { return su != nil && su.changed.Load() }

// Watch polls the path until ctx ends and publishes BinaryUpdated once.
func (su *SelfUpdate) Watch(ctx context.Context, publish func(...any)) {
	if su.Path == "" || su.modTime.IsZero() {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			info, err := os.Stat(su.Path)
			if err != nil || (info.Size() == su.size && info.ModTime().Equal(su.modTime)) {
				continue
			}
			if su.changed.CompareAndSwap(false, true) {
				su.log.Info("new binary on disk, restart to use it", "path", su.Path)
				publish(domain.BinaryUpdated{})
			}
		}
	}
}

// restart answers first, then hands over to main through OnRestart; the
// stream reconnects on its own once the new process listens.
func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	if s.OnRestart == nil {
		s.fail(w, r, errRestartUnavailable)
		return
	}
	s.Log.Info("restart requested", "from", r.RemoteAddr)
	s.ok(w, r)
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.OnRestart()
	}()
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
