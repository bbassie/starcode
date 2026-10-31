//go:build e2e

package e2e

import (
	"context"
	"testing"
)

// Select mode on a phone: tapping rows ticks them instead of opening
// them, settle acts on the ticked ones and leaves select mode, and the
// toast's undo brings them back.
func TestSelectAndSettle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b, c := e.thread(), e.thread(), e.thread()
	e.turns(a, "one")
	e.turns(b, "two")
	e.turns(c, "three")
	p := e.page("/threads/"+c, true)
	p.MustEval(`() => document.querySelector('#sidebar [aria-label="Select threads"]').click()`)
	wait(p, `() => document.getElementById('sidebar').classList.contains('selecting')`)
	for _, id := range []string{a, b} {
		p.MustEval(`(id) => document.querySelector('#row-' + id + ' a').click()`, id)
	}
	wait(p, `(id) => document.querySelectorAll('#sidebar .thread-row.selected').length === 2 && location.pathname.endsWith(id)`, c)
	if got := p.MustElement(".side-bulk-head span").MustText(); got != "2 selected" {
		t.Fatalf("count reads %q", got)
	}
	click(p.MustElementR(".side-bulk-acts button", "settle"))
	p.MustElement("#toast .toast-undo")
	wait(p, `() => !document.getElementById('sidebar').classList.contains('selecting')`)
	for _, id := range []string{a, b} {
		th, _ := e.store.Thread(ctx, id)
		if !th.Archived {
			t.Errorf("%s not settled", id)
		}
	}
	if th, _ := e.store.Thread(ctx, c); th.Archived {
		t.Error("the thread on screen was settled too")
	}
	click(p.MustElement("#toast .toast-undo"))
	e.until("both back", func() bool {
		x, _ := e.store.Thread(ctx, a)
		y, _ := e.store.Thread(ctx, b)
		return !x.Archived && !y.Archived
	})
}
