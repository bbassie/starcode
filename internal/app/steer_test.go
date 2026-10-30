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

// steerSession is a queueSession that takes steered messages, or refuses
// them when refuse is set.
type steerSession struct {
	*queueSession
	steers chan [2]string
	refuse bool
}

func (s *steerSession) Steer(_ context.Context, id, text string) error {
	if s.refuse {
		return agent.ErrNoSteer
	}
	s.steers <- [2]string{id, text}
	return nil
}

type steerAgent struct{ session *steerSession }

func (a *steerAgent) Name() string { return "steer-test" }
func (a *steerAgent) Start(context.Context, agent.Config) (agent.Session, error) {
	return a.session, nil
}

func newSteerApp(t *testing.T) (*App, *store.Store, *steerSession, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "steer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"})
	sess := &steerSession{queueSession: newQueueSession(), steers: make(chan [2]string, 4)}
	a := New(st, bus.New(64), map[string]agent.Agent{"steer-test": &steerAgent{session: sess}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.Shutdown)
	id, _ := a.CreateThread(ctx, "p1", "steer-test", "")
	if err := a.SendPrompt(ctx, id, "running"); err != nil {
		t.Fatal(err)
	}
	receivePrompt(t, sess.sent)
	return a, st, sess, id
}

func receiveSteer(t *testing.T, ch <-chan [2]string) [2]string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a steer")
		return [2]string{}
	}
}

// userBodies is the thread's user items in transcript order, a steered
// one marked with a "~".
func userBodies(st *store.Store, id string) string {
	items, _ := st.Items(context.Background(), id)
	var out []string
	for _, it := range items {
		switch {
		case it.Kind == domain.KindUser && strings.Contains(string(it.Meta), `"steered":true`):
			out = append(out, "~"+it.Body)
		case it.Kind == domain.KindUser:
			out = append(out, it.Body)
		case it.Kind == domain.KindTool:
			out = append(out, "tool")
		}
	}
	return strings.Join(out, ",")
}

// A steered prompt waits above the composer until the agent takes it,
// then goes into the transcript at that point, and the turn goes on.
func TestSteerGoesInWhereTheAgentTakesIt(t *testing.T) {
	ctx := context.Background()
	a, st, sess, id := newSteerApp(t)
	sess.events <- agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: "t1", Name: "Bash"}}
	if err := a.SteerPrompt(ctx, id, "also this"); err != nil {
		t.Fatal(err)
	}
	got := receiveSteer(t, sess.steers)
	if got[1] != "also this" {
		t.Fatalf("steered %q", got[1])
	}
	if s, _ := st.SteeredPrompts(ctx, id); len(s) != 1 {
		t.Fatalf("steered prompts %+v", s)
	}
	if q, _ := st.QueuedPrompts(ctx, id); len(q) != 0 {
		t.Fatalf("a steered prompt is still queued: %+v", q)
	}
	sess.events <- agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: "t1", Status: "done"}}
	sess.events <- agent.Event{Kind: agent.KindSteered, Steered: &agent.Steered{ID: got[0]}}
	sess.events <- agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: "t2", Name: "Bash"}}
	waitFor(t, func() bool { return userBodies(st, id) == "running,tool,~also this,tool" })
	if s, _ := st.SteeredPrompts(ctx, id); len(s) != 0 {
		t.Fatalf("taken prompt still waits: %+v", s)
	}
	sess.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "t", Status: "done"}}
	waitStatus(t, st, id, domain.StatusIdle)
	if n, err := st.Replay(ctx); err != nil || n == 0 {
		t.Fatal(err)
	}
	if got := userBodies(st, id); got != "running,tool,~also this,tool" {
		t.Fatalf("after replay %s", got)
	}
}

// A steer the turn ended too early for is the session's to deliver: the
// thread stays busy and the queue waits until the session starts a turn
// with it.
func TestLateSteerStartsTheNextTurn(t *testing.T) {
	ctx := context.Background()
	a, st, sess, id := newSteerApp(t)
	a.SteerPrompt(ctx, id, "late")
	got := receiveSteer(t, sess.steers)
	a.SendPrompt(ctx, id, "queued")
	sess.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "t1", Status: "done"}}
	sess.events <- agent.Event{Kind: agent.KindTurnStarted, TurnStarted: &agent.TurnStarted{TurnID: "t2"}}
	sess.events <- agent.Event{Kind: agent.KindSteered, Steered: &agent.Steered{ID: got[0]}}
	waitFor(t, func() bool { return userBodies(st, id) == "running,~late" })
	select {
	case p := <-sess.sent:
		t.Fatalf("the queue went ahead of the steer: %q", p)
	default:
	}
	if th, _ := st.Thread(ctx, id); th.Status != domain.StatusRunning {
		t.Fatalf("status %q", th.Status)
	}
	sess.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "t2", Status: "done"}}
	if p := receivePrompt(t, sess.sent); p != "queued" {
		t.Fatalf("next %q", p)
	}
}

// A session that never delivers a late steer does not leave the thread
// busy for good: after steerGrace the prompt goes back to the queue and
// starts.
func TestUndeliveredSteerGoesBackToTheQueue(t *testing.T) {
	ctx := context.Background()
	defer func(d time.Duration) { steerGrace = d }(steerGrace)
	steerGrace = 20 * time.Millisecond
	a, _, sess, id := newSteerApp(t)
	a.SteerPrompt(ctx, id, "lost")
	receiveSteer(t, sess.steers)
	sess.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "t1", Status: "done"}}
	if p := receivePrompt(t, sess.sent); p != "lost" {
		t.Fatalf("next %q", p)
	}
}

// An agent that cannot steer now leaves the prompt in the queue, and
// "now" on a queued prompt steers it in.
func TestSteerFallsBackToTheQueue(t *testing.T) {
	ctx := context.Background()
	a, st, sess, id := newSteerApp(t)
	sess.refuse = true
	a.SteerPrompt(ctx, id, "later")
	q, _ := st.QueuedPrompts(ctx, id)
	if len(q) != 1 || q[0].Body != "later" {
		t.Fatalf("queue %+v", q)
	}
	sess.refuse = false
	if err := a.SteerQueued(ctx, id, q[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := receiveSteer(t, sess.steers); got[0] != q[0].ID {
		t.Fatalf("steered %v", got)
	}
	if s, _ := st.SteeredPrompts(ctx, id); len(s) != 1 {
		t.Fatalf("steered prompts %+v", s)
	}
	// The session going away puts it back in the queue.
	sess.events <- agent.Event{Kind: agent.KindClosed, Closed: &agent.Closed{}}
	waitFor(t, func() bool { q, _ := st.QueuedPrompts(ctx, id); return len(q) == 1 })
}
