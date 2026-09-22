//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/input"
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
