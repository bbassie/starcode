//go:build e2e

package e2e

import (
	"context"
	"testing"

	"starcode/internal/domain"
)

// A mermaid block in a finished reply is drawn as a diagram; a click
// shows the source again. One mermaid cannot parse stays as code.
func TestMermaidDiagram(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.thread()
	body := "Here:\n\n```mermaid\ngraph TD\n  A[Start] --> B[Done]\n```\n\nand\n\n```mermaid\nthis is not a diagram ((\n```\n"
	if _, err := e.store.Append(ctx, id, domain.ItemStarted{ID: "m1", Kind: domain.KindAssistant, Status: domain.ItemDone, Body: body}); err != nil {
		t.Fatal(err)
	}
	p := e.page("/threads/"+id, false)
	p.Timeout(30 * 1e9).MustElement(".md .mermaid-diagram svg")
	wait(p, `() => document.querySelectorAll('.md .mermaid-diagram').length === 1`)
	wait(p, `() => document.querySelector('.md .mermaid-diagram').textContent.includes('Start')`)
	wait(p, `() => document.querySelectorAll('.md pre[data-mmd="failed"]:not([hidden])').length === 1`)
	wait(p, `() => document.querySelector('.md .mermaid-diagram').previousElementSibling.hidden`)
	click(p.MustElement(".md .mermaid-diagram"))
	wait(p, `() => !document.querySelector('.md .mermaid-diagram').previousElementSibling.hidden`)
}
