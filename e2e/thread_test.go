//go:build e2e

package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/input"
)

func TestSendAndRewind(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	p := e.page("/threads/"+id, false)
	for _, text := range []string{"first prompt", "second prompt", "third prompt"} {
		before := len(e.items(id, "result"))
		p.MustElement("form.composer .prompt").MustInput(text)
		p.KeyActions().Type(input.Enter).MustDo()
		e.until("the turn", func() bool { return len(e.items(id, "result")) > before })
		e.waitIdle(id)
	}
	wait(p, `() => document.querySelectorAll('.item.user').length === 3`)
	if n := len(p.MustElements(`.item.user [aria-label="Rewind to before this message"]`)); n != 3 {
		t.Fatalf("rewind buttons = %d, want 3", n)
	}
	if ts := p.MustElement(".item.user .msg-time").MustText(); ts == "" {
		t.Error("the prompt has no time")
	}

	click(p.MustElements(`.item.user [aria-label="Rewind to before this message"]`)[1])
	wait(p, `() => document.querySelectorAll('.item.user').length === 1`)
	wait(p, `() => document.querySelector('form.composer .prompt').value === 'second prompt'`)
	if n := len(e.items(id, "user")); n != 1 {
		t.Fatalf("prompts in the store after the rewind = %d", n)
	}
	// The next turn forks the fake conversation at the first turn's end.
	before := len(e.items(id, "result"))
	p.KeyActions().Type(input.Enter).MustDo()
	e.until("the turn after the rewind", func() bool { return len(e.items(id, "result")) > before })
	e.waitIdle(id)
	wait(p, `() => document.querySelectorAll('.item.user').length === 2`)
}

func TestSettleUndo(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	e.turns(id, "hello")
	p := e.page("/threads/"+id, false)
	settle := func() {
		click(p.MustElement("#mainhead .more-toggle"))
		click(p.MustElementR(".head-more button", "^\\s*settle"))
		p.MustElement("#toast .toast-undo")
		wait(p, `() => !!document.querySelector('#mainhead .status-pill[title^="Settled"]')`)
	}
	settle()
	// Ctrl+Z outside a text box takes the settle back.
	p.MustElement("#items").MustClick()
	p.KeyActions().Press(input.ControlLeft).Type('z').MustDo()
	wait(p, `() => !document.querySelector('#mainhead .status-pill[title^="Settled"]')`)
	wait(p, `() => !document.querySelector('#toast .toast-undo')`)

	// In the composer Ctrl+Z is the text box's own undo.
	settle()
	p.MustElement("form.composer .prompt").MustInput("abc")
	p.KeyActions().Press(input.ControlLeft).Type('z').MustDo()
	if !p.MustHas(`#mainhead .status-pill[title^="Settled"]`) {
		t.Fatal("Ctrl+Z in the composer took the settle back")
	}
	click(p.MustElement("#toast .toast-undo"))
	wait(p, `() => !document.querySelector('#mainhead .status-pill[title^="Settled"]')`)
}

func TestSnoozeAndWake(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	e.turns(id, "hello")
	p := e.page("/threads/"+id, false)
	click(p.MustElement("#mainhead .more-toggle"))
	click(p.MustElementR(".snooze-opts button", "in 3 hours"))
	p.MustElement("#toast .toast-undo")
	wait(p, `() => document.querySelectorAll('#snoozed .thread-row').length === 1`)
	if p.MustHas("#sidebar .threads.inbox #row-" + id) {
		t.Error("a snoozed thread is still in the list")
	}
	pill := p.MustElement("#mainhead .status-pill time")
	if txt := pill.MustText(); txt == "" || strings.Contains(txt, "UTC") {
		t.Errorf("the pill's time is not in the reader's zone: %q", txt)
	}
	click(p.MustElement("#mainhead button.status-pill:has(time)"))
	wait(p, `() => !document.querySelector('#snoozed')`)
	p.MustElement("#sidebar .threads.inbox #row-" + id)

	// The row's own snooze button, then the toast's undo.
	click(p.MustElement("#row-" + id + ` [aria-label="Snooze until tomorrow"]`))
	wait(p, `() => document.querySelectorAll('#snoozed .thread-row').length === 1`)
	click(p.MustElement("#toast .toast-undo"))
	wait(p, `() => !document.querySelector('#snoozed')`)
}

