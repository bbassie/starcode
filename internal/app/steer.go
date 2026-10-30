package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"starcode/internal/agent"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// SteerPrompt sends text into the running turn: the agent reads it at its
// next step, after the tool call that runs now, and the turn goes on. It
// waits above the composer until the agent takes it, and goes into the
// transcript there. An agent that cannot take it now (no steering, a
// compaction running) leaves it in the queue for after the turn. On an
// idle thread it is an ordinary send.
func (a *App) SteerPrompt(ctx context.Context, threadID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("prompt is empty")
	}
	a.promptMu.Lock()
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		a.promptMu.Unlock()
		return err
	}
	if !busyThread(t) {
		a.promptMu.Unlock()
		return a.SendPrompt(ctx, threadID, text)
	}
	defer a.promptMu.Unlock()
	id := newID()
	if _, err := a.Store.Append(ctx, threadID, domain.PromptQueued{ID: id, Body: text}); err != nil {
		return err
	}
	a.Store.SaveDraft(ctx, threadID, "")
	return a.steerLocked(ctx, threadID, id, text)
}

// SteerQueued sends a queued prompt now. Into the running turn when the
// agent can take it there; otherwise the prompt moves to the front and
// the turn stops, so it is the next thing the agent reads. A thread with
// no turn running starts it.
func (a *App) SteerQueued(ctx context.Context, threadID, promptID string) error {
	a.promptMu.Lock()
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		a.promptMu.Unlock()
		return err
	}
	queued, err := a.Store.QueuedPrompts(ctx, threadID)
	if err != nil {
		a.promptMu.Unlock()
		return err
	}
	var q *store.QueuedPrompt
	for i := range queued {
		if queued[i].ID == promptID {
			q = &queued[i]
		}
	}
	if q == nil {
		a.promptMu.Unlock()
		return errors.New("queued prompt not found")
	}
	if !busyThread(t) {
		defer a.promptMu.Unlock()
		l, err := a.session(ctx, t)
		if err != nil {
			return err
		}
		return a.startPromptLocked(ctx, l, t, q.ID, q.Body)
	}
	if a.canSteer(threadID) {
		defer a.promptMu.Unlock()
		return a.steerLocked(ctx, threadID, q.ID, q.Body)
	}
	err = a.moveQueuedLocked(ctx, threadID, promptID, -1<<30)
	a.promptMu.Unlock()
	if err != nil {
		return err
	}
	return a.Interrupt(ctx, threadID)
}

// canSteer is whether the thread's session takes messages mid-turn.
func (a *App) canSteer(threadID string) bool {
	a.mu.Lock()
	l := a.sessions[threadID]
	a.mu.Unlock()
	if l == nil {
		return false
	}
	_, ok := l.sess.(agent.Steerer)
	return ok
}

// steerLocked hands queued prompt id to the running turn and marks it
// steered. A session that cannot take it leaves it queued, which is not
// an error: it goes out after the turn like any other. promptMu must be
// held, so the agent's Steered cannot be handled before PromptSteered is
// in the log.
func (a *App) steerLocked(ctx context.Context, threadID, id, text string) error {
	a.mu.Lock()
	l := a.sessions[threadID]
	a.mu.Unlock()
	if l == nil {
		return nil
	}
	st, ok := l.sess.(agent.Steerer)
	if !ok {
		return nil
	}
	if err := st.Steer(ctx, id, text); err != nil {
		a.Log.Debug("steer: left in the queue", "thread", threadID, "err", err)
		return nil
	}
	_, err := a.Store.Append(ctx, threadID, domain.PromptSteered{ID: id})
	return err
}

// steered puts a steered prompt the agent took into the transcript, at
// this point of the turn, and out of the queue. The reply it interrupts
// is closed, so what the agent says next is a new item under it.
func (a *App) steered(ctx context.Context, l *live, id string) []any {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	steering, err := a.Store.SteeredPrompts(ctx, l.threadID)
	if err != nil {
		return nil
	}
	for _, q := range steering {
		if q.ID != id {
			continue
		}
		meta, _ := json.Marshal(map[string]any{"steered": true})
		evs := a.closeMessages(l, "", "")
		return append(evs, domain.PromptDequeued{ID: id},
			domain.ItemStarted{ID: newID(), Kind: domain.KindUser, Body: q.Body, Status: domain.ItemDone, Meta: meta})
	}
	return nil
}

// dropSteered puts the thread's steered prompts back in the queue, for
// when the session that was to deliver them is gone.
func (a *App) dropSteered(ctx context.Context, threadID string) []any {
	steering, _ := a.Store.SteeredPrompts(ctx, threadID)
	var evs []any
	for _, q := range steering {
		evs = append(evs, domain.PromptSteerDropped{ID: q.ID})
	}
	return evs
}

// steerGrace is how long a turn's end waits for the session to start the
// turn it owes a steered prompt it did not deliver in time (see
// agent.Steerer). Claude starts it within milliseconds; past this the
// prompts go back to the queue and the queue moves on.
var steerGrace = 10 * time.Second

// watchSteered runs after a turn that ended with steered prompts still
// waiting. If no turn has started for them after steerGrace, they go back
// in the queue and the first queued prompt starts, as at any turn's end.
func (a *App) watchSteered(l *live, turnID string) {
	time.Sleep(steerGrace)
	ctx := context.Background()
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	l.mu.Lock()
	moved := l.turnID != turnID
	l.mu.Unlock()
	a.mu.Lock()
	current := a.sessions[l.threadID] == l
	a.mu.Unlock()
	if moved || !current {
		return
	}
	evs := a.dropSteered(ctx, l.threadID)
	if len(evs) == 0 {
		return
	}
	a.Log.Warn("steered prompts not taken; back to the queue", "thread", l.threadID, "n", len(evs))
	if _, err := a.Store.Append(ctx, l.threadID, evs...); err != nil {
		return
	}
	queued, _ := a.Store.QueuedPrompts(ctx, l.threadID)
	t, err := a.Store.Thread(ctx, l.threadID)
	if err != nil || len(queued) == 0 {
		return
	}
	if err := a.startPromptLocked(ctx, l, t, queued[0].ID, queued[0].Body); err != nil {
		a.Log.Error("start queued prompt", "err", err)
	}
}

// StopAndSend sends text now: on a busy thread it goes to the front of
// the queue and the running turn is stopped, so it is the next thing the
// agent reads; on an idle one it is an ordinary send.
func (a *App) StopAndSend(ctx context.Context, threadID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("prompt is empty")
	}
	a.promptMu.Lock()
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		a.promptMu.Unlock()
		return err
	}
	if !busyThread(t) {
		a.promptMu.Unlock()
		return a.SendPrompt(ctx, threadID, text)
	}
	id := newID()
	if _, err := a.Store.Append(ctx, threadID, domain.PromptQueued{ID: id, Body: text}); err != nil {
		a.promptMu.Unlock()
		return err
	}
	a.Store.SaveDraft(ctx, threadID, "")
	err = a.moveQueuedLocked(ctx, threadID, id, -1<<30)
	a.promptMu.Unlock()
	if err != nil {
		return err
	}
	return a.Interrupt(ctx, threadID)
}
