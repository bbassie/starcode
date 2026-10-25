package views

import (
	"html"
	"regexp"
	"testing"
)

func TestShellHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{`go test ./...`, `<span class="hljs-built_in">go</span> test ./...`},
		{`ls -la | grep -v x`, `<span class="hljs-built_in">ls</span> <span class="hljs-attr">-la</span> <span class="hljs-meta">|</span> <span class="hljs-built_in">grep</span> <span class="hljs-attr">-v</span> x`},
		{`echo 'a b' "c $HOME d"`, `<span class="hljs-built_in">echo</span> <span class="hljs-string">&#39;a b&#39;</span> <span class="hljs-string">&#34;c <span class="hljs-variable">$HOME</span> d&#34;</span>`},
		{`make build && ./starcode # run it`, `<span class="hljs-built_in">make</span> build <span class="hljs-meta">&amp;&amp;</span> <span class="hljs-built_in">./starcode</span> <span class="hljs-comment"># run it</span>`},
		{`FOO=1 go run . > out.txt`, `<span class="hljs-attr">FOO=1</span> <span class="hljs-built_in">go</span> run . <span class="hljs-meta">&gt;</span> out.txt`},
		{"for f in *.go; do\n  wc -l $f\ndone", "<span class=\"hljs-keyword\">for</span> f in *.go<span class=\"hljs-meta\">;</span> <span class=\"hljs-keyword\">do</span>\n  <span class=\"hljs-built_in\">wc</span> <span class=\"hljs-attr\">-l</span> <span class=\"hljs-variable\">$f</span>\n<span class=\"hljs-keyword\">done</span>"},
		{`echo ${X:-y} $?`, `<span class="hljs-built_in">echo</span> <span class="hljs-variable">${X:-y}</span> <span class="hljs-variable">$?</span>`},
	}
	for _, c := range cases {
		if got := shellHTML(c.in); got != c.want {
			t.Errorf("shellHTML(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

// The text of the highlighted HTML is always the command, whatever odd
// input it gets: nothing dropped, nothing doubled, everything escaped.
func TestShellHTMLKeepsText(t *testing.T) {
	tags := regexp.MustCompile(`<[^>]*>`)
	for _, in := range []string{
		`echo "unterminated $X`, `echo 'open`, `a\`, `$`, `x#y # z`, `<script>alert(1)</script>`,
		`cat <<'EOF'` + "\nbody $x\nEOF", `"$(git rev-parse HEAD)"`, `echo "\"$A\""`, `a=b c=d`, `$(`,
	} {
		out := shellHTML(in)
		if got := html.UnescapeString(tags.ReplaceAllString(out, "")); got != in {
			t.Errorf("text of shellHTML(%q) = %q", in, got)
		}
	}
}
