//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/go-rod/rod/lib/input"
)

// "@" in the composer offers the project's files and the other threads;
// Enter puts the marked one where the "@" was.
func TestAtMention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	other := e.thread()
	if err := e.app.RenameThread(ctx, other, "Fix the login page"); err != nil {
		t.Fatal(err)
	}
	id := e.thread()
	p := e.page("/threads/"+id, false)
	ta := p.MustElement("form.composer textarea.prompt")
	ta.MustInput("look at @readm")
	p.MustElementR("form.composer .slash-menu.open .opt", "README.md")
	p.KeyActions().Type(input.Enter).MustDo()
	wait(p, `() => document.querySelector('form.composer textarea.prompt').value === 'look at @README.md '`)

	ta.MustInput("and @login")
	p.MustElementR("form.composer .slash-menu.open .opt", "Fix the login page")
	p.KeyActions().Type(input.Enter).MustDo()
	want := "look at @README.md and [thread: Fix the login page](/threads/" + other + ") "
	wait(p, `(w) => document.querySelector('form.composer textarea.prompt').value === w`, want)
	wait(p, `() => !document.querySelector('form.composer .slash-menu.open')`)
}
