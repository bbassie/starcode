package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"starcode/internal/domain"
	"starcode/internal/store"
)

// Threads an agent spawns through the starcode tools are children of the
// thread it runs in. A child that finishes sends its parent a prompt with
// its last reply, unless the parent is in a wait_thread on it already
// (which returns the reply itself). Children that finish close together
// go out as one prompt, so a parent that fanned out work wakes once.

// MaxDepth is how deep spawned threads may nest: a thread the reader
// opened, its children, and theirs.
const MaxDepth = 3

// MaxRunningChildren is how many children of one thread may run at once.
const MaxRunningChildren = 6

// childBatch is how long a finished child waits for its siblings before
// the parent is woken.
var childBatch = 5 * time.Second

// kids is the bookkeeping behind waking parents.
type kids struct {
	mu sync.Mutex
	// waiting counts the wait_thread calls on each child.
	waiting map[string]int
	// done holds, per parent, the children that finished since the last
	// wake, and timer the wake that is due.
	done  map[string][]string
	timer map[string]*time.Timer
}

// SpawnThread opens a thread for parent's agent on projectID and returns
// its id; the caller sends the first prompt. It refuses past MaxDepth and
// MaxRunningChildren.
func (a *App) SpawnThread(ctx context.Context, parentID, projectID, agentName, model string, wake bool) (string, error) {
	parent, err := a.Store.Thread(ctx, parentID)
	if err != nil {
		return "", err
	}
	// level is how deep parent sits: 1 for a thread the reader opened.
	level := 1
	for p := parent; p.Parent != "" && level <= MaxDepth; level++ {
		if p, err = a.Store.Thread(ctx, p.Parent); err != nil {
			break
		}
	}
	if level >= MaxDepth {
		return "", fmt.Errorf("threads spawned by agents nest at most %d deep", MaxDepth)
	}
	running := 0
	for _, c := range a.Children(ctx, parentID) {
		if busyThread(c) {
			running++
		}
	}
	if running >= MaxRunningChildren {
		return "", fmt.Errorf("%d of this thread's children are running already; wait for one to finish", running)
	}
	if projectID == "" {
		projectID = parent.ProjectID
	}
	if _, err := a.Store.Project(ctx, projectID); err != nil {
		return "", fmt.Errorf("project: %w", err)
	}
	if _, ok := a.Agent(agentName); !ok {
		return "", fmt.Errorf("unknown agent %q", agentName)
	}
	id := newID()
	_, err = a.Store.Append(ctx, id, domain.ThreadCreated{ID: id, ProjectID: projectID, Title: "new thread", Agent: agentName, Model: strings.TrimSpace(model), Parent: parentID, WakeParent: wake})
	return id, err
}

// Children lists the threads parentID spawned, newest first.
func (a *App) Children(ctx context.Context, parentID string) []store.Thread {
	ts, _ := a.Store.Threads(ctx)
	var out []store.Thread
	for _, t := range ts {
		if t.Parent == parentID {
			out = append(out, t)
		}
	}
	return out
}

// Settled is whether a thread has nothing left to do without the reader
// or another prompt: no turn running, nothing queued.
func (a *App) Settled(ctx context.Context, t store.Thread) bool {
	if t.Status == domain.StatusRunning {
		return false
	}
	q, _ := a.Store.QueuedPrompts(ctx, t.ID)
	return len(q) == 0
}

// WaitThread blocks until the thread stops running (it finished, failed,
// or waits on the reader for an approval) or ctx ends, and returns it as
// it is then. A parent waiting on its child does not also get woken by it.
func (a *App) WaitThread(ctx context.Context, id string) (store.Thread, error) {
	a.kids.mu.Lock()
	if a.kids.waiting == nil {
		a.kids.waiting = map[string]int{}
	}
	a.kids.waiting[id]++
	a.kids.mu.Unlock()
	defer func() {
		a.kids.mu.Lock()
		if a.kids.waiting[id]--; a.kids.waiting[id] <= 0 {
			delete(a.kids.waiting, id)
		}
		a.kids.mu.Unlock()
	}()
	sub, cancel := context.WithCancel(ctx)
	defer cancel()
	events := a.Bus.Subscribe(sub)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		t, err := a.Store.Thread(ctx, id)
		if err != nil {
			return t, err
		}
		if a.Settled(ctx, t) {
			return t, nil
		}
		select {
		case <-ctx.Done():
			return t, ctx.Err()
		case <-events:
		case <-tick.C:
		}
	}
}

