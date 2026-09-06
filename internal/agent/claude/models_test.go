package claude

import "testing"

func TestVersionedName(t *testing.T) {
	cases := []struct{ name, desc, want string }{
		{"Fable", "Fable 5.1 · Most capable for your hardest and longest-running tasks", "Fable 5.1"},
		{"Opus (1M context)", "Opus 5 with 1M context · Best for everyday, complex tasks", "Opus 5 (1M)"},
		{"Sonnet", "Sonnet 5 · Efficient for routine tasks", "Sonnet 5"},
		{"Haiku", "Haiku 4.5 · Fastest for quick answers", "Haiku 4.5"},
		{"Default (recommended)", "Opus 5 with 1M context · Best for everyday, complex tasks", "Default (recommended)"},
		{"Fable", "", "Fable"},
	}
	for _, c := range cases {
		if got := versionedName(c.name, c.desc); got != c.want {
			t.Errorf("versionedName(%q, %q) = %q, want %q", c.name, c.desc, got, c.want)
		}
	}
}