func TestStash(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	p := e.page("/threads/"+id, false)
	if p.MustElement(".pick-stash").MustVisible() {
		t.Error("the stash button shows with nothing to stash")
	}
	p.MustElement("form.composer .prompt").MustInput("a prompt for later")
	click(p.MustElement("form.composer .stash-btn"))
	wait(p, `() => document.querySelector('form.composer .prompt').value === ''`)
	p.MustElement("#toast .toast-undo")
	click(p.MustElement("form.composer .stash-btn"))
	row := p.MustElement(".stash-row .opt")
	click(row)
	wait(p, `() => document.querySelector('form.composer .prompt').value === 'a prompt for later'`)

	// The hotkey stashes from inside the composer.
	p.MustElement("form.composer .prompt").MustFocus()
	p.KeyActions().Press(input.ControlLeft).Type('s').MustDo()
	wait(p, `() => document.querySelector('form.composer .prompt').value === ''`)
}

func TestQuoteAndBigPaste(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	e.turns(id, "hello")
	p := e.page("/threads/"+id, false)
	p.MustElement(".item.assistant").MustEval(`() => {
		const r = document.createRange();
		r.selectNodeContents(this.querySelector('p') || this);
		getSelection().removeAllRanges();
		getSelection().addRange(r);
	}`)
	wait(p, `() => { const b = document.querySelector('.quote-btn'); return b && !b.hidden; }`)
	click(p.MustElement(".quote-btn"))
	if v := value(p, "form.composer .prompt"); !strings.HasPrefix(v, "> ") {
		t.Errorf("composer after the quote = %q", v)
	}

	p.MustElement("form.composer .prompt").MustEval(`() => { this.value = ''; this.dispatchEvent(new Event('input', { bubbles: true })); }`)
	p.MustElement("form.composer .prompt").MustEval(`() => {
		const dt = new DataTransfer();
		dt.setData('text/plain', 'x'.repeat(40000));
		this.focus();
		this.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
	}`)
	wait(p, `() => document.querySelectorAll('.attach-thumbs .attach-item').length === 1`)
	if v := value(p, "form.composer .prompt"); v != "" {
		t.Errorf("the big paste went into the composer too (%d characters)", len(v))
	}
}

func TestFilePreview(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	os.WriteFile(filepath.Join(e.dir, "page.html"), []byte(`<h1>Hello preview</h1><script>document.title = "ran"</script>`), 0o644)
	p := e.page("/threads/"+id, false)
	click(p.MustElementR(".panel-tab", "files"))
	click(p.MustElementR("#file-explorer a", "page.html"))
	frame := p.MustElement("iframe.file-preview")
	if sb := frame.MustAttribute("sandbox"); sb == nil || *sb != "" {
		t.Errorf("the page preview is not sandboxed: %v", sb)
	}
	src := *frame.MustAttribute("src")
	res, err := http.Get(e.url + src)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !strings.HasPrefix(res.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("the page is served without the sandbox policy")
	}
	h1 := frame.MustFrame().MustElement("h1").MustText()
	if h1 != "Hello preview" {
		t.Errorf("preview heading = %q", h1)
	}
}

func TestPaletteAndPhone(t *testing.T) {
	e := newEnv(t)
	id := e.thread()
	e.turns(id, "hello")
	p := e.page("/threads/"+id, false)
	p.MustElement("#items").MustClick()
	p.KeyActions().Press(input.ControlLeft).Type('k').MustDo()
	p.MustElement("#palette input").MustInput("snooze")
	p.MustElementR("#palette-results .opt", "Snooze thread")

	phone := e.page("/threads/"+id, true)
	// The chips stay on one row, and a toast lands under the header.
	if h := phone.MustElement("form.composer .chips").MustEval(`() => this.offsetHeight`).Int(); h > 60 {
		t.Errorf("the composer's chips wrap on a phone: %dpx high", h)
	}
	click(phone.MustElement("#mainhead .more-toggle"))
	click(phone.MustElementR(".head-more button", "^\\s*pin"))
	click(phone.MustElement("#mainhead .more-toggle"))
	click(phone.MustElementR(".head-more button", "unpin"))
	toast := phone.MustElement("#toast .toast-undo").MustEval(`() => this.closest('.toast').getBoundingClientRect().top`).Int()
	if toast > 200 {
		t.Errorf("the toast sits %dpx down on a phone, over the composer", toast)
	}
}
