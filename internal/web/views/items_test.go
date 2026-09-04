package views

import (
	"encoding/json"
	"reflect"
	"testing"
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
