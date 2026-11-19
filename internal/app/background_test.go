package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/store"
)

func bgEvent(id, kind, label string, done bool) agent.Event {
	return agent.Event{Kind: agent.KindBackground, Background: &agent.Background{ID: id, Kind: kind, Label: label, Done: done, Status: "completed"}}
}

// Work that ends while no turn runs gets a line in the transcript; work
// still running when the process closes is named ahead of the next
// prompt, once.
func TestBackgroundLostWithTheProcess(t *testing.T) {
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	id, _ := a.CreateThread(ctx, "p1", "fresh-test", "")
	if err := a.SendPrompt(ctx, id, "first"); err != nil {
		t.Fatal(err)
	}
	s := ag.session(0)
	receivePrompt(t, s.sent)
	s.events <- bgEvent("t1", agent.BackgroundSubagent, "review the diff", false)
	s.events <- bgEvent("t2", agent.BackgroundCommand, "npm run dev", false)
	finishTurn(t, st, id, s, "a1")
	s.events <- bgEvent("t1", "", "", true)
	waitFor(t, func() bool {
		n := systemNotes(t, st, id)
		return len(n) > 0 && n[len(n)-1] == "Background subagent finished: review the diff"
	})
	s.events <- agent.Event{Kind: agent.KindClosed, Closed: &agent.Closed{}}
	waitFor(t, func() bool { bg, _ := st.Background(ctx, id); return len(bg) == 1 && bg[0].Lost })

	if err := a.SendPrompt(ctx, id, "second"); err != nil {
		t.Fatal(err)
	}
	got := receivePrompt(t, ag.session(1).sent)
	if !strings.HasPrefix(got, "[starcode] Your agent process was closed") || !strings.Contains(got, "- command: npm run dev\n") || strings.Contains(got, "review the diff") || !strings.HasSuffix(got, "second") {
		t.Fatalf("prompt:\n%s", got)
	}
	if bg, _ := st.Background(ctx, id); len(bg) != 0 {
		t.Fatalf("still listed after the note: %+v", bg)
	}
	finishTurn(t, st, id, ag.session(1), "a2")
	if err := a.SendPrompt(ctx, id, "third"); err != nil {
		t.Fatal(err)
	}
	if got := receivePrompt(t, ag.session(1).sent); got != "third" {
		t.Fatalf("the note went out twice: %q", got)
	}
}

// After a restart, a thread whose agent waited on background work gets
// a prompt naming it; one with only a background command (a dev server)
// keeps the note for its next prompt.
func TestBackgroundWakeAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bg.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := &freshAgent{}
	a := New(st, bus.New(64), map[string]agent.Agent{"fresh-test": ag}, log)
	run := func(i int, prompt, conv string, bg agent.Event) string {
		id, _ := a.CreateThread(ctx, "p1", "fresh-test", "")
		if err := a.SendPrompt(ctx, id, prompt); err != nil {
			t.Fatal(err)
		}
		s := ag.session(i)
		receivePrompt(t, s.sent)
		s.events <- agent.Event{Kind: agent.KindSessionInfo, SessionInfo: &agent.SessionInfo{ExternalID: conv}}
		s.events <- bg
		finishTurn(t, st, id, s, "a")
		waitFor(t, func() bool { b, _ := st.Background(ctx, id); return len(b) == 1 })
		return id
	}
	waiting := run(0, "fan out", "conv1", bgEvent("t1", agent.BackgroundSubagent, "look into the logs", false))
	server := run(1, "start the server", "conv2", bgEvent("t2", agent.BackgroundCommand, "go run .", false))
	a.closing.Store(true)
	a.Shutdown()
	time.Sleep(20 * time.Millisecond)
	st.Close()

	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ag2 := &freshAgent{}
	b := New(st, bus.New(64), map[string]agent.Agent{"fresh-test": ag2}, log)
	defer b.Shutdown()
	if err := b.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b.ResumeCutOff(ctx)
	if ag2.started() != 1 || ag2.config(0).ResumeID != "conv1" {
		t.Fatalf("started %d sessions", ag2.started())
	}
	got := receivePrompt(t, ag2.session(0).sent)
	if !strings.Contains(got, "- subagent: look into the logs") || !strings.HasSuffix(got, "Carry on with the task.") || strings.HasPrefix(got, "[starcode]") {
		t.Fatalf("wake prompt:\n%s", got)
	}
	ps := userItems(t, st, waiting)
	if m := promptMeta(ps[len(ps)-1]); m.Auto != "resume" {
		t.Fatalf("wake meta %+v", m)
	}
	if bg, _ := st.Background(ctx, server); len(bg) != 1 || !bg[0].Lost {
		t.Fatalf("server thread: %+v", bg)
	}
}

// A background command is listed by the description its tool call gave,
// not by the command line the CLI reports.
func TestBackgroundLabelIsTheCallsDescription(t *testing.T) {
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	id, _ := a.CreateThread(ctx, "p1", "fresh-test", "")
	if err := a.SendPrompt(ctx, id, "first"); err != nil {
		t.Fatal(err)
	}
	s := ag.session(0)
	receivePrompt(t, s.sent)
	s.events <- agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: "call1", Name: "Bash",
		Input: []byte(`{"command":"cd /x && go test ./...","description":"Run the unit tests","run_in_background":true}`)}}
	bg := bgEvent("t1", agent.BackgroundCommand, "cd /x && go test ./...", false)
	bg.Background.ToolID = "call1"
	s.events <- bg
	s.events <- bgEvent("t2", agent.BackgroundCommand, "npm run dev", false)
	waitFor(t, func() bool { b, _ := st.Background(ctx, id); return len(b) == 2 })
	b, _ := st.Background(ctx, id)
	if b[0].Label != "Run the unit tests" || b[1].Label != "npm run dev" {
		t.Fatalf("labels %q, %q", b[0].Label, b[1].Label)
	}
}
