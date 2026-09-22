package views

import "testing"

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
