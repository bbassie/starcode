//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// rowsJS sums up the transcript: the ids found more than once, and each
// row with the items in it when it is a work block.
const rowsJS = `() => {
  const n = {};
  document.querySelectorAll('[id^="it-"]').forEach(e => n[e.id] = (n[e.id] || 0) + 1);
  const rows = [...document.querySelectorAll('#items .items-inner > *')].map(r => r.id +
    (r.classList.contains('work') ? '[' + [...r.querySelectorAll(':scope > .work-body > *')].map(k => k.id).join(',') + ']' : ''));
  return JSON.stringify({twice: Object.keys(n).filter(k => n[k] > 1), rows});
}`

// The stream hands the page each item once, however the page came to
// have it: an append, a morph that drew it from the store first, or the
// first render on a reconnect, which a phone does every time it comes
// back to the tab. Reloads in the middle of turns caught steps drawn
// twice before.
func TestTranscriptShowsEachItemOnce(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	p := e.page("/threads/"+id, true)
	twice := func(p *rod.Page) string {
		return p.MustEval(`() => JSON.parse((` + rowsJS + `)()).twice.join(' ')`).String()
	}
	for turn := 0; turn < 3; turn++ {
		before := len(e.items(id, "result"))
		if err := e.app.SendPrompt(context.Background(), id, "go"); err != nil {
			t.Fatal(err)
		}
		for len(e.items(id, "result")) == before {
			p.MustReload().MustWaitLoad()
			p.MustElement("#sidebar .side-brand")
			time.Sleep(100 * time.Millisecond)
			if s := twice(p); s != "" {
				t.Fatalf("on the page twice: %s", s)
			}
		}
		e.waitIdle(id)
	}

	// One more turn watched live from start to end, then compared with
	// a fresh load of the same transcript.
	before := len(e.items(id, "result"))
	if err := e.app.SendPrompt(context.Background(), id, "go"); err != nil {
		t.Fatal(err)
	}
	for len(e.items(id, "result")) == before {
		if s := twice(p); s != "" {
			t.Fatalf("on the page twice while the turn ran: %s", s)
		}
		time.Sleep(15 * time.Millisecond)
	}
	e.waitIdle(id)
	wait(p, `() => document.querySelectorAll('.item.result').length === 4`)
	live := p.MustEval(rowsJS).String()
	fresh := e.page("/threads/"+id, true).MustEval(rowsJS).String()
	if live != fresh {
		t.Fatalf("the live transcript is not the one a fresh load draws:\nlive  %s\nfresh %s", live, fresh)
	}
}
