package app

import (
	"context"
	"encoding/json"
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

func TestSessionRuleSurvivesRestartAndRevoke(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "rules.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"}); err != nil {
		t.Fatal(err)
	}
	sess := newQueueSession()
	a := New(st, bus.New(64), map[string]agent.Agent{"queue-test": &queueAgent{session: sess}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	threadID, err := a.CreateThread(ctx, "p1", "queue-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SendPrompt(ctx, threadID, "run tests"); err != nil {
		t.Fatal(err)
	}
	receivePrompt(t, sess.sent)
	sess.events <- agent.Event{Kind: agent.KindApproval, Approval: &agent.ApprovalRequested{ID: "ap1", ToolID: "t1", ToolName: "Bash", Input: json.RawMessage(`{"command":"go test ./..."}`)}}
	waitFor(t, func() bool { aps, _ := st.PendingApprovals(ctx, threadID); return len(aps) == 1 })
	if err := a.ResolveApproval(ctx, threadID, "ap1", domain.DecisionAllowSession); err != nil {
		t.Fatal(err)
	}
	if rules := a.SessionRules(ctx, threadID); len(rules) != 1 || rules[0] != "Bash:go" {
		t.Fatalf("rules = %v", rules)
	}
	a.Shutdown()
	st.Close()

	// A new process on the same database still has the rule, and it
	// answers the next matching request on its own.
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess = newQueueSession()
	a = New(st, bus.New(64), map[string]agent.Agent{"queue-test": &queueAgent{session: sess}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.Shutdown()
	if err := a.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if rules := a.SessionRules(ctx, threadID); len(rules) != 1 {
		t.Fatalf("rules after restart = %v", rules)
	}
	if err := a.SendPrompt(ctx, threadID, "again"); err != nil {
		t.Fatal(err)
	}
	receivePrompt(t, sess.sent)
	sess.events <- agent.Event{Kind: agent.KindApproval, Approval: &agent.ApprovalRequested{ID: "ap2", ToolID: "t2", ToolName: "Bash", Input: json.RawMessage(`{"command":"go vet"}`)}}
	waitFor(t, func() bool { ap, err := st.Approval(ctx, threadID, "ap2"); return err == nil && ap.Decision == domain.DecisionAllowSession })
	if th, _ := st.Thread(ctx, threadID); th.Status != domain.StatusRunning {
		t.Fatalf("auto-allowed request changed status to %q", th.Status)
	}
	if err := a.RevokeRule(ctx, threadID, "Bash:go"); err != nil {
		t.Fatal(err)
	}
	if rules := a.SessionRules(ctx, threadID); len(rules) != 0 {
		t.Fatalf("rules after revoke = %v", rules)
	}
	if err := a.RevokeRule(ctx, threadID, "Bash:go"); err == nil {
		t.Fatal("revoking twice should fail")
	}
	sess.events <- agent.Event{Kind: agent.KindApproval, Approval: &agent.ApprovalRequested{ID: "ap3", ToolID: "t3", ToolName: "Bash", Input: json.RawMessage(`{"command":"go build"}`)}}
	waitFor(t, func() bool { th, _ := st.Thread(ctx, threadID); return th.Status == domain.StatusAwaitingApproval })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// restartAgent hands out a new session per Start and remembers the Config
// of each, so a test can see the flags a resumed session was started with.
type restartAgent struct {
	live    bool
	mu      sync.Mutex
	configs []agent.Config
	started chan *restartSession
}

func newRestartAgent(live bool) *restartAgent {
	return &restartAgent{live: live, started: make(chan *restartSession, 4)}
}

func (a *restartAgent) Name() string { return "restart-test" }
func (a *restartAgent) Start(_ context.Context, cfg agent.Config) (agent.Session, error) {
	a.mu.Lock()
	a.configs = append(a.configs, cfg)
	a.mu.Unlock()
	s := &restartSession{queueSession: newQueueSession(), modes: make(chan string, 4)}
	a.started <- s
	if a.live {
		return &liveModeSession{s}, nil
	}
	return s, nil
}

// restartSession is a queueSession that announces its end the way a real
// adapter does: a Closed event, then the channel closes.
type restartSession struct {
	*queueSession
	modes chan string
}

func (s *restartSession) Close() error {
	s.once.Do(func() {
		s.events <- agent.Event{Kind: agent.KindClosed, Closed: &agent.Closed{}}
		close(s.events)
	})
	return nil
}

// liveModeSession is a restartSession whose agent takes mode switches
// during a turn.
type liveModeSession struct{ *restartSession }

func (s *liveModeSession) SetPermissionMode(_ context.Context, mode string) error {
	s.modes <- mode
	return nil
}

func startedSession(t *testing.T, ag *restartAgent) *restartSession {
	t.Helper()
	select {
	case s := <-ag.started:
		return s
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a session to start")
		return nil
	}
}

func systemNotes(t *testing.T, st *store.Store, threadID string) []string {
	t.Helper()
	items, err := st.Items(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, it := range items {
		if it.Kind == domain.KindSystem {
			notes = append(notes, it.Body)
		}
	}
	return notes
}

func newRestartApp(t *testing.T, ag *restartAgent) (*App, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"}); err != nil {
		t.Fatal(err)
	}
	a := New(st, bus.New(64), map[string]agent.Agent{"restart-test": ag}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.Shutdown)
	threadID, err := a.CreateThread(ctx, "p1", "restart-test", "")
	if err != nil {
		t.Fatal(err)
	}
	return a, st, threadID
}

func TestSetThreadSettingsDuringTurnSwitchesModeLive(t *testing.T) {
	ctx := context.Background()
	ag := newRestartAgent(true)
	a, st, threadID := newRestartApp(t, ag)

	if err := a.SendPrompt(ctx, threadID, "first"); err != nil {
		t.Fatal(err)
	}
	first := startedSession(t, ag)
	receivePrompt(t, first.sent)

	if err := a.SetThreadSettings(ctx, threadID, ThreadSettings{Agent: "restart-test", PermissionMode: "yolo"}); err != nil {
		t.Fatal(err)
	}
	select {
	case mode := <-first.modes:
		if mode != "yolo" {
			t.Fatalf("live mode = %q", mode)
		}
	case <-time.After(time.Second):
		t.Fatal("the running session was not told the new mode")
	}
	if notes := systemNotes(t, st, threadID); len(notes) != 1 || notes[0] != "permission mode is now yolo" {
		t.Fatalf("notes = %q", notes)
	}
	thread, err := st.Thread(ctx, threadID)
	if err != nil || thread.PermissionMode != "yolo" {
		t.Fatalf("thread mode = %q, %v", thread.PermissionMode, err)
	}

	// The session took the change, so the next turn keeps using it.
	first.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "turn-1", Status: "done"}}
	waitStatus(t, st, threadID, domain.StatusIdle)
	if err := a.SendPrompt(ctx, threadID, "second"); err != nil {
		t.Fatal(err)
	}
	if got := receivePrompt(t, first.sent); got != "second" {
		t.Fatalf("second prompt went to %q", got)
	}
	if len(ag.started) != 0 {
		t.Fatal("a new session was started although the old one took the mode")
	}
}

func TestSetThreadSettingsDuringTurnRestartsForNextTurn(t *testing.T) {
	ctx := context.Background()
	ag := newRestartAgent(false)
	a, st, threadID := newRestartApp(t, ag)

	if err := a.SendPrompt(ctx, threadID, "first"); err != nil {
		t.Fatal(err)
	}
	first := startedSession(t, ag)
	receivePrompt(t, first.sent)
	if err := a.SendPrompt(ctx, threadID, "second"); err != nil {
		t.Fatal(err)
	}

	if err := a.SetThreadSettings(ctx, threadID, ThreadSettings{Agent: "restart-test", Model: "m2", PermissionMode: "yolo"}); err != nil {
		t.Fatal(err)
	}
	notes := systemNotes(t, st, threadID)
	if len(notes) != 2 || notes[0] != "permission mode yolo applies from the next turn; this turn keeps its current mode" || notes[1] != "model and effort changes apply from the next turn" {
		t.Fatalf("notes = %q", notes)
	}

	// The turn ends: the queued prompt must start on a fresh session that
	// carries the new flags, and the old session going away must not
	// touch the new turn's status.
	first.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "turn-1", Status: "done"}}
	second := startedSession(t, ag)
	if got := receivePrompt(t, second.sent); got != "second" {
		t.Fatalf("queued prompt = %q", got)
	}
	ag.mu.Lock()
	cfgs := append([]agent.Config(nil), ag.configs...)
	ag.mu.Unlock()
	if len(cfgs) != 2 || cfgs[1].PermissionMode != "yolo" || cfgs[1].Model != "m2" {
		t.Fatalf("configs = %+v", cfgs)
	}
	select {
	case <-first.sent:
		t.Fatal("the retired session got the queued prompt")
	default:
	}
	// Give the retired session's Closed event time to be handled.
	time.Sleep(50 * time.Millisecond)
	thread, err := st.Thread(ctx, threadID)
	if err != nil || thread.Status != domain.StatusRunning {
		t.Fatalf("status after the handoff = %q, %v", thread.Status, err)
	}
}

func waitStatus(t *testing.T, st *store.Store, threadID, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		thread, err := st.Thread(context.Background(), threadID)
		if err == nil && thread.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want %q (%v)", thread.Status, want, err)
		}
		time.Sleep(time.Millisecond)
	}
}
