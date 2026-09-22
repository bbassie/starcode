package views

import (
	"strings"
	"testing"
)

func TestMidTrunc(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"starcode/fix-the-thing-that-broke", 15, "starcod…t-broke"},
		{"ünïcödé-branch-name", 9, "ünïc…name"},
	} {
		if got := MidTrunc(c.in, c.n); got != c.want {
			t.Errorf("MidTrunc(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// TestStreamURLEscapes checks the page's stream URL, which sits inside a
// Datastar expression: a quote in a query value must not end its string.
func TestStreamURLEscapes(t *testing.T) {
	for _, p := range []Page{
		{View: "projects", ProjectSel: "x');alert(1);//"},
		{View: "providers", ProviderSel: "a'b", ProviderTab: "c'd"},
		{View: "usage", UsageMetric: "m');alert(1);//"},
		{View: "thread", ThreadID: "t');x"},
	} {
		if expr := streamOpen(p); strings.Count(expr, "'") != 2 {
			t.Errorf("%s: quote leaked into %s", p.View, expr)
		}
	}
}
