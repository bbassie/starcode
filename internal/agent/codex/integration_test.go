package codex

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"starcode/internal/agent"
)

// These tests talk to a real `codex app-server` and spend tokens. They are
// skipped with -short or when codex is not on PATH.

func startIntegration(t *testing.T, dir string) agent.Session {
	t.Helper()
	if testing.Short() {
		t.Skip("-short")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not on PATH")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := New(WithLogger(log))
	t.Cleanup(func() { a.Shutdown() })
	s, err := a.Start(context.Background(), agent.Config{Cwd: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// collect drains events until pred matches or the deadline passes,
// returning everything seen so far.
func collect(t *testing.T, s agent.Session, pred func(agent.Event) bool, timeout time.Duration) []agent.Event {
	t.Helper()
	var seen []agent.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events closed; seen: %s", describe(seen))
			}
			seen = append(seen, ev)
			if pred(ev) {
				return seen
			}
		case <-deadline:
			t.Fatalf("timeout; seen: %s", describe(seen))
		}
	}
}

func describe(evs []agent.Event) string {
	out := ""
	for _, e := range evs {
		out += "\n  " + string(e.Kind)
		switch e.Kind {
		case agent.KindToolStarted:
			out += " " + e.ToolStarted.Name + " " + e.ToolStarted.Summary
		case agent.KindToolCompleted:
			out += " " + e.ToolCompleted.Status
		case agent.KindTurnCompleted:
			out += " " + e.TurnCompleted.Status
		case agent.KindNotice:
			out += " " + e.Notice.Text
		}
	}
	return out
}

func TestIntegrationFileChangeAndCommand(t *testing.T) {
	dir := t.TempDir()
	s := startIntegration(t, dir)

	seen := collect(t, s, func(e agent.Event) bool { return e.Kind == agent.KindSessionInfo }, 30*time.Second)
	if seen[len(seen)-1].SessionInfo.ExternalID == "" {
		t.Fatal("empty session id")
	}

	if err := s.Send(t.Context(), "Create a file named hello.txt containing exactly: hi from starcode. Then run the shell command `cat hello.txt`. Do not ask questions."); err != nil {
		t.Fatalf("Send: %v", err)
	}
	seen = collect(t, s, func(e agent.Event) bool { return e.Kind == agent.KindTurnCompleted }, 3*time.Minute)
	t.Logf("events: %s", describe(seen))

	var tools, completed int
	for _, e := range seen {
		switch e.Kind {
		case agent.KindToolStarted:
			tools++
		case agent.KindToolCompleted:
			completed++
		}
	}
	if tools == 0 || completed == 0 {
		t.Errorf("expected tool events, got %d started / %d completed", tools, completed)
	}
	done := seen[len(seen)-1].TurnCompleted
	if done.Status != "done" {
		t.Errorf("turn status = %q (%s)", done.Status, done.Error)
	}
	if done.DurationMS <= 0 {
		t.Errorf("DurationMS = %d", done.DurationMS)
	}
	if done.InputTokens <= 0 || done.OutputTokens <= 0 {
		t.Errorf("tokens = %d in / %d out", done.InputTokens, done.OutputTokens)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "hello.txt")); err != nil || string(b) != "hi from starcode\n" && string(b) != "hi from starcode" {
		t.Errorf("hello.txt = %q, %v", b, err)
	}
}
