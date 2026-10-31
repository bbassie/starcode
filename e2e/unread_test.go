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

// A thread opened after more came in shows a line above the first new
// row and opens there; a reload, with everything seen, has no line.
func TestNewSinceLastLooked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.thread()
	e.turns(a, "first", "second", "third", "4", "5", "6", "7", "8")
	e.app.MarkSeen(ctx, a)
	e.turns(a, "ninth", "10")
	p := e.page("/threads/"+a, false)
	// The page opens at the line, not at the bottom.
	wait(p, `() => {
		const line = document.getElementById('new-mark');
		const row = line && line.nextElementSibling;
		if (!row || !row.textContent.includes('ninth')) return false;
		const box = document.getElementById('items').getBoundingClientRect(), at = line.getBoundingClientRect();
		return at.top >= box.top && at.top < box.top + 60 && document.body.classList.contains('scrolled-up');
	}`)
	p = e.page("/threads/"+a, false)
	p.MustElement(".turn")
	if p.MustEval(`() => !!document.getElementById('new-mark')`).Bool() {
		t.Error("the line is back on a thread with nothing new")
	}
}
