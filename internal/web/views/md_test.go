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

// Links to other sites open in a new tab; links within starcode and
// mail links stay as they are.
func TestMarkdownExternalLinksOpenNewTab(t *testing.T) {
	got := Markdown("[x](https://example.com) https://foo.com www.bar.com [t](/threads/abc) me@x.com")
	for _, want := range []string{
		`<a href="https://example.com" target="_blank" rel="noopener">`,
		`<a href="https://foo.com" target="_blank" rel="noopener">`,
		`<a href="http://www.bar.com" target="_blank" rel="noopener">`,
		`<a href="/threads/abc">`,
		`<a href="mailto:me@x.com">`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
}
