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
	child, err := e.app.SpawnThread(ctx, parent, "", "fake", "", false)
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
}
