package views

import (
	"strings"
	"testing"
)

func TestMarkdownWrapsTables(t *testing.T) {
	got := Markdown("| a | b |\n|---|---|\n| 1 | 2 |\n")
	if !strings.Contains(got, `<div class="md-table"><table>`) || !strings.Contains(got, "</table></div>") {
		t.Fatalf("table is not wrapped:\n%s", got)
	}
	if !strings.Contains(got, "<th>a</th>") || !strings.Contains(got, "<td>2</td>") {
		t.Fatalf("table body lost:\n%s", got)
	}
}
