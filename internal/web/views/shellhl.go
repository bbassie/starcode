package views

import (
	"html"
	"strings"
)

// Shell highlighting for the command tool rows. It runs here rather than
// in highlight.js because the rows are morphed while a turn streams: a
// page-side pass would be undone by every morph and flicker. The spans use
// highlight.js class names so the themes colour them like the editor.
//
// It is a tokenizer, not a parser: the word that starts a command (after
// a pipe, &&, ;, a newline or a keyword) is the command, words starting
// with - are flags, and quotes, $variables and comments are what they look
// like. Heredoc bodies and nested quoting read as plain words, which is
// fine for a line meant to be read at a glance.

var shellKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"for": true, "in": true, "do": true, "done": true, "while": true, "until": true,
	"case": true, "esac": true, "function": true, "select": true, "time": true,
}

// shellHTML is cmd as escaped HTML with highlight spans.
func shellHTML(cmd string) string {
	var b strings.Builder
	span := func(class, text string) {
		b.WriteString(`<span class="hljs-`)
		b.WriteString(class)
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(text))
		b.WriteString(`</span>`)
	}
	plain := func(text string) { b.WriteString(html.EscapeString(text)) }

	start := true // the next word names a command
	i := 0
	for i < len(cmd) {
		c := cmd[i]
		switch {
		case c == '\n':
			plain("\n")
			start = true
			i++
		case c == ' ' || c == '\t':
			j := i
			for j < len(cmd) && (cmd[j] == ' ' || cmd[j] == '\t') {
				j++
			}
			plain(cmd[i:j])
			i = j
		case c == '\\' && i+1 < len(cmd):
			// A line continuation or an escaped character stays with the
			// text around it.
			plain(cmd[i : i+2])
			i += 2
		case c == '#' && (i == 0 || cmd[i-1] == ' ' || cmd[i-1] == '\t' || cmd[i-1] == '\n'):
			j := strings.IndexByte(cmd[i:], '\n')
			if j < 0 {
				j = len(cmd) - i
			}
			span("comment", cmd[i:i+j])
			i += j
		case c == '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			end := len(cmd)
			if j >= 0 {
				end = i + 1 + j + 1
			}
			span("string", cmd[i:end])
			start = false
			i = end
		case c == '"':
			end := closingQuote(cmd, i)
			b.WriteString(`<span class="hljs-string">`)
			writeVars(&b, cmd[i:end])
			b.WriteString(`</span>`)
			start = false
			i = end
		case c == '$':
			end := varEnd(cmd, i)
			span("variable", cmd[i:end])
			start = false
			i = end
		case strings.ContainsRune("|&;()<>", rune(c)):
			j := i
			for j < len(cmd) && strings.ContainsRune("|&;()<>", rune(cmd[j])) {
				j++
			}
			op := cmd[i:j]
			span("meta", op)
			// After a redirection comes a file, after anything else a command.
			start = !strings.ContainsAny(op, "<>")
			i = j
		default:
			j := i
			for j < len(cmd) && !strings.ContainsRune(" \t\n|&;()<>'\"$#\\", rune(cmd[j])) {
				j++
			}
			if j == i {
				// A # inside a word, or a lone backslash at the end.
				j = i + 1
			}
			word := cmd[i:j]
			switch {
			case start && shellKeywords[word]:
				span("keyword", word)
				// "for x in" and "case x in" take a word next, not a command.
				start = word != "for" && word != "case" && word != "select" && word != "in"
			case start && strings.Contains(word, "=") && !strings.HasPrefix(word, "="):
				// FOO=bar before a command sets its environment.
				span("attr", word)
			case start:
				span("built_in", word)
				start = false
			case strings.HasPrefix(word, "-") && len(word) > 1:
				span("attr", word)
			default:
				plain(word)
			}
			i = j
		}
	}
	return b.String()
}

// closingQuote is the index after the double quote that closes the one
// at i, skipping escaped characters; the end of s when it never closes.
func closingQuote(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(s)
}

// varEnd is the index after the variable that starts with the $ at i:
// ${...}, $(...) kept short to its opening, $NAME, or a special like $?.
func varEnd(s string, i int) int {
	j := i + 1
	if j >= len(s) {
		return j
	}
	switch {
	case s[j] == '{':
		if k := strings.IndexByte(s[j:], '}'); k >= 0 {
			return j + k + 1
		}
		return len(s)
	case s[j] == '(':
		// Command substitution: colour the $( and let the command inside
		// read as one.
		return j + 1
	case strings.ContainsRune("?!#@*$0123456789-", rune(s[j])):
		return j + 1
	}
	for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
		j++
	}
	return j
}

// writeVars writes a double-quoted string with its $variables marked.
func writeVars(b *strings.Builder, s string) {
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] != '$' || i+1 >= len(s) || s[i+1] == '(' {
			continue
		}
		end := varEnd(s, i)
		if end == i+1 || s[end-1] == '"' {
			continue
		}
		b.WriteString(html.EscapeString(s[last:i]))
		b.WriteString(`<span class="hljs-variable">`)
		b.WriteString(html.EscapeString(s[i:end]))
		b.WriteString(`</span>`)
		last = end
		i = end - 1
	}
	b.WriteString(html.EscapeString(s[last:]))
}
