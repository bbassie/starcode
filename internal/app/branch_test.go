package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// threeTurns runs the prompts one, two and three on a new thread, the
// first binding session conv-1; turn n ends on anchor-n.
func threeTurns(t *testing.T, a *App, st *store.Store, ag *freshAgent) string {
	t.Helper()
	ctx := context.Background()
	id, err := a.CreateThread(ctx, "p1", "fresh-test", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"one", "two", "three"} {
		if err := a.SendPrompt(ctx, id, p); err != nil {
			t.Fatal(err)
		}
		s := ag.session(0)
		receivePrompt(t, s.sent)
		if i == 0 {
			s.events <- agent.Event{Kind: agent.KindSessionInfo, SessionInfo: &agent.SessionInfo{ExternalID: "conv-1"}}
		}
		finishTurn(t, st, id, s, "anchor-"+string(rune('1'+i)))
	}
	return id
}

// TestBranchInFrontOfAPrompt branches before the second prompt: the new
// thread has the first exchange, the second prompt as its draft, and its
// session forks the conversation where the rewind would; the original
// keeps all three.
func TestBranchInFrontOfAPrompt(t *testing.T) {
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	id := threeTurns(t, a, st, ag)
	prompts := userItems(t, st, id)

	bid, draft, err := a.Branch(ctx, id, prompts[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft != "two" {
		t.Fatalf("draft = %q", draft)
	}
	if d, _ := st.Draft(ctx, bid); d != "two" {
		t.Fatalf("stored draft = %q", d)
	}
	got := userItems(t, st, bid)
	if len(got) != 1 || got[0].Body != "one" || got[0].ID == prompts[0].ID {
		t.Fatalf("branch prompts = %+v", got)
	}
	if !got[0].CreatedAt.Equal(prompts[0].CreatedAt) {
		t.Fatalf("copied prompt time %v, original %v", got[0].CreatedAt, prompts[0].CreatedAt)
	}
	if n := len(userItems(t, st, id)); n != 3 {
		t.Fatalf("original has %d prompts", n)
	}
	b, _ := st.Thread(ctx, bid)
	if b.ExternalSessionID != "conv-1" || b.ForkAt != "anchor-1" || b.Agent != "fresh-test" || !strings.HasSuffix(b.Title, "(branch)") {
		t.Fatalf("branch thread = %+v", b)
	}

	if err := a.SendPrompt(ctx, bid, "two, differently"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return ag.started() == 2 })
	if cfg := ag.config(1); cfg.ResumeID != "conv-1" || cfg.ForkAt != "anchor-1" {
		t.Fatalf("branch session started with %+v", cfg)
	}

	// In front of the first prompt there is nothing to fork.
	bid2, _, err := a.Branch(ctx, id, prompts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := st.Thread(ctx, bid2)
	if b2.ExternalSessionID != "" || b2.Title != "new thread" || len(userItems(t, st, bid2)) != 0 {
		t.Fatalf("branch before the first prompt = %+v", b2)
	}
}

// TestBranchWholeThread branches after the last turn, which forks at
// its anchor, and is refused while a turn runs.
func TestBranchWholeThread(t *testing.T) {
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	id := threeTurns(t, a, st, ag)

	bid, draft, err := a.Branch(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.Thread(ctx, bid)
	if draft != "" || b.ExternalSessionID != "conv-1" || b.ForkAt != "anchor-3" || len(userItems(t, st, bid)) != 3 {
		t.Fatalf("branch = %+v, draft %q", b, draft)
	}

	if err := a.SendPrompt(ctx, id, "four"); err != nil {
		t.Fatal(err)
	}
	receivePrompt(t, ag.session(0).sent)
	if _, _, err := a.Branch(ctx, id, ""); err == nil {
		t.Fatal("a running thread was branched")
	}
}

// TestHandOff hands a thread to another agent: a new thread on it in the
// same checkout, a fresh session, and a first prompt that attaches the
// conversation as a file.
func TestHandOff(t *testing.T) {
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	other := &freshAgent{}
	a.SetAgent("other-test", other)
	a.AttachDir = t.TempDir()
	id := threeTurns(t, a, st, ag)

	if _, err := a.HandOff(ctx, id, Handoff{Agent: "fresh-test"}); err == nil {
		t.Fatal("handed off to the agent it runs on")
	}
	// The original waits out a usage limit, which the handoff calls off.
	if _, err := st.Append(ctx, id, domain.ThreadSnoozed{Until: time.Now().Add(time.Hour), Resume: true}); err != nil {
		t.Fatal(err)
	}
	hid, err := a.HandOff(ctx, id, Handoff{Agent: "other-test", PermissionMode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return other.started() == 1 })
	if cfg := other.config(0); cfg.ResumeID != "" || cfg.PermissionMode != "plan" {
		t.Fatalf("handoff session started with %+v", cfg)
	}
	sent := receivePrompt(t, other.session(0).sent)
	i := strings.Index(sent, "is saved at: ")
	if i < 0 {
		t.Fatalf("prompt has no attachment: %q", sent)
	}
	path := strings.TrimSuffix(sent[i+len("is saved at: "):], "]")
	conv, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Me", "one", "two", "three", "fresh-test"} {
		if !strings.Contains(string(conv), want) {
			t.Fatalf("conversation file lacks %q:\n%s", want, conv)
		}
	}
	if th, _ := st.Thread(ctx, id); th.Snoozed() || th.SnoozeResume {
		t.Fatalf("the original still waits to resume: %+v", th)
	}
	h, _ := st.Thread(ctx, hid)
	if h.Agent != "other-test" || h.ExternalSessionID != "" {
		t.Fatalf("handoff thread = %+v", h)
	}
	prompts := userItems(t, st, hid)
	if len(prompts) != 4 {
		t.Fatalf("handoff thread has %d prompts", len(prompts))
	}
	var m PromptMeta
	json.Unmarshal(prompts[3].Meta, &m)
	if m.Auto != "handoff" {
		t.Fatalf("handoff prompt meta = %+v", m)
	}
	// The copies still name the agent they went to, so a rewind to one
	// is refused rather than forking a session the new agent cannot read.
	if _, err := a.Rewind(ctx, hid, prompts[1].ID); err == nil {
		t.Fatal("rewound to a prompt of the other agent")
	}
}

// TestCopyItems checks copies get new ids, their output, and parent
// links that point at the copies.
func TestCopyItems(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	items := []store.Item{
		{ID: "task", Kind: domain.KindTool, ToolName: "Task", Status: domain.ItemDone, Meta: json.RawMessage(`{"summary":"look around"}`), CreatedAt: at},
		{ID: "sub", Kind: domain.KindTool, ToolName: "Read", Status: domain.ItemDone, Output: "file body", Meta: json.RawMessage(`{"parent":"task"}`), CreatedAt: at},
	}
	evs := (&App{}).copyItems(items, "new")
	if len(evs) != 3 {
		t.Fatalf("events = %d", len(evs))
	}
	task := evs[0].(domain.ItemStarted)
	sub := evs[1].(domain.ItemStarted)
	out := evs[2].(domain.ItemDelta)
	if task.ID == "task" || sub.ID == "sub" || !task.At.Equal(at) || !task.Quiet {
		t.Fatalf("copies = %+v %+v", task, sub)
	}
	var m map[string]string
	json.Unmarshal(sub.Meta, &m)
	if m["parent"] != task.ID {
		t.Fatalf("parent = %q, want %q", m["parent"], task.ID)
	}
	if out.ID != sub.ID || out.Field != "output" || out.Text != "file body" {
		t.Fatalf("output = %+v", out)
	}
}

// TestTranscriptCap checks a long conversation keeps its first prompt
// and its end.
func TestTranscriptCap(t *testing.T) {
	big := strings.Repeat("x", 1000)
	var items []store.Item
	items = append(items, store.Item{Kind: domain.KindUser, Body: "the task"})
	for i := 0; i < 400; i++ {
		items = append(items, store.Item{Kind: domain.KindAssistant, Body: big})
	}
	items = append(items, store.Item{Kind: domain.KindAssistant, Body: "the end"})
	out := Transcript(store.Thread{Title: "t", Agent: "claude"}, store.Project{Path: "/p"}, items)
	if len(out) > transcriptCap || !strings.Contains(out, "the task") || !strings.Contains(out, "the end") || !strings.Contains(out, "earlier entries left out") {
		t.Fatalf("capped transcript: %d bytes", len(out))
	}
}

// TestUsageLimitResumes checks a turn the usage limit stopped snoozes the
// thread until the reset, holds the queue, and resumes when the snooze
// runs out; a wake by hand cancels the resume.
func TestUsageLimitResumes(t *testing.T) {
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	id, err := a.CreateThread(ctx, "p1", "fresh-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SendPrompt(ctx, id, "work"); err != nil {
		t.Fatal(err)
	}
	s := ag.session(0)
	receivePrompt(t, s.sent)
	if err := a.SendPrompt(ctx, id, "queued"); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	s.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "t", Status: "error", Error: "limit", LimitReset: reset}}
	waitFor(t, func() bool { th, _ := st.Thread(ctx, id); return th.Snoozed() })
	th, _ := st.Thread(ctx, id)
	if !th.SnoozeResume || !th.SnoozedUntil.Equal(reset.Add(time.Minute)) || th.Status != domain.StatusError {
		t.Fatalf("thread after the limit: %+v", th)
	}
	if q, _ := st.QueuedPrompts(ctx, id); len(q) != 1 {
		t.Fatalf("queue = %d", len(q))
	}

	a.wakeDue(ctx, reset.Add(2*time.Minute))
	if got := receivePrompt(t, s.sent); got != limitPrompt {
		t.Fatalf("resumed with %q", got)
	}
	prompts := userItems(t, st, id)
	var m PromptMeta
	json.Unmarshal(prompts[len(prompts)-1].Meta, &m)
	if m.Auto != "limit" {
		t.Fatalf("resume prompt meta = %+v", m)
	}
	// The queue goes on after the resumed turn, without going idle.
	s.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "t", Anchor: "a1", Status: "done"}}
	if got := receivePrompt(t, s.sent); got != "queued" {
		t.Fatalf("after the resume: %q", got)
	}
	finishTurn(t, st, id, s, "a2")

	// Again, but woken by hand: no resume.
	if err := a.SendPrompt(ctx, id, "more"); err != nil {
		t.Fatal(err)
	}
	receivePrompt(t, s.sent)
	s.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "t", Status: "error", Error: "limit", LimitReset: reset}}
	waitFor(t, func() bool { th, _ := st.Thread(ctx, id); return th.Snoozed() })
	if err := a.Wake(ctx, id); err != nil {
		t.Fatal(err)
	}
	if th, _ := st.Thread(ctx, id); th.SnoozeResume {
		t.Fatal("a wake by hand kept the resume")
	}
	a.wakeDue(ctx, reset.Add(2*time.Minute))
	select {
	case got := <-s.sent:
		t.Fatalf("sent %q after a wake by hand", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLimitWake(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	if got := limitWake(now.Add(time.Hour), now); !got.Equal(now.Add(61 * time.Minute)) {
		t.Errorf("an hour out: %v", got)
	}
	if got := limitWake(now.Add(-time.Hour), now); !got.Equal(now.Add(2 * time.Minute)) {
		t.Errorf("already past: %v", got)
	}
}

// TestTurnFiles checks a turn's result line records the files the
// turn's edit calls changed.
func TestTurnFiles(t *testing.T) {
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	id, err := a.CreateThread(ctx, "p1", "fresh-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SendPrompt(ctx, id, "edit it"); err != nil {
		t.Fatal(err)
	}
	s := ag.session(0)
	receivePrompt(t, s.sent)
	in, _ := json.Marshal(map[string]string{"file_path": "/p/a.go", "old_string": "x", "new_string": "y\nz"})
	s.events <- agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: "e1", Name: "Edit", Input: in}}
	s.events <- agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: "e1", Status: "done"}}
	finishTurn(t, st, id, s, "b1")
	a.bg.Wait()
	items, _ := st.Items(ctx, id)
	var tf TurnFiles
	for _, it := range items {
		if it.Kind == domain.KindResult {
			json.Unmarshal(it.Meta, &tf)
		}
	}
	if len(tf.Files) != 1 || tf.Files[0].Path != "/p/a.go" || tf.Files[0].Add != 2 || tf.Files[0].Del != 1 {
		t.Fatalf("turn files = %+v", tf)
	}
}
