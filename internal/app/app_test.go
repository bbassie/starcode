package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"starcode/internal/agent"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/store"
)

func TestExpandProjectPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	got, err := expandProjectPath("~")
	if err != nil {
		t.Fatal(err)
	}
	if got != home {
		t.Errorf("expandProjectPath(~) = %q, want %q", got, home)
	}

	got, err = expandProjectPath("~/projects/starcode")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "projects", "starcode")
	if got != want {
		t.Errorf("expandProjectPath(~/...) = %q, want %q", got, want)
	}
}

type queueAgent struct{ session *queueSession }

func (a *queueAgent) Name() string { return "queue-test" }
func (a *queueAgent) Start(context.Context, agent.Config) (agent.Session, error) {
	return a.session, nil
}

type queueSession struct {
	events chan agent.Event
	sent   chan string
	once   sync.Once
}

func newQueueSession() *queueSession {
	return &queueSession{events: make(chan agent.Event, 16), sent: make(chan string, 16)}
}

func (s *queueSession) Send(_ context.Context, text string) error {
	s.sent <- text
	return nil
}
func (s *queueSession) Interrupt(context.Context) error                       { return nil }
func (s *queueSession) Resolve(context.Context, string, agent.Decision) error { return nil }
func (s *queueSession) Events() <-chan agent.Event                            { return s.events }
func (s *queueSession) Close() error {
	s.once.Do(func() { close(s.events) })
	return nil
}

func TestSendPromptQueuesAndDispatchesInOrder(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	projectDir := t.TempDir()
	if _, err := st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: projectDir, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	sess := newQueueSession()
	a := New(st, bus.New(64), map[string]agent.Agent{"queue-test": &queueAgent{session: sess}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.Shutdown()
	threadID, err := a.CreateThread(ctx, "p1", "queue-test", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := a.SendPrompt(ctx, threadID, "first"); err != nil {
		t.Fatal(err)
	}
	if got := receivePrompt(t, sess.sent); got != "first" {
		t.Fatalf("first send = %q", got)
	}
	if err := a.SendPrompt(ctx, threadID, "second"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendPrompt(ctx, threadID, "third"); err != nil {
		t.Fatal(err)
	}
	queued, err := st.QueuedPrompts(ctx, threadID)
	if err != nil || len(queued) != 2 {
		t.Fatalf("queued = %+v, %v", queued, err)
	}
	if err := a.CancelQueuedPrompt(ctx, threadID, queued[1].ID); err != nil {
		t.Fatal(err)
	}

	sess.events <- agent.Event{Kind: agent.KindThreadTitle, ThreadTitle: &agent.ThreadTitle{Title: "Agent generated title"}}
	sess.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "turn-1", Status: "done", DurationMS: 10}}
	if got := receivePrompt(t, sess.sent); got != "second" {
		t.Fatalf("queued send = %q", got)
	}

	deadline := time.Now().Add(time.Second)
	for {
		queued, err = st.QueuedPrompts(ctx, threadID)
		if err == nil && len(queued) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue did not drain: %+v, %v", queued, err)
		}
		time.Sleep(time.Millisecond)
	}
	items, err := st.Items(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Kind != domain.KindUser || items[0].Body != "first" || items[1].Kind != domain.KindResult || items[2].Kind != domain.KindUser || items[2].Body != "second" {
		t.Fatalf("transcript order = %+v", items)
	}
	thread, err := st.Thread(ctx, threadID)
	if err != nil || thread.Title != "Agent generated title" {
		t.Fatalf("thread title = %q, %v", thread.Title, err)
	}
}

func receivePrompt(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case prompt := <-ch:
		return prompt
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for prompt")
		return ""
	}
}

func TestSendPromptUnarchives(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "arch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"}); err != nil {
		t.Fatal(err)
	}
	sess := newQueueSession()
	a := New(st, bus.New(64), map[string]agent.Agent{"queue-test": &queueAgent{session: sess}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.Shutdown()
	threadID, err := a.CreateThread(ctx, "p1", "queue-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ArchiveThread(ctx, threadID); err != nil {
		t.Fatal(err)
	}
	if th, _ := st.Thread(ctx, threadID); !th.Archived {
		t.Fatalf("not archived: %+v", th)
	}
	if err := a.SendPrompt(ctx, threadID, "hello again"); err != nil {
		t.Fatal(err)
	}
	receivePrompt(t, sess.sent)
	if th, _ := st.Thread(ctx, threadID); th.Archived || th.Status != domain.StatusRunning {
		t.Fatalf("after send: %+v", th)
	}
}
