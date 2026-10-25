//go:build e2e

package e2e

import (
	"context"
	"testing"
)

// Send later from the composer: the message leaves the composer, shows
// with the queue, and Settings > Schedules lists it.
func TestSendLater(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	p := e.page("/threads/"+id, false)
	p.MustElement("form.composer .prompt").MustInput("look again tomorrow")
	wait(p, `() => getComputedStyle(document.querySelector('.pick-later')).display !== 'none'`)
	click(p.MustElement(".later-btn"))
	wait(p, `() => getComputedStyle(document.querySelector('.pick-later-menu')).display !== 'none'`)
	click(p.MustElement(".later-opts .btn"))
	e.until("scheduled", func() bool {
		scs, _ := e.store.Schedules(context.Background(), id)
		return len(scs) == 1 && scs[0].Prompt == "look again tomorrow"
	})
	wait(p, `() => document.querySelector('form.composer .prompt').value === '' && !!document.querySelector('#prompt-queue .queue-item.scheduled')`)

	s := e.page("/settings/schedules", false)
	wait(s, `() => document.querySelectorAll('.schedule-card').length === 1`)
	click(s.MustElement(`.schedule-card .btn[title="Run it now"]`))
	e.waitIdle(id)
	wait(s, `() => document.querySelectorAll('.schedule-card').length === 0`)
}

// The Schedules page adds a recurring prompt in the browser's zone.
func TestRecurringScheduleForm(t *testing.T) {
	e := newEnv(t)
	p := e.page("/settings/schedules", false)
	p.MustElement(".schedule-prompt").MustInput("weekly tidy-up")
	p.MustElement("#sched-title").MustInput("Tidy")
	click(p.MustElement(`.schedule-form button[type="submit"]`))
	e.until("added", func() bool {
		scs, _ := e.store.Schedules(context.Background(), "")
		return len(scs) == 1 && scs[0].Cron == "0 9 * * 1-5" && scs[0].TZ != "" && scs[0].ProjectID == e.project
	})
	wait(p, `() => document.querySelectorAll('.schedule-card').length === 1 && document.querySelector('.schedule-prompt').value === ''`)
}
