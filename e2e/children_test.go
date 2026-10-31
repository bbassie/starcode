//go:build e2e

package e2e

import (
	"context"
	"testing"

	"starcode/internal/app"
)

// A thread an agent spawned hangs under its parent in the sidebar, and
// its header leads back to the parent.
func TestSpawnedThreadNestsUnderItsParent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	parent := e.thread()
	e.turns(parent, "parent work")
	child, err := e.app.SpawnThread(ctx, parent, "", "fake", "", app.ReportNone)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.app.SetThreadSettings(ctx, child, app.ThreadSettings{Agent: "fake", PermissionMode: "yolo"}); err != nil {
		t.Fatal(err)
	}
	e.turns(child, "child work")

	p := e.page("/threads/"+child, false)
	wait(p, `() => {
		const rows = [...document.querySelectorAll('#sidebar .thread-row')].map(r => r.dataset.id);
		const i = rows.indexOf('`+parent+`');
		return i >= 0 && rows[i+1] === '`+child+`' && document.getElementById('row-`+child+`').classList.contains('child');
	}`)
	wait(p, `() => document.querySelector('#mainhead .crumb-parent')?.getAttribute('href') === '/threads/`+parent+`'`)

	// On a phone the parent's title has no room: an arrow leads up to it.
	ph := e.page("/threads/"+child, true)
	wait(ph, `() => { const a = document.querySelector('#mainhead .crumb-up'); return a && a.offsetWidth > 0 && a.getAttribute('href') === '/threads/`+parent+`'; }`)
	shot(ph, "child-phone.png")

	// A message the child's agent sends shows where it came from, as a
	// link to the child under its title.
	if err := e.app.SetThreadSettings(ctx, parent, app.ThreadSettings{Agent: "fake", PermissionMode: "yolo"}); err != nil {
		t.Fatal(err)
	}
	if err := e.app.RenameThread(ctx, child, "Child work"); err != nil {
		t.Fatal(err)
	}
	if err := e.app.AutoPrompt(ctx, parent, "Message from thread \"Child work\" ("+child+"):\n\nall done", "agent"); err != nil {
		t.Fatal(err)
	}
	pp := e.page("/threads/"+parent, true)
	// It folds to one line that says who sent it and how it starts, and
	// opens to the message with the link.
	wait(pp, `() => {
		const d = [...document.querySelectorAll('details.item.auto-prompt')].pop();
		return d && !d.open && d.querySelector('summary').textContent.replace(/\s+/g, ' ').includes('Message from Child work all done');
	}`)
	click(pp.MustElement("details.item.auto-prompt:last-of-type > summary"))
	wait(pp, `() => [...document.querySelectorAll('.user-from a')].some(a => a.offsetWidth > 0 && a.getAttribute('href') === '/threads/`+child+`' && a.textContent === 'Child work')`)
	shot(pp, "parent-phone.png")
}
