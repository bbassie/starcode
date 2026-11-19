//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// TestSidebarFilter types in the sidebar's filter box and checks rows
// hide and come back.
func TestSidebarFilter(t *testing.T) {
	e := newEnv(t)
	a, b := e.thread(), e.thread()
	e.turns(a, "alpha work")
	e.turns(b, "beta work")
	p := e.page("/", false)
	p.MustElement("#row-" + a)
	p.MustElement(`#sidebar input[type=search]`).MustInput("beta")
	wait(p, `(a) => getComputedStyle(document.getElementById('row-' + a)).display === 'none'`, a)
	if !p.MustElement("#row-" + b).MustVisible() {
		t.Error("the matching row hid too")
	}
	p.MustElement(`#sidebar input[type=search]`).MustSelectAllText().MustInput("")
	wait(p, `(a) => getComputedStyle(document.getElementById('row-' + a)).display !== 'none'`, a)
}

// TestEarlierItems builds a transcript past the cut, then loads the rest
// with the button, and checks every prompt is on the page in order.
func TestEarlierItems(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	var prompts []string
	for i := 1; i <= 9; i++ {
		prompts = append(prompts, fmt.Sprintf("prompt %d", i))
	}
	e.turns(id, prompts...)
	p := e.page("/threads/"+id, false)
	click(p.MustElement("#load-earlier"))
	wait(p, `() => !document.getElementById('load-earlier') && document.querySelectorAll('.item.user').length === 9`)
	got := p.MustEval(`() => [...document.querySelectorAll('.item.user .bubble')].map((b) => b.textContent).join('|')`).String()
	want := ""
	for i, s := range prompts {
		if i > 0 {
			want += "|"
		}
		want += s
	}
	if got != want {
		t.Errorf("prompts on the page:\n%s\nwant\n%s", got, want)
	}
	// A reconnect keeps the whole transcript: the page's $full rides
	// along, and the redraw does not cut it again.
	reconnect(p)
	wait(p, `() => !document.getElementById('load-earlier') && document.querySelectorAll('.item.user').length === 9`)
}

// TestFollowWaitsForTheFinger checks the transcript does not pull itself
// to the bottom while a finger is on the screen, and follows again after
// the finger lifts.
func TestFollowWaitsForTheFinger(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	var prompts []string
	for i := 1; i <= 4; i++ {
		prompts = append(prompts, fmt.Sprintf("prompt %d", i))
	}
	e.turns(id, prompts...)
	p := e.page("/threads/"+id, true)
	wait(p, `() => { const m = document.getElementById('items'); return m.scrollHeight - m.scrollTop - m.clientHeight < 5; }`)
	// A finger lands and moves the transcript up a little, less than the
	// distance at which it stops following; the stream then adds a row.
	top := p.MustEval(`() => {
		document.dispatchEvent(new TouchEvent('touchstart', { bubbles: true }));
		const m = document.getElementById('items');
		m.scrollTop -= 60;
		m.querySelector('.items-inner').append(Object.assign(document.createElement('div'), { textContent: 'new row' }));
		return m.scrollTop;
	}`).Int()
	time.Sleep(300 * time.Millisecond)
	if got := p.MustEval(`() => document.getElementById('items').scrollTop`).Int(); got != top {
		t.Fatalf("the transcript moved under the finger: %d to %d", top, got)
	}
	p.MustEval(`() => {
		document.dispatchEvent(new TouchEvent('touchend', { bubbles: true }));
		document.querySelector('#items .items-inner').append(Object.assign(document.createElement('div'), { textContent: 'another row' }));
	}`)
	wait(p, `() => { const m = document.getElementById('items'); return m.scrollHeight - m.scrollTop - m.clientHeight < 5; }`)
}

// TestPromptFillsComposer checks the prompt is as wide as the composer
// around it, on a phone and on a desktop, on the home page too.
func TestPromptFillsComposer(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	for _, phone := range []bool{false, true} {
		for _, path := range []string{"/threads/" + id, "/"} {
			p := e.page(path, phone)
			ratio := p.MustEval(`() => {
				const box = document.querySelector('form.composer .prompt-box');
				return document.querySelector('form.composer .prompt').getBoundingClientRect().width / box.getBoundingClientRect().width;
			}`).Num()
			if ratio < 0.99 {
				t.Errorf("%s (phone %v): the prompt is %.0f%% of its box", path, phone, ratio*100)
			}
		}
	}
}

