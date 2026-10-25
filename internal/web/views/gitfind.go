package views

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"starcode/internal/gitx"
)

// Find in changes: the changes tab searches every changed file's diff
// (gitx.FindInDiff), lists the lines that match, and opens a file's diff
// at the line picked, with each match marked.

// findPart is a piece of a line, marked when it is a match.
type findPart struct {
	Text string
	Mark bool
}

// findParts splits s around the matches of q, with the same smart case as
// gitx.FindInDiff: case matters only when q has a capital letter.
func findParts(s, q string) []findPart {
	if q == "" {
		return []findPart{{Text: s}}
	}
	hay, needle := s, q
	if strings.ToLower(q) == q {
		hay = strings.ToLower(s)
	}
	// Lowercasing can change byte lengths outside ASCII; then the offsets
	// would not line up with s, and the line goes unmarked.
	if len(hay) != len(s) {
		return []findPart{{Text: s}}
	}
	var parts []findPart
	at := 0
	for {
		i := strings.Index(hay[at:], needle)
		if i < 0 {
			break
		}
		if i > 0 {
			parts = append(parts, findPart{Text: s[at : at+i]})
		}
		parts = append(parts, findPart{Text: s[at+i : at+i+len(needle)], Mark: true})
		at += i + len(needle)
	}
	if at < len(s) {
		parts = append(parts, findPart{Text: s[at:]})
	}
	return parts
}

// hitParts is a result line for the list: leading space trimmed, and a
// line whose first match sits far to the right cut so the match shows.
func hitParts(text, q string) []findPart {
	text = strings.TrimLeft(text, " \t")
	parts := findParts(text, q)
	if len(parts) > 1 && !parts[0].Mark && utf8.RuneCountInString(parts[0].Text) > 40 {
		r := []rune(parts[0].Text)
		parts[0].Text = "…" + string(r[len(r)-30:])
	}
	return parts
}

// diffRow is one line of the diff detail.
type diffRow struct {
	Class  string
	Parts  []findPart
	Target bool
}

// diffRows lays out a diff for the detail. With a search, matches are
// marked; line and op ("add", "del" or "ctx") name the row a result
// pointed at, which scrolls into view.
func diffRows(diff, find string, line int, op string) []diffRow {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	rows := make([]diffRow, 0, len(lines))
	oldLine, newLine, inHunk := 0, 0, false
	target := false
	for _, l := range lines {
		row := diffRow{Class: diffClass(l)}
		var n int
		var kind string
		switch {
		case strings.HasPrefix(l, "diff "):
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			if m := hunkNums(l); m != nil {
				oldLine, newLine, inHunk = m[0], m[1], true
			}
		case inHunk && strings.HasPrefix(l, "+"):
			n, kind = newLine, "add"
			newLine++
		case inHunk && strings.HasPrefix(l, "-"):
			n, kind = oldLine, "del"
			oldLine++
		case inHunk && strings.HasPrefix(l, " "):
			n, kind = newLine, "ctx"
			oldLine++
			newLine++
		}
		if kind != "" && find != "" {
			row.Parts = append([]findPart{{Text: l[:1]}}, findParts(l[1:], find)...)
		} else {
			row.Parts = []findPart{{Text: l}}
		}
		if !target && line > 0 && n == line && kind == op {
			row.Target, target = true, true
		}
		rows = append(rows, row)
	}
	return rows
}

// hunkNums reads the old and new start lines of a hunk header.
func hunkNums(l string) []int {
	// @@ -12,3 +14,5 @@
	f := strings.Fields(l)
	if len(f) < 3 || !strings.HasPrefix(f[1], "-") || !strings.HasPrefix(f[2], "+") {
		return nil
	}
	num := func(s string) (int, bool) {
		s, _, _ = strings.Cut(s[1:], ",")
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	o, ok1 := num(f[1])
	n, ok2 := num(f[2])
	if !ok1 || !ok2 {
		return nil
	}
	return []int{o, n}
}

// hitOp is the word for a hit's diff mark in the diff URL.
func hitOp(h gitx.DiffHit) string {
	switch h.Op {
	case '+':
		return "add"
	case '-':
		return "del"
	}
	return "ctx"
}

// findDiffURL opens path's diff at hit h with the matches of q marked.
func findDiffURL(projectID, q string, h gitx.DiffHit) string {
	return "/api/git/" + projectID + "/diff?path=" + urlQuery(h.Path) + "&find=" + urlQuery(q) + "&line=" + strconv.Itoa(h.Line) + "&op=" + hitOp(h)
}
