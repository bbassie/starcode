package views

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
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
