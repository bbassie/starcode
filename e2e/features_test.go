//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-rod/rod/lib/input"
)

// TestFindInChanges searches the changed files from the changes tab:
// Ctrl+F lands in the box, the hits list under their file, and a hit
// opens that file's diff at the line with the match marked.
func TestFindInChanges(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	os.WriteFile(filepath.Join(e.dir, "README.md"), []byte("# proj\n\nA needle here.\n"), 0o644)
	os.WriteFile(filepath.Join(e.dir, "new.txt"), []byte("one\nNeedle two\n"), 0o644)
	p := e.page("/threads/"+id, false)
	wait(p, `() => document.getElementById('gitpanel').classList.contains('open')`)
	p.MustElement("#git-files")
	p.MustElement("textarea.prompt").MustFocus()
	p.KeyActions().Press(input.ControlLeft).Type('f').MustDo()
	wait(p, `() => document.activeElement && document.activeElement.id === 'git-find-in'`)
	p.MustElement("#git-find-in").MustInput("needle")
	p.MustElementR(".find-hits .find-file", "README.md")
	p.MustElementR(".find-hits .find-file", "new.txt")
	click(p.MustElementR(".find-hits a", "two"))
	wait(p, `() => { const t = document.getElementById('diff-target'); return t && t.querySelector('mark') && t.textContent.includes('Needle two'); }`)

	// With the panel closed, Ctrl+F is the browser's again.
	click(p.MustElement("#gitpanel .side-head button[aria-label='Close changes panel']"))
	wait(p, `() => !document.getElementById('gitpanel').classList.contains('open')`)
	p.MustElement("textarea.prompt").MustFocus()
	p.KeyActions().Press(input.ControlLeft).Type('f').MustDo()
	if !p.MustEval(`() => document.activeElement.classList.contains('prompt')`).Bool() {
		t.Error("Ctrl+F took the focus with the panel closed")
	}
}

// TestRetitle has the agent write a title from the conversation through
// the rename box's button (the fake agent answers without a model).
func TestRetitle(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	e.turns(id, "tidy the parser")
	p := e.page("/threads/"+id, false)
	click(p.MustElement(".crumb-title"))
	click(p.MustElement(".rename .retitle"))
	p.MustElementR(".crumb-title", "Fake title: tidy the parser")
	th, _ := e.store.Thread(context.Background(), id)
	if th.Title != "Fake title: tidy the parser" {
		t.Errorf("title = %q", th.Title)
	}
}

// TestCompactChip checks the compact nudge's chip: on, a send compacts
// first and the prompt follows the summary; off, sendQuery leaves it out
// and the choice is kept. The nudge only renders for a big conversation
// idle for an hour, so the test puts one on the page itself.
func TestCompactChip(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	e.turns(id, "first")
	p := e.page("/threads/"+id, false)
	p.MustEval(`() => document.querySelector('form.composer').insertAdjacentHTML('afterbegin',
		'<div class="compact-nudge" data-key="keepfull:test"><button type="button" class="chip compact-chip" aria-pressed="true">compact</button></div>')`)
	if q := p.MustEval(`() => sendQuery()`).String(); q != "?compact=1" {
		t.Fatalf("sendQuery with the chip on = %q", q)
	}
	p.MustEval(`() => compactToggle(document.querySelector('.compact-chip'))`)
	if q := p.MustEval(`() => sendQuery() + '|' + localStorage.getItem('keepfull:test')`).String(); q != "|1" {
		t.Fatalf("with the chip off: %q", q)
	}
	p.MustEval(`() => compactToggle(document.querySelector('.compact-chip'))`)

	p.MustElement("textarea.prompt").MustInput("after the summary")
	click(p.MustElement("#composer-actions button[type=submit]"))
	e.until("the compaction, then the prompt", func() bool {
		all, _ := e.store.Items(context.Background(), id)
		notice := -1
		for i, it := range all {
			if notice < 0 && it.Body == "Context was compacted." {
				notice = i
			}
			if notice >= 0 && i > notice && it.Body == "after the summary" {
				return true
			}
		}
		return false
	})
	e.waitIdle(id)
}