// TestReconnectPill takes the browser offline: the "reconnecting" pill
// shows once the stream has been down a moment, and goes away when the
// stream is back.
func TestReconnectPill(t *testing.T) {
	e := newEnv(t)
	p := e.page("/", false)
	if p.MustEval(`() => getComputedStyle(document.querySelector('.stream-pill')).display`).String() != "none" {
		t.Fatal("pill shows on a connected page")
	}
	proto.NetworkEnable{}.Call(p)
	proto.NetworkEmulateNetworkConditions{Offline: true, Latency: 0, DownloadThroughput: -1, UploadThroughput: -1}.Call(p)
	wait(p, `() => getComputedStyle(document.querySelector('.stream-pill')).display === 'flex'`)
	proto.NetworkEmulateNetworkConditions{Offline: false, Latency: 0, DownloadThroughput: -1, UploadThroughput: -1}.Call(p)
	wait(p, `() => getComputedStyle(document.querySelector('.stream-pill')).display === 'none'`)
}

// TestReopenSettles checks a reopened stream stays open. Opening a stream
// aborts the one before it, and that end used to schedule another reopen,
// so the page reconnected every second from then on. A stream the server
// ends must still come back.
func TestReopenSettles(t *testing.T) {
	e := newEnv(t)
	p := e.page("/", false)
	p.MustEval(`() => {
		window._opens = 0;
		const f = window.fetch;
		window.fetch = (u, o) => { if (String(u).includes('/events')) window._opens++; return f(u, o); };
	}`)
	// A stray reopen comes 1s after the stream it follows (the page's
	// first retry wait), so 2s without one is enough to tell.
	const quiet = 2 * time.Second
	p.MustEval(`() => window.dispatchEvent(new Event('stream'))`)
	time.Sleep(quiet)
	if n := p.MustEval(`() => window._opens`).Int(); n != 1 {
		t.Fatalf("one reopen opened the stream %d times in %v", n, quiet)
	}
	e.srv.CloseClientConnections()
	wait(p, `() => window._opens >= 2`)
	time.Sleep(quiet)
	if n := p.MustEval(`() => window._opens`).Int(); n != 2 {
		t.Fatalf("a stream the server ended came back %d times in %v, want once", n-1, quiet)
	}
}

// TestPRChipsLinkToGitHub links a thread to a pull request and checks
// both chips go to GitHub: the header's is a link in a new tab, and the
// sidebar row's opens one without opening the thread the row links to.
func TestPRChipsLinkToGitHub(t *testing.T) {
	e := newEnv(t)
	a, b := e.thread(), e.thread()
	if err := e.app.LinkPR(context.Background(), a, "o/r", 12, "https://github.com/o/r/pull/12"); err != nil {
		t.Fatal(err)
	}
	p := e.page("/threads/"+b, false)
	head := e.page("/threads/"+a, false).MustElement("#mainhead a.pr-chip-btn")
	if href, target := *head.MustAttribute("href"), *head.MustAttribute("target"); href != "https://github.com/o/r/pull/12" || target != "_blank" {
		t.Errorf("header chip: href %q, target %q", href, target)
	}
	p.MustEval(`() => { window._opened = []; window.open = (u, t) => { window._opened.push(u + ' ' + t); return null; }; }`)
	click(p.MustElement("#row-" + a + " .pr-chip-link"))
	wait(p, `() => window._opened.length === 1`)
	if got := p.MustEval(`() => window._opened[0]`).Str(); got != "https://github.com/o/r/pull/12 _blank" {
		t.Errorf("opened %q", got)
	}
	if path := p.MustEval(`() => location.pathname`).Str(); path != "/threads/"+b {
		t.Errorf("the chip's click opened the thread too: %s", path)
	}
}
