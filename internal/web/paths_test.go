package web

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCompleteProjectPathsStartsAtHome(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"alpha", "alpine", "beta", ".config"} {
		if err := os.Mkdir(filepath.Join(home, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(home, "alpha", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "not-a-directory"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if got, want := completeProjectPaths("", home), []string{"~/alpha/", "~/alpine/", "~/beta/", "~/.config/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("home completions = %q, want %q", got, want)
	}
	if got, want := completeProjectPaths("~/al", home), []string{"~/alpha/", "~/alpine/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tilde completions = %q, want %q", got, want)
	}
	if got, want := completeProjectPaths("~/alpha/", home), []string{"~/alpha/nested/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nested tilde completions = %q, want %q", got, want)
	}
}

func TestCompleteProjectPathsAbsolute(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "only", "two"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := completeProjectPaths(filepath.Join(root, "on"), t.TempDir()), []string{filepath.Join(root, "one") + "/", filepath.Join(root, "only") + "/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("absolute completions = %q, want %q", got, want)
	}
}

func TestCompleteProjectPathsRejectsRelativeInput(t *testing.T) {
	if got, want := completeProjectPaths("relative/path", t.TempDir()), []string{}; !reflect.DeepEqual(got, want) {
		t.Errorf("relative completions = %q, want %q", got, want)
	}
}
