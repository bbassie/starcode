package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"starcode/internal/agent"
)

// TestCapsLoadIsStale checks a catalog read from disk is served but
// fetched again: the binary that wrote it may have read the CLI's model
// list differently (it once had every model at 200K).
func TestCapsLoadIsStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capabilities.json")
	raw, _ := json.Marshal(capsFile{Entries: map[string]capsFileEntry{
		"claude": {Caps: agent.Capabilities{Models: []agent.Model{{ID: "opus", ContextWindow: 200_000}}}, Fetched: time.Now()},
	}})
	os.WriteFile(path, raw, 0o644)
	c := capsCache{path: path}
	c.load()
	e, ok := c.entries["claude"]
	if !ok || len(e.caps.Models) != 1 {
		t.Fatalf("the saved catalog was not loaded: %+v", e)
	}
	if time.Since(e.fetched) < capsTTL {
		t.Fatal("a loaded catalog counts as fresh and is not fetched again")
	}
}

// TestRestartWithNothingNew checks a restart tap on a banner drawn before
// the last restart reconnects the page instead of restarting again.
func TestRestartWithNothingNew(t *testing.T) {
	var restarts atomic.Int32
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Update: &SelfUpdate{Home: "/bin/sc"}}
	s.OnRestart = func() { restarts.Add(1) }
	post := func() string {
		w := httptest.NewRecorder()
		s.restart(w, httptest.NewRequest("POST", "/api/restart", nil))
		return w.Body.String()
	}
	if body := post(); !strings.Contains(body, "new Event('stream')") {
		t.Fatalf("no reconnect in the answer:\n%s", body)
	}
	time.Sleep(300 * time.Millisecond)
	if restarts.Load() != 0 {
		t.Fatal("restarted with nothing new to run")
	}
	s.Update.changed.Store(true)
	post()
	time.Sleep(300 * time.Millisecond)
	if n := restarts.Load(); n != 1 {
		t.Fatalf("restarts after a rebuild = %d, want 1", n)
	}
}
