package views

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
	"starcode/internal/domain"
	"starcode/internal/store"
)

func TestSplitAttachments(t *testing.T) {
	body := "look at this\n\n" +
		`[Attached image "Screenshot From 2026-09-04 20-20-17.png" is saved at: /data/attachments/th1/Screenshot From 2026-09-04 20-20-17.png]` + "\n" +
		`[Attached file "log.txt" is saved at: /data/attachments/th1/log.txt]`
	text, atts := splitAttachments(body)
	if text != "look at this" {
		t.Fatalf("text = %q", text)
	}
	want := []attachmentRef{
		{Kind: "image", Name: "Screenshot From 2026-09-04 20-20-17.png", Path: "/data/attachments/th1/Screenshot From 2026-09-04 20-20-17.png"},
		{Kind: "file", Name: "log.txt", Path: "/data/attachments/th1/log.txt"},
	}
	if !reflect.DeepEqual(atts, want) {
		t.Fatalf("atts = %#v", atts)
	}
	if got := attachmentURL(atts[0].Path); got != "/api/attachments/th1/Screenshot%20From%202026-09-04%2020-20-17.png" {
		t.Fatalf("url = %q", got)
	}

	// Plain messages, including ones that merely mention attachments, pass
	// through untouched.
	for _, plain := range []string{"just text", "", "[Attached image mid-message]\nmore text"} {
		if text, atts := splitAttachments(plain); text != plain || len(atts) != 0 {
			t.Fatalf("splitAttachments(%q) = %q, %v", plain, text, atts)
		}
	}
}

func TestUsageChartAndFormatting(t *testing.T) {
	points := []UsagePoint{
		{Date: time.Now(), CostUSD: 1, InputTokens: 1_000},
		{Date: time.Now(), CostUSD: 2, InputTokens: 2_000},
	}
	if got := usageChartPath(points, "cost", 2); !strings.HasPrefix(got, "M 48.0 130.0") || !strings.Contains(got, "L 772.0 38.0") {
		t.Errorf("cost chart path = %q", got)
	}
	if got := usageAreaPath(points, "tokens", 2_000); !strings.HasSuffix(got, "L 772 222 L 48 222 Z") {
		t.Errorf("token area path = %q", got)
	}
	if x, width := usageChartHitBounds(0, 2); x != 48 || width != 362 {
		t.Errorf("first chart hit bounds = %.1f, %.1f", x, width)
	}
	if x, width := usageChartHitBounds(1, 2); x != 410 || width != 362 {
		t.Errorf("last chart hit bounds = %.1f, %.1f", x, width)
	}
	if got := formatTokens(1_250_000); got != "1.25M" {
		t.Errorf("formatTokens = %q", got)
	}
	if got := formatCost(0.0042); got != "$0.0042" {
		t.Errorf("formatCost = %q", got)
	}
}

