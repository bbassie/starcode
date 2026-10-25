//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"starcode/internal/domain"
)

// The stop button on a running subagent ends that subagent, and the turn
// goes on to its end.
func TestStopSubagent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Slow enough to click while the subagent runs.
	e.fake.Delay = 300 * time.Millisecond
	id := e.thread()
	p := e.page("/threads/"+id, false)
	if err := e.app.SendPrompt(ctx, id, "go"); err != nil {
		t.Fatal(err)
	}
	btn := p.Timeout(30 * time.Second).MustElement(`details.item.agent.st-running .agent-stop`)
	click(btn)
	e.waitIdle(id)
	items, err := e.store.Items(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var agentSt string
	var reply bool
	for _, it := range items {
		if it.Kind == domain.KindTool && it.ToolName == "Task" {
			agentSt = it.Status
		}
		if it.Kind == domain.KindAssistant && it.Body != "" {
			reply = true
		}
	}
	if agentSt != domain.ItemFailed {
		t.Errorf("subagent status %q, want failed", agentSt)
	}
	if th, _ := e.store.Thread(ctx, id); th.Status != domain.StatusIdle || !reply {
		t.Errorf("turn did not run on after the stop: status %q, reply %v", th.Status, reply)
	}
	wait(p, `() => !document.querySelector('.agent-stop')`)
}
