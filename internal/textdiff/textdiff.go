// Package textdiff turns edits into line diffs: the before and after
// text of an edit, or the unified hunks a patch carries. The transcript
// shows edits with them, and a turn's changed files are counted with
// them when there is no git checkpoint to ask.
package textdiff

import "strings"

// Line is one line of a diff: Op is '+', '-', ' ' or '@' (a hunk header).
type Line struct {
	Op   byte
	Text string
}

// Lines is the line diff from a to b, the lines they share kept as
// context. Big inputs skip the matching and read as a replacement.
func Lines(a, b string) []Line {
	x, y := split(a), split(b)
	var out []Line
	if len(x)*len(y) > 1_000_000 {
		for _, l := range x {
			out = append(out, Line{'-', l})
		}
		for _, l := range y {
			out = append(out, Line{'+', l})
		}
		return out
	}
	// Longest common subsequence, from the end.
	n, m := len(x), len(y)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			out = append(out, Line{' ', x[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, Line{'-', x[i]})
			i++
		default:
			out = append(out, Line{'+', y[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, Line{'-', x[i]})
	}
	for ; j < m; j++ {
		out = append(out, Line{'+', y[j]})
	}
	return out
}

// Hunks reads unified diff text: hunk headers, added, removed and
// context lines. File headers (diff, index, ---, +++) are skipped.
func Hunks(diff string) []Line {
	var out []Line
	for _, l := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			out = append(out, Line{'@', l})
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"),
			strings.HasPrefix(l, "diff "), strings.HasPrefix(l, "index "),
			strings.HasPrefix(l, "new file"), strings.HasPrefix(l, "deleted file"),
			strings.HasPrefix(l, "similarity"), strings.HasPrefix(l, "rename "),
			strings.HasPrefix(l, `\ No newline`), l == "":
		case l[0] == '+' || l[0] == '-' || l[0] == ' ':
			out = append(out, Line{l[0], l[1:]})
		default:
			out = append(out, Line{' ', l})
		}
	}
	return out
}

// Count is how many lines a diff adds and removes.
func Count(lines []Line) (add, del int) {
	for _, l := range lines {
		switch l.Op {
		case '+':
			add++
		case '-':
			del++
		}
	}
	return add, del
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
