package keys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	good := map[string]string{
		"mod+k":          "mod+k",
		"Shift+Mod+K":    "mod+shift+k",
		"ctrl+alt+Up":    "mod+alt+up",
		"cmd+,":          "mod+,",
		"y":              "y",
		"alt+`":          "alt+`",
		"mod++":          "mod++",
		"f5":             "f5",
		"alt+shift+down": "alt+shift+down",
	}
	for in, want := range good {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "mod", "mod+", "mod+k+j", "escape", "enter", "tab", "mod+foo", "f13"} {
		if got, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", bad, got)
		}
	}
	if got := Display("mod+shift+k"); got != "Ctrl+Shift+K" {
		t.Errorf("Display = %q", got)
	}
}

func TestStoreSetResetAndConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keybindings.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Combo("approve") != "alt+y" || (*Store)(nil).Combo("deny") != "alt+n" {
		t.Fatal("defaults wrong")
	}
	if err := s.Set("approve", "Y"); err != nil {
		t.Fatal(err)
	}
	if s.Combo("approve") != "y" {
		t.Errorf("combo = %q", s.Combo("approve"))
	}
	if err := s.Set("deny", "mod+k"); err == nil || !strings.Contains(err.Error(), "Search and commands") {
		t.Errorf("conflict not refused: %v", err)
	}
	if err := s.Set("nope", "mod+x"); err == nil {
		t.Error("unknown action accepted")
	}
	// Setting the default drops the override; the file goes with the last one.
	if err := s.Set("approve", "alt+y"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file kept with no overrides: %v", err)
	}
	if err := s.Set("deny", "mod+shift+n"); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil || again.Combo("deny") != "mod+shift+n" {
		t.Fatalf("reload: %q, %v", again.Combo("deny"), err)
	}
	var custom int
	for _, b := range again.All() {
		if b.Custom {
			custom++
		}
	}
	if custom != 1 {
		t.Errorf("custom = %d", custom)
	}
	if err := again.Reset(""); err != nil || again.Combo("deny") != "alt+n" {
		t.Errorf("reset all: %q, %v", again.Combo("deny"), err)
	}

	// A file with junk in it loses the junk, keeps the rest.
	os.WriteFile(path, []byte(`{"deny": "mod+d", "bogus": "mod+x", "approve": "not a key at all"}`), 0o644)
	junk, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if junk.Combo("deny") != "mod+d" || junk.Combo("approve") != "alt+y" || len(junk.custom) != 1 {
		t.Errorf("after junk: %+v", junk.custom)
	}
}
