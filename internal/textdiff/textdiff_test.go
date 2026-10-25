package textdiff

import (
	"strings"
	"testing"
)

func ops(lines []Line) string {
	var out []string
	for _, l := range lines {
		out = append(out, string(l.Op)+l.Text)
	}
	return strings.Join(out, ",")
}

func TestLines(t *testing.T) {
	if got := ops(Lines("a\nb\nc\n", "a\nx\nc\ny")); got != " a,-b,+x, c,+y" {
		t.Fatalf("diff = %s", got)
	}
}

func TestHunks(t *testing.T) {
	d := "diff --git a/x b/x\nindex 1..2 100644\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n ctx\n-old\n+new\n\\ No newline at end of file\n"
	lines := Hunks(d)
	if got := ops(lines); got != "@@@ -1,2 +1,2 @@, ctx,-old,+new" {
		t.Fatalf("hunks = %s", got)
	}
	if a, d := Count(lines); a != 1 || d != 1 {
		t.Fatalf("count = +%d -%d", a, d)
	}
}
