package web

import (
	"os"
	"path/filepath"
	"testing"

	"starcode/internal/web/views"
)

func TestSkillDesc(t *testing.T) {
	cases := map[string]string{
		"---\nname: unslop\ndescription: Cut AI tells from any writing.\n---\n# Unslop\n": "Cut AI tells from any writing.",
		"---\ndescription: \"Quoted, with: a colon\"\n---\nbody":                          "Quoted, with: a colon",
		"---\ndescription: >\n  folded text\n---\n":                                       "folded text",
		"---\nname: x\n---\n\n# Review the diff\n\nMore.":                                 "Review the diff",
		"\nFirst line wins.\nSecond.":                                                     "First line wins.",
		"":                                                                                "",
	}
	for in, want := range cases {
		if got := skillDesc([]byte(in)); got != want {
			t.Errorf("skillDesc(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlashItems(t *testing.T) {
	cfg, proj := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cfg, "skills", "unslop", "SKILL.md"), "---\nname: unslop\ndescription: Cut AI tells.\n---\n")
	write(filepath.Join(cfg, "skills", "broken", "notes.txt"), "no SKILL.md here")
	write(filepath.Join(cfg, "commands", "deploy.md"), "# Deploy\n\nShip it.\n")
	write(filepath.Join(proj, ".claude", "skills", "unslop", "SKILL.md"), "---\ndescription: project copy, must lose\n---\n")
	write(filepath.Join(proj, ".claude", "commands", "fix.md"), "Fix the failing test.\n")
	write(filepath.Join(proj, ".claude", "commands", ".hidden.md"), "not a command\n")

	items := slashItems("claude", cfg, proj)
	if len(items) != len(claudeCommands)+3 {
		t.Fatalf("got %d items, want %d: %+v", len(items), len(claudeCommands)+3, items)
	}
	for i, c := range claudeCommands {
		if items[i] != c {
			t.Errorf("item %d = %+v, want built-in %+v", i, items[i], c)
		}
	}
	want := []views.SlashItem{
		{Cmd: "/deploy", Desc: "Deploy"},
		{Cmd: "/fix", Desc: "Fix the failing test."},
		{Cmd: "/unslop", Desc: "Cut AI tells.", Skill: true},
	}
	for i, w := range want {
		if got := items[len(claudeCommands)+i]; got != w {
			t.Errorf("scanned item %d = %+v, want %+v", i, got, w)
		}
	}
	if got := slashItems("codex", cfg, proj); got != nil {
		t.Errorf("codex items = %+v, want none", got)
	}
	if got := slashItems("claude", cfg, ""); len(got) != len(claudeCommands)+2 {
		t.Errorf("without a project got %d items, want %d", len(got), len(claudeCommands)+2)
	}
}
