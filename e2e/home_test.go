//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestStartWithoutProject starts a thread from the home page with no
// project: it lands on the thread, which works in a new folder of its
// own under the scratch directory, and the project pickers leave that
// folder out.
func TestStartWithoutProject(t *testing.T) {
	e := newEnv(t)
	p := e.page("/", false)
	wait(p, `() => document.querySelector('.hero-proj')?.offsetParent !== null`)
	click(p.MustElementR(".hero-link", "without a project"))
	wait(p, `() => document.querySelector('.hero-title').textContent.includes('What should we work on?')`)
	p.MustElement("form.composer .prompt").MustInput("Sketch a tiny CLI")
	click(p.MustElement("form.composer button[type=submit]"))
	wait(p, `() => location.pathname.startsWith('/threads/')`)
	id := strings.TrimPrefix(p.MustEval(`() => location.pathname`).String(), "/threads/")

	ctx := context.Background()
	th, err := e.app.Store.Thread(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := e.app.Store.Project(ctx, th.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Scratch || !strings.Contains(pr.Path, "sketch-a-tiny-cli") {
		t.Fatalf("thread project = %+v, want a scratch folder named for the prompt", pr)
	}
	if fi, err := os.Stat(pr.Path); err != nil || !fi.IsDir() {
		t.Fatalf("scratch folder %s: %v", pr.Path, err)
	}
	wait(p, `() => document.querySelector('#mainhead .crumbs').textContent.includes('No project')`)

	// Back home, the menu lists the real project and not the folder.
	p = e.page("/", false)
	click(p.MustElement(".hero-proj"))
	wait(p, `() => getComputedStyle(document.querySelector('.hero-menu')).display !== 'none'`)
	if n := p.MustEval(`() => [...document.querySelectorAll('.hero-menu .opt[data-value]')].filter((o) => o.dataset.value !== '').length`).Int(); n != 1 {
		t.Errorf("project menu lists %d projects, want 1", n)
	}

	// Deleting the thread drops its scratch project; the folder stays.
	if err := e.app.DeleteThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Store.Project(ctx, pr.ID); err == nil {
		t.Error("scratch project outlived its only thread")
	}
	if _, err := os.Stat(pr.Path); err != nil {
		t.Error("deleting the thread removed its folder")
	}
}

// TestNewThreadPicksProject follows the sidebar's new-thread button: the
// home page opens with that thread's project picked.
func TestNewThreadPicksProject(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	p := e.page("/threads/"+id, false)
	click(p.MustElement(`#sidebar a[aria-label="New thread"]`))
	wait(p, `() => location.pathname === '/' && document.querySelector('.hero-proj')?.textContent.trim() === 'proj'`)
}
