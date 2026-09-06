package providers

import (
	"log/slog"
	"path/filepath"
	"testing"
)

func TestOpenAddsBuiltInsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	s, err := Open(path, map[string]string{"claude": "/opt/claude"}, true, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, in := range s.Instances() {
		names = append(names, in.Name)
	}
	if got := len(names); got != 3 || names[0] != "claude" || names[1] != "codex" || names[2] != "fake" {
		t.Fatalf("instances = %v", names)
	}
	if s.Binary(Instance{Driver: "claude"}) != "/opt/claude" || s.Binary(Instance{Driver: "codex"}) != "codex" {
		t.Fatal("default binaries")
	}
	second := Instance{Name: "claude-work", Driver: "claude", Label: "Work", ConfigDir: "/tmp/claude-work", Env: []string{"FOO=bar"}, Enabled: true}
	if err := s.Put(second); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Instance{Name: "claude-work", Driver: "codex"}); err == nil {
		t.Fatal("driver change accepted")
	}
	if err := s.Put(Instance{Name: "codex", Driver: "codex"}); err != nil {
		t.Fatalf("replacing a built-in: %v", err)
	}
	if err := s.Delete("claude"); err == nil {
		t.Fatal("built-in deleted")
	}
	s2, err := Open(path, nil, false, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get("claude-work")
	if !ok || got.Label != "Work" || got.ConfigDir != "/tmp/claude-work" {
		t.Fatalf("reloaded instance = %+v", got)
	}
	if env := got.ChildEnv(); len(env) != 2 || env[1] != "CLAUDE_CONFIG_DIR=/tmp/claude-work" {
		t.Fatalf("child env = %v", env)
	}
	if err := s2.Delete("claude-work"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get("claude-work"); ok {
		t.Fatal("still present after delete")
	}
}

func TestValidate(t *testing.T) {
	bad := []Instance{
		{Name: "Bad Name", Driver: "claude"},
		{Name: "x", Driver: "gemini"},
		{Name: "x", Driver: "claude", Env: []string{"NOEQUALS"}},
		{Name: "x", Driver: "claude", ConfigDir: "relative/dir"},
	}
	for _, in := range bad {
		if Validate(in) == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	if Validate(Instance{Name: "ok-1", Driver: "codex", Env: []string{"A=1"}}) != nil {
		t.Error("rejected a valid instance")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Work Claude": "work-claude", "  Second.Codex_2 ": "second-codex-2", "###": ""} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
