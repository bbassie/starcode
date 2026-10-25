//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"starcode/internal/app"
	"starcode/internal/domain"
)

// While a turn waits on an approval, queued prompts can be edited in
// place and moved.
func TestQueueEditAndMove(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.thread()
	// The default mode asks before its Bash call, so the turn stays put.
	if err := e.app.SetThreadSettings(ctx, id, app.ThreadSettings{Agent: "fake"}); err != nil {
		t.Fatal(err)
	}
	e.app.SendPrompt(ctx, id, "start")
	e.until("waiting", func() bool {
		th, _ := e.store.Thread(ctx, id)
		return th.Status == domain.StatusAwaitingApproval
	})
	for _, p := range []string{"one", "two"} {
		e.app.SendPrompt(ctx, id, p)
	}
	order := func() string {
		q, _ := e.store.QueuedPrompts(ctx, id)
		var s []string
		for _, x := range q {
			s = append(s, x.Body)
		}
		return strings.Join(s, ",")
	}
	p := e.page("/threads/"+id, false)
	wait(p, `() => document.querySelectorAll('#prompt-queue .queue-item').length === 2`)
	click(p.MustElement(`#prompt-queue .queue-item:nth-child(2) .btn[title="Move up"]`))
	e.until("moved", func() bool { return order() == "two,one" })
	wait(p, `() => document.querySelector('#prompt-queue .queue-item .ellipsis').textContent === 'two'`)
	click(p.MustElement(`#prompt-queue .queue-item .btn[title="Edit"]`))
	ta := p.MustElement(`#prompt-queue .queue-item.editing textarea`)
	ta.MustSelectAllText().MustInput("TWO")
	click(p.MustElement(`#prompt-queue .queue-item.editing .queue-edit .btn`))
	e.until("edited", func() bool { return order() == "TWO,one" })
	wait(p, `() => !document.querySelector('#prompt-queue .queue-item.editing') && document.querySelector('#prompt-queue .queue-item .ellipsis').textContent === 'TWO'`)
}
