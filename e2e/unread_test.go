//go:build e2e

package e2e

import (
	"context"
	"testing"
)

// A read thread can get its unread mark back, from its sidebar row or
// from its own header menu, which then leaves for the home page.
func TestMarkUnread(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.thread()
	e.turns(a, "hello")
	e.app.MarkSeen(ctx, a)
	b := e.thread()
	p := e.page("/threads/"+b, false)
	row := `#sidebar .thread-row[data-id="` + a + `"]`
	wait(p, `(s) => { const r = document.querySelector(s); return r && !r.classList.contains('unread'); }`, row)
	click(p.MustElement(row + ` .row-act[title="Mark unread"]`))
	wait(p, `(s) => document.querySelector(s).classList.contains('unread')`, row)

	e.app.MarkSeen(ctx, a)
	p = e.page("/threads/"+a, false)
	p.MustEval(`() => document.querySelector('.head-more .btn[title^="Mark unread"]').click()`)
	wait(p, `() => location.pathname === '/'`)
	seen, _ := e.store.Seen(ctx)
	if _, ok := seen[a]; ok {
		t.Errorf("thread still marked seen after mark unread")
	}
}