func TestContextMeterNumbers(t *testing.T) {
	cases := []struct {
		used, window int64
		pct          int
		level        string
	}{
		{0, 200_000, 0, ""},
		{1, 200_000, 1, ""}, // anything in the window is not 0%
		{100_000, 200_000, 50, ""},
		{150_000, 200_000, 75, "warn"},
		{199_999, 200_000, 100, "full"}, // and nearly full is not 99%
		{300_000, 200_000, 100, "full"}, // a shrunken window does not overflow
		{50_000, 0, 0, ""},              // window unknown
	}
	for _, c := range cases {
		pct := contextPct(c.used, c.window)
		if pct != c.pct {
			t.Errorf("contextPct(%d, %d) = %d, want %d", c.used, c.window, pct, c.pct)
		}
		if got := contextLevel(pct); got != c.level {
			t.Errorf("contextLevel(%d) = %q, want %q", pct, got, c.level)
		}
	}
	for pct, want := range map[int]string{0: "2 100", 1: "2 100", 42: "42 100", 100: "100 100"} {
		if got := ringDash(pct); got != want {
			t.Errorf("ringDash(%d) = %q, want %q", pct, got, want)
		}
	}
	for n, want := range map[int64]string{200_000: "200K", 1_000_000: "1M", 272_000: "272K", 131_072: "131.1K", 0: ""} {
		if got := windowLabel(n); got != want {
			t.Errorf("windowLabel(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestContextWindowPrefersWhatTheAgentReported(t *testing.T) {
	caps := agent.Capabilities{Models: []agent.Model{
		{ID: "", DisplayName: "Default", Default: true, ContextWindow: 200_000},
		{ID: "opus[1m]", DisplayName: "Opus (1M)", ContextWindow: 1_000_000},
	}}
	d := ThreadData{
		Thread:   store.Thread{Agent: "claude", Model: "opus[1m]"},
		Settings: SettingsData{Caps: map[string]agent.Capabilities{"claude": caps}},
	}
	// Nothing reported yet: the catalog answers for the model selected.
	if got := contextWindow(d); got != 1_000_000 {
		t.Errorf("catalog window = %d, want 1000000", got)
	}
	d.Thread.Model = ""
	if got := contextWindow(d); got != 200_000 {
		t.Errorf("default model window = %d, want 200000", got)
	}
	// What the agent said about the model that ran wins.
	d.Thread.ContextWindow = 400_000
	if got := contextWindow(d); got != 400_000 {
		t.Errorf("reported window = %d, want 400000", got)
	}
	// An agent with no catalog and nothing reported has no window.
	empty := ThreadData{Thread: store.Thread{Agent: "codex"}}
	if got := contextWindow(empty); got != 0 {
		t.Errorf("unknown window = %d, want 0", got)
	}
}

func TestToolSummaryPrefersDescription(t *testing.T) {
	input, _ := json.Marshal(map[string]string{"command": "go test ./...", "description": "Run the test suite"})
	m := toolMeta{Input: input, Summary: "go test ./..."}
	if got := toolSummary("Bash", m); got != "Run the test suite" {
		t.Fatalf("Bash summary = %q", got)
	}
	if got := toolSummary("Read", m); got != "go test ./..." {
		t.Fatalf("Read summary = %q", got)
	}
	if got := toolSummary("Bash", toolMeta{Input: json.RawMessage("null"), Summary: "fallback"}); got != "fallback" {
		t.Fatalf("null input summary = %q", got)
	}
}

func TestPrettyJSONNull(t *testing.T) {
	if got := PrettyJSON(json.RawMessage("null")); got != "" {
		t.Fatalf("PrettyJSON(null) = %q", got)
	}
	if got := PrettyJSON(json.RawMessage(`{"a": null, "b": "x"}`)); got != "b: x" {
		t.Fatalf("PrettyJSON(nullfield) = %q", got)
	}
}

func TestPromptSummaryAndPreview(t *testing.T) {
	body := "First line\nSecond line\n\n" + `[Attached image "shot.png" is saved at: /data/attachments/th1/shot.png]`
	if got := promptSummary(body); got != "First line" {
		t.Fatalf("summary = %q", got)
	}
	if got := promptPreview(body); got != "First line\nSecond line\n1 attachment" {
		t.Fatalf("preview = %q", got)
	}
	attachmentOnly := `[Attached file "notes.txt" is saved at: /data/attachments/th1/notes.txt]`
	if got := promptSummary(attachmentOnly); got != "notes.txt" {
		t.Fatalf("attachment summary = %q", got)
	}
}

func TestSplitMatchesFoldsRunesOneToOne(t *testing.T) {
	parts := splitMatches("İİİİabc", "ABC")
	if len(parts) != 2 || parts[0].text != "İİİİ" || parts[0].hit || parts[1].text != "abc" || !parts[1].hit {
		t.Fatalf("parts = %+v", parts)
	}
	if parts := splitMatches("a-b-a", "A"); len(parts) != 3 || !parts[0].hit || parts[1].text != "-b-" || !parts[2].hit {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestWorkStateLeavesOutApprovalWaits(t *testing.T) {
	t0 := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	items := []store.Item{
		{ID: "i1", Kind: domain.KindTool, Status: domain.ItemDone, CreatedAt: t0, UpdatedAt: t0.Add(5 * time.Second)},
		{ID: "i2", Kind: domain.KindTool, Status: domain.ItemDone, CreatedAt: t0.Add(5 * time.Second), UpdatedAt: t0.Add(2 * time.Minute)},
	}
	aps := []store.Approval{
		// Two minutes of the span went by waiting for this one, some of it
		// before the block started.
		{ID: "a1", Decision: domain.DecisionAllow, CreatedAt: t0.Add(-30 * time.Second), ResolvedAt: t0.Add(90 * time.Second)},
		// Answered before the store kept answer times: counts for nothing.
		{ID: "a0", Decision: domain.DecisionDeny, CreatedAt: t0.Add(2 * time.Second)},
		// A later turn's approval does not touch this block.
		{ID: "a2", Decision: domain.DecisionAllow, CreatedAt: t0.Add(10 * time.Minute), ResolvedAt: t0.Add(11 * time.Minute)},
	}
	running, steps, dur := WorkState(items, aps)
	if running || steps != 2 {
		t.Fatalf("running=%v steps=%d", running, steps)
	}
	if dur != 30*time.Second {
		t.Fatalf("dur = %s, want 30s", dur)
	}
	if _, _, dur := WorkState(items, nil); dur != 2*time.Minute {
		t.Fatalf("without approvals dur = %s, want 2m", dur)
	}
}

func TestElapsedText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:                    "0s",
		12 * time.Second:                "12s",
		65 * time.Second:                "1m 05s",
		59*time.Minute + 59*time.Second: "59m 59s",
		time.Hour:                       "1h 00m 00s",
		3*time.Hour + 2*time.Minute + 5*time.Second: "3h 02m 05s",
	} {
		if got := elapsedText(d); got != want {
			t.Errorf("elapsedText(%s) = %q, want %q", d, got, want)
		}
	}
}
