//go:build e2e

package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"starcode/internal/app"
	"starcode/internal/domain"
)

// Work the agent leaves running after its turn shows over the composer
// and as a slow dashed glyph in the sidebar, until it ends. The prompt
// box keeps the height of what is typed in it while the composer is
// redrawn around it.
func TestBackgroundWorkShows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.thread()
	e.turns(id, "hi")
	p := e.page("/threads/"+id, true)
	p.MustSetViewport(390, 844, 3, true)

	box := p.MustElement(`form.composer .prompt`)
	box.MustInput("one\ntwo\nthree\nfour")
	h := box.MustEval(`() => this.offsetHeight`).Int()
	if h < 60 {
		t.Fatalf("four lines are %dpx high", h)
	}
	// Typing on never takes the box down to its first line on the way
	// (autoGrow measured from height auto on every keystroke, which iOS
	// showed for a moment), and a composer redraw keeps its height.
	box.MustEval(`() => { window._heights = []; new MutationObserver(ms => ms.forEach(m => window._heights.push(m.oldValue))).observe(this, {attributes: true, attributeOldValue: true, attributeFilter: ['style']}) }`)
	box.MustInput("\nfive")
	wait(p, `() => window._heights.length > 0`)
	if hs := box.MustEval(`() => window._heights.join(' ')`).String(); strings.Contains(hs, "auto") {
		t.Fatalf("typing set the prompt box's height to %s", hs)
	}
	h = box.MustEval(`() => this.offsetHeight`).Int()
	// The settings change redraws the whole composer (see renderHead).
	// The mark is not in the server's markup, so the morph drops it.
	p.MustEval(`() => document.querySelector('#composer').dataset.mark = '1'`)
	if err := e.app.SetThreadSettings(ctx, id, app.ThreadSettings{Agent: "fake"}); err != nil {
		t.Fatal(err)
	}
	wait(p, `() => !document.querySelector('#composer').dataset.mark`)
	if got := box.MustEval(`() => this.offsetHeight`).Int(); got != h {
		t.Fatalf("the prompt box went from %dpx to %dpx on a composer redraw", h, got)
	}
	// A height set from outside autoGrow is put back before a paint.
	box.MustEval(`() => { this.style.height = '20px' }`)
	wait(p, `() => document.querySelector('form.composer .prompt').offsetHeight === `+strconv.Itoa(h))

	if _, err := e.store.Append(ctx, id, domain.ThreadBackgroundStarted{TaskID: "b1", Kind: "command", Label: "cd /home/x/proj && go test -tags e2e ./e2e/..."}); err != nil {
		t.Fatal(err)
	}
	wait(p, `() => document.querySelector('#prompt-queue .queue-item.background .ellipsis')?.textContent === 'go test -tags e2e ./e2e/...'`)
	wait(p, `(id) => !!document.querySelector('#row-' + id + ' .glyph.st-background')`, id)
	shot(p, "background-phone.png")

	if _, err := e.store.Append(ctx, id, domain.ThreadBackgroundEnded{TaskID: "b1", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	wait(p, `() => document.querySelector('#prompt-queue').classList.contains('empty')`)
	wait(p, `(id) => !!document.querySelector('#row-' + id + ' .glyph.st-idle')`, id)
}

// A link to another site opens in a new tab wherever it is on the page,
// so the home screen app is never replaced by that site; a link within
// starcode stays in the tab.
func TestExternalLinksOpenNewTab(t *testing.T) {
	e := newEnv(t)
	p := e.page("/", true)
	got := p.MustEval(`() => {
		const out = [];
		for (const href of ['https://example.com/x', '/settings']) {
			const a = Object.assign(document.createElement('a'), { href, textContent: 'x' });
			document.body.append(a);
			// Stop the navigation after the page's handler has run.
			a.addEventListener('click', (ev) => ev.preventDefault());
			a.click();
			out.push(a.target || '-');
			a.remove();
		}
		return out.join(' ');
	}`).String()
	if got != "_blank -" {
		t.Fatalf("targets after a click: %q, want \"_blank -\"", got)
	}
}
