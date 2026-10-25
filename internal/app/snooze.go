package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"starcode/internal/domain"
)

// Snooze parks a thread on the Snoozed shelf until until, when the sweep
// (wakeDue) brings it back on top. A settled thread is unsettled by it;
// pins and the thread's place stay as they are.
func (a *App) Snooze(ctx context.Context, id string, until time.Time) error {
	return a.SnoozeWith(ctx, id, until, "")
}

// SnoozeWith is Snooze with a prompt the thread gets when the snooze runs
// out on time, for an agent that wants to look at something again later.
// Waking it early by hand drops the prompt.
func (a *App) SnoozeWith(ctx context.Context, id string, until time.Time, prompt string) error {
	if !until.After(time.Now()) {
		return errors.New("pick a time in the future")
	}
	if _, err := a.Store.Thread(ctx, id); err != nil {
		return err
	}
	_, err := a.Store.Append(ctx, id, domain.ThreadSnoozed{Until: until, Prompt: strings.TrimSpace(prompt)})
	return err
}

// Wake ends a snooze early.
func (a *App) Wake(ctx context.Context, id string) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if !t.Snoozed() {
		return nil
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadWoken{})
	return err
}

// wakeDue wakes every thread whose snooze ran out by now. The wake counts
// as activity, so the thread comes back on top and marked unread.
func (a *App) wakeDue(ctx context.Context, now time.Time) {
	ts, err := a.Store.Threads(ctx)
	if err != nil {
		a.Log.Warn("wake snoozed threads", "err", err)
		return
	}
	for _, t := range ts {
		if !t.Snoozed() || t.SnoozedUntil.After(now) {
			continue
		}
		if _, err := a.Store.Append(ctx, t.ID, domain.ThreadWoken{Auto: true}); err != nil {
			a.Log.Warn("wake snoozed thread", "thread", t.ID, "err", err)
			continue
		}
		switch {
		case t.SnoozeResume:
			a.resumeAfterLimit(ctx, t.ID)
		case t.SnoozePrompt != "":
			if err := a.AutoPrompt(ctx, t.ID, t.SnoozePrompt, "snooze"); err != nil {
				a.warn(ctx, t.ID, "Could not send the prompt the snooze was holding: "+err.Error())
			}
		}
	}
}

// limitPrompt is what a turn the usage limit stopped is resumed with.
const limitPrompt = "Your plan's usage limit stopped you in the middle of the turn. It has reset now: carry on with the task from where you left off."

// limitWake is when a thread whose turn hit the usage limit comes back:
// a minute after the limit resets, so the reset has happened on the
// provider's side too, and never sooner than two minutes from now, so a
// reset time already past does not wake it into the same limit at once.
func limitWake(reset, now time.Time) time.Time {
	at := reset.Add(time.Minute)
	if soonest := now.Add(2 * time.Minute); at.Before(soonest) {
		at = soonest
	}
	return at
}

// resumeAfterLimit sends the resume prompt to a thread whose turn the
// usage limit stopped, unless something else started a turn meanwhile.
func (a *App) resumeAfterLimit(ctx context.Context, id string) {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	t, err := a.Store.Thread(ctx, id)
	if err != nil || busyThread(t) {
		return
	}
	l, err := a.session(ctx, t)
	if err == nil {
		err = a.startPromptMeta(ctx, l, t, "", limitPrompt, map[string]any{"auto": "limit"})
	}
	if err != nil {
		a.Log.Warn("resume after the usage limit", "thread", id, "err", err)
		a.warn(ctx, id, "Could not resume the turn after the usage limit reset: "+err.Error())
	}
}