// childDone is the end of a turn on tid with nothing queued after it.
// When tid is a child that wakes its parent, it joins the parent's batch.
func (a *App) childDone(tid string) {
	ctx := context.Background()
	t, err := a.Store.Thread(ctx, tid)
	if err != nil || t.Parent == "" || !t.WakeParent {
		return
	}
	a.kids.mu.Lock()
	defer a.kids.mu.Unlock()
	if a.kids.waiting[tid] > 0 {
		return
	}
	if a.kids.done == nil {
		a.kids.done, a.kids.timer = map[string][]string{}, map[string]*time.Timer{}
	}
	parent := t.Parent
	for _, id := range a.kids.done[parent] {
		if id == tid {
			return
		}
	}
	a.kids.done[parent] = append(a.kids.done[parent], tid)
	if a.kids.timer[parent] == nil {
		a.kids.timer[parent] = time.AfterFunc(childBatch, func() { a.wakeParent(parent) })
	}
}

// wakeParent sends a parent the replies of the children that finished.
func (a *App) wakeParent(parent string) {
	a.kids.mu.Lock()
	ids := a.kids.done[parent]
	delete(a.kids.done, parent)
	delete(a.kids.timer, parent)
	a.kids.mu.Unlock()
	if a.closing.Load() || len(ids) == 0 {
		return
	}
	ctx := context.Background()
	var b strings.Builder
	for _, id := range ids {
		t, err := a.Store.Thread(ctx, id)
		if err != nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "Child thread %q (%s) %s.", t.Title, t.ID, a.ThreadState(t))
		if reply := a.LastReply(ctx, id); reply != "" {
			b.WriteString(" Its last reply:\n\n" + clipRunes(reply, 4000))
		}
	}
	if b.Len() == 0 {
		return
	}
	if err := a.AutoPrompt(ctx, parent, b.String(), "child"); err != nil {
		a.Log.Warn("wake parent", "thread", parent, "err", err)
	}
}

// ThreadState says in words where a thread stands, for an agent.
func (a *App) ThreadState(t store.Thread) string {
	switch t.Status {
	case domain.StatusRunning:
		return "is still working"
	case domain.StatusAwaitingApproval:
		return "is waiting for the user to approve a tool call or answer a question"
	case domain.StatusError:
		if t.StatusDetail != "" {
			return "stopped with an error: " + t.StatusDetail
		}
		return "stopped with an error"
	}
	return "finished"
}

// LastReply is the text of the thread's last agent message.
func (a *App) LastReply(ctx context.Context, id string) string {
	items, err := a.Store.Items(ctx, id)
	if err != nil {
		return ""
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Kind == domain.KindAssistant && itemParent(it) == "" && strings.TrimSpace(it.Body) != "" {
			return strings.TrimSpace(it.Body)
		}
	}
	return ""
}

// itemParent is the subagent an item belongs to, empty for the main
// conversation's.
func itemParent(it store.Item) string {
	var m struct {
		Parent string `json:"parent"`
	}
	json.Unmarshal(it.Meta, &m)
	return m.Parent
}

// AutoPrompt sends text to a thread on starcode's behalf (auto names why,
// for the prompt's meta). A thread that is busy gets it queued, so it goes
// out when the running turn ends; a settled or snoozed one comes back.
func (a *App) AutoPrompt(ctx context.Context, id, text, auto string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("prompt is empty")
	}
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.Archived {
		if _, err := a.Store.Append(ctx, id, domain.ThreadUnarchived{}); err != nil {
			return err
		}
	}
	if t.Snoozed() {
		if _, err := a.Store.Append(ctx, id, domain.ThreadWoken{Auto: true}); err != nil {
			return err
		}
	}
	q, err := a.Store.QueuedPrompts(ctx, id)
	if err != nil {
		return err
	}
	if busyThread(t) {
		_, err := a.Store.Append(ctx, id, domain.PromptQueued{ID: newID(), Body: text})
		return err
	}
	l, err := a.session(ctx, t)
	if err != nil {
		return err
	}
	// A queue left behind by a closed session goes first, as in
	// SendPrompt.
	if len(q) > 0 {
		if _, err := a.Store.Append(ctx, id, domain.PromptQueued{ID: newID(), Body: text}); err != nil {
			return err
		}
		return a.startPromptMeta(ctx, l, t, q[0].ID, q[0].Body, nil)
	}
	return a.startPromptMeta(ctx, l, t, "", text, map[string]any{"auto": auto})
}

// Notify is the agent of thread id asking for the reader: the message
// goes in the transcript as activity, and every open page shows it.
func (a *App) Notify(ctx context.Context, id, text string) {
	text = strings.TrimSpace(text)
	t, err := a.Store.Thread(ctx, id)
	if err != nil || text == "" {
		return
	}
	a.warn(ctx, id, "The agent asks for your attention: "+text)
	a.Bus.Publish(domain.AgentNotified{ThreadID: id, Title: t.Title, Text: text})
}
