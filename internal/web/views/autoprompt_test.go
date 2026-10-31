package views

import "testing"

func TestAutoSummary(t *testing.T) {
	for _, c := range []struct {
		m              promptMeta
		text           string
		label, preview string
	}{
		{promptMeta{}, "my own words", "", ""},
		{promptMeta{Auto: "schedule"}, "check the deploy", "", ""},
		{promptMeta{Auto: "prwatch", PR: 12}, "A check failed.\nDetails", "Pull request #12 changed", "A check failed."},
		{promptMeta{Auto: "agent"}, "Message from thread \"Fix CSS\" (0123456789abcdef01):\n\nall done", "Message from Fix CSS", "all done"},
		{promptMeta{Auto: "child"}, "Thread \"A\" (0123456789abcdef01) finished. Its last reply:\n\nok\n\nThread \"B\" (0123456789abcdef02) finished.", "Replies from 2 threads", "ok"},
		{promptMeta{Auto: "snooze"}, "look again", "Back from snooze", "look again"},
	} {
		_, label, preview := autoSummary(c.m, c.text)
		if label != c.label || preview != c.preview {
			t.Errorf("%q: got %q / %q, want %q / %q", c.text, label, preview, c.label, c.preview)
		}
	}
}
