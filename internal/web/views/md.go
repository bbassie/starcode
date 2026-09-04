package views

import (
	"bytes"
	"encoding/json"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	ghtml "github.com/yuin/goldmark/renderer/html"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(), ghtml.WithHardWraps()),
)

// Markdown renders assistant prose. Raw HTML in the source is dropped by
// goldmark's default renderer, so model output cannot inject markup.
func Markdown(src string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "<pre>" + html.EscapeString(src) + "</pre>"
	}
	return buf.String()
}

// PrettyJSON re-indents a raw JSON value for display; non-JSON comes back
// unchanged.
func PrettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	if v == nil {
		// Adapters emit null for inputs they never saw completed; showing
		// the word "null" helps nobody.
		return ""
	}
	// Long string fields (file contents) read better unescaped.
	if m, ok := v.(map[string]any); ok {
		var sb strings.Builder
		for _, k := range sortedKeys(m) {
			val := m[k]
			if val == nil {
				continue
			}
			if s, ok := val.(string); ok {
				if strings.Contains(s, "\n") || len(s) > 80 {
					sb.WriteString(k + ":\n" + indent(s) + "\n")
					continue
				}
				sb.WriteString(k + ": " + s + "\n")
				continue
			}
			b, _ := json.MarshalIndent(val, "", "  ")
			sb.WriteString(k + ": " + string(b) + "\n")
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Put the most useful key first for common tools.
	first := map[string]int{"command": 0, "file_path": 1, "pattern": 2, "description": 3}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && less(keys[j], keys[j-1], first); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func less(a, b string, first map[string]int) bool {
	ra, oka := first[a]
	rb, okb := first[b]
	switch {
	case oka && okb:
		return ra < rb
	case oka:
		return true
	case okb:
		return false
	}
	return a < b
}
