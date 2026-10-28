package views

import (
	"context"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
)

// A window of known length gets the even-pace tick; one without does not.
func TestLimitBarsPace(t *testing.T) {
	var b strings.Builder
	ws := []agent.LimitWindow{
		{Label: "Session", Percent: 80, ResetsAt: time.Now().Add(4 * time.Hour), Length: 5 * time.Hour},
		{Label: "Other", Percent: 10},
	}
	if err := LimitBars(ws).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	if strings.Count(html, `class="limit-pace"`) != 1 || !strings.Contains(html, "ahead of the even pace (20%)") {
		t.Fatalf("html:\n%s", html)
	}
}
