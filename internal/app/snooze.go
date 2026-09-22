package app

import (
	"context"
	"errors"
	"time"

	"starcode/internal/domain"
)

// Snooze parks a thread on the Snoozed shelf until until, when the sweep
// (wakeDue) brings it back on top. A settled thread is unsettled by it;
// pins and the thread's place stay as they are.
func (a *App) Snooze(ctx context.Context, id string, until time.Time) error {
	if !until.After(time.Now()) {
		return errors.New("pick a time in the future")
	}
	if _, err := a.Store.Thread(ctx, id); err != nil {
		return err
	}
	_, err := a.Store.Append(ctx, id, domain.ThreadSnoozed{Until: until})
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
		}
	}
}
