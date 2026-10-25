//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// TestEditorSaves opens a file from the tree, edits it and saves with
// Ctrl+S, which writes the thread's checkout.
func TestEditorSaves(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	os.WriteFile(filepath.Join(e.dir, "notes.txt"), []byte("one\n"), 0o644)
	p := e.page("/threads/"+id, false)
	click(p.MustElementR(".panel-tab", "files"))
	click(p.MustElementR("#file-explorer a", "notes.txt"))
	ta := p.MustElement(".git-editor textarea.file-editor")
	wait(p, `() => document.querySelector('.git-editor textarea').value === 'one\n'`)
	ta.MustEval(`() => { this.focus(); this.setSelectionRange(this.value.length, this.value.length); }`)
	ta.MustInput("two\n")
	p.MustElementR(".editor-state", "unsaved")
	p.KeyActions().Press(input.ControlLeft).Type('s').MustDo()
	e.until("the file on disk", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.dir, "notes.txt"))
		return string(b) == "one\ntwo\n"
	})
	// The file now shows in the changes list.
	click(p.MustElementR(".panel-tab", "changes"))
	p.MustElementR("#git-files a", "notes.txt")
}

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
	p.MustEval(`() => window.dispatchEvent(new Event('stream'))`)
	time.Sleep(time.Second)
	wait(p, `() => !document.getElementById('load-earlier') && document.querySelectorAll('.item.user').length === 9`)
}

// TestPanelSurvivesReconnect opens a diff in the panel, forces the page's
// stream to reconnect, and checks the diff is still there: the stream
// reads the page's gitPath on connect and draws the detail again.
func TestPanelSurvivesReconnect(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	os.WriteFile(filepath.Join(e.dir, "README.md"), []byte("# proj\nchanged\n"), 0o644)
	p := e.page("/threads/"+id, false)
	click(p.MustElementR("#git-files a", "README.md"))
	p.MustElementR("#git-detail .diff span.add", "changed")
	p.MustEval(`() => window.dispatchEvent(new Event('stream'))`)
	time.Sleep(time.Second)
	wait(p, `() => [...document.querySelectorAll('#git-detail .diff span.add')].some((s) => s.textContent.includes('changed'))`)
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
	p.MustEval(`() => window.dispatchEvent(new Event('stream'))`)
	time.Sleep(3 * time.Second)
	if n := p.MustEval(`() => window._opens`).Int(); n != 1 {
		t.Fatalf("one reopen opened the stream %d times in 3s", n)
	}
	e.srv.CloseClientConnections()
	wait(p, `() => window._opens >= 2`)
	time.Sleep(3 * time.Second)
	if n := p.MustEval(`() => window._opens`).Int(); n != 2 {
		t.Fatalf("a stream the server ended came back %d times in 3s, want once", n-1)
	}
}

// TestPhonePanelHeader opens the changes panel on a phone with a long
// branch name. The panel fills the screen, close stays inside it and the
// branch sits on a row of its own below the tabs.
func TestPhonePanelHeader(t *testing.T) {
	e := newEnv(t)
	cmd := exec.Command("git", "checkout", "--quiet", "-b", "feature/a-branch-name-too-long-for-one-row")
	cmd.Dir = e.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v\n%s", err, out)
	}
	id := e.thread()
	p := e.page("/threads/"+id, true)
	click(p.MustElement(".gittoggle"))
	p.MustElement("#git-head .git-branch")
	wait(p, `() => {
		const panel = document.getElementById('gitpanel').getBoundingClientRect();
		if (Math.round(panel.left) !== 0 || Math.round(panel.width) !== innerWidth) return false;
		const head = document.getElementById('git-head');
		const close = head.querySelector('[aria-label="Close changes panel"]').getBoundingClientRect();
		const tab = head.querySelector('.panel-tab').getBoundingClientRect();
		const branch = head.querySelector('.git-branch').getBoundingClientRect();
		return close.right <= innerWidth && Math.abs(close.top - tab.top) < tab.height
			&& branch.top >= tab.bottom && branch.height < 30 && branch.right <= innerWidth;
	}`)
}
