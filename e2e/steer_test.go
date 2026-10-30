//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod"

	"starcode/internal/app"
	"starcode/internal/domain"
)

// shot saves a screenshot of p as name when STARCODE_E2E_SHOTS names a
// directory.
func shot(p *rod.Page, name string) {
	if dir := os.Getenv("STARCODE_E2E_SHOTS"); dir != "" {
		p.MustScreenshot(filepath.Join(dir, name))
	}
}

// On a phone, a message sent while the turn runs goes into it by default:
// it waits above the composer until the agent's next step, then shows in
// the transcript there and the turn goes on. A tap on the mode chip
// queues one message instead, and "now" on a queued message steers it.
func TestSteer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.thread()
	// The default mode asks before its Bash call, so the turn waits there.
	if err := e.app.SetThreadSettings(ctx, id, app.ThreadSettings{Agent: "fake"}); err != nil {
		t.Fatal(err)
	}
	p := e.page("/threads/"+id, true)
	p.MustSetViewport(390, 844, 3, true)
	e.app.SendPrompt(ctx, id, "start")
	e.until("waiting", func() bool {
		th, _ := e.store.Thread(ctx, id)
		return th.Status == domain.StatusAwaitingApproval
	})
	wait(p, `() => document.querySelector('#composer-actions .busy-mode')?.dataset.mode === 'steer'`)

	send := func(text string) {
		p.MustElement(`form.composer .prompt`).MustInput(text)
		click(p.MustElement(`#composer-actions .btn.primary`))
		wait(p, `() => document.querySelector('form.composer .prompt').value === ''`)
	}
	send("check the docs too")
	wait(p, `() => document.querySelector('#prompt-queue .queue-item.steering')?.textContent.includes('check the docs too')`)

	// One message queued with the chip; it flips back after the send.
	click(p.MustElement(`#composer-actions .busy-mode`))
	wait(p, `() => document.querySelector('#composer-actions .busy-mode').dataset.mode === 'queue'`)
	p.MustElement(`form.composer .prompt`).MustInput("then write a summary")
	shot(p, "steer-composer.png")
	click(p.MustElement(`#composer-actions .btn.primary`))
	wait(p, `() => [...document.querySelectorAll('#prompt-queue .queue-item:not(.steering) .ellipsis')].some(el => el.textContent === 'then write a summary')`)
	wait(p, `() => document.querySelector('#composer-actions .busy-mode').dataset.mode === 'steer'`)
	send("and one more")
	click(p.MustElement(`#composer-actions .busy-mode`))
	send("queued, then now")
	wait(p, `() => document.querySelectorAll('#prompt-queue .queue-item:not(.steering)').length === 2`)
	shot(p, "steer-queue.png")

	// "now" on the second queued message steers it.
	click(p.MustElement(`#prompt-queue .queue-item:not(.steering):nth-child(2) .queue-now`))
	wait(p, `() => document.querySelectorAll('#prompt-queue .queue-item.steering').length === 3`)

	// The agent takes them once the approved call is done; the queued
	// message waits for the next turn.
	aps, _ := e.store.PendingApprovals(ctx, id)
	if len(aps) != 1 {
		t.Fatalf("approvals %+v", aps)
	}
	if err := e.app.ResolveApproval(ctx, id, aps[0].ID, domain.DecisionAllow); err != nil {
		t.Fatal(err)
	}
	wait(p, `() => document.querySelectorAll('.item.user').length >= 4 && !document.querySelector('#prompt-queue .queue-item.steering')`)
	// The queued message's turn asks too.
	e.until("two turns", func() bool {
		if aps, _ := e.store.PendingApprovals(ctx, id); len(aps) == 1 {
			e.app.ResolveApproval(ctx, id, aps[0].ID, domain.DecisionAllow)
		}
		return len(e.items(id, domain.KindResult)) == 2
	})
	e.waitIdle(id)

	var order []string
	all, _ := e.store.Items(ctx, id)
	for _, it := range all {
		switch it.Kind {
		case domain.KindUser:
			order = append(order, it.Body)
		case domain.KindResult:
			order = append(order, "result")
		case domain.KindTool:
			if it.ToolName == "Bash" {
				order = append(order, "bash")
			}
		}
	}
	want := "start,bash,check the docs too,and one more,queued, then now,result,then write a summary,result"
	if got := strings.Join(order, ","); !strings.HasPrefix(got, "start,bash,check the docs too,and one more,queued, then now,") || !strings.HasSuffix(got, "result,then write a summary,bash,result") {
		t.Errorf("transcript %s, want about %s", got, want)
	}
	reply := e.items(id, domain.KindAssistant)
	var text string
	for _, it := range reply {
		text += it.Body
	}
	if !strings.Contains(text, "You also said: *check the docs too*") {
		t.Errorf("the reply did not hear the steer: %q", text)
	}
	wait(p, `() => [...document.querySelectorAll('.item.user')].filter(el => el.textContent.includes('steered into the turn')).length === 3`)
	p.MustElement(`.item.user .user-auto`).MustScrollIntoView()
	shot(p, "steer-transcript.png")
}
