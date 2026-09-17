package views

import (
	"bytes"
	"encoding/json"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	ghtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(
		// GFM registers its table renderer at 500. goldmark registers from
		// the highest number down, so the lowest number owns the kind.
		renderer.WithNodeRenderers(util.Prioritized(tableWrapper{}, 100)),
		ghtml.WithHardWraps(),
	),
)

// tableWrapper puts every table in a div that scrolls sideways, so a wide
// table scrolls inside the message instead of stretching the page.
type tableWrapper struct{}

func (tableWrapper) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(east.KindTable, func(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			_, _ = w.WriteString(`<div class="md-table"><table>`)
		} else {
			_, _ = w.WriteString("</table></div>\n")
		}
		return ast.WalkContinue, nil
	})
}

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
