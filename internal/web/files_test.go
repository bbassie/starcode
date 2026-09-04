package web

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProjectFileStaysWithinProject(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(inside, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got, err := readProjectFile(root, "inside.txt"); err != nil || got != "before" {
		t.Fatalf("readProjectFile() = %q, %v", got, err)
	}
	if err := writeProjectFile(root, "inside.txt", "after"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(inside); err != nil || string(got) != "after" {
		t.Fatalf("saved content = %q, %v", got, err)
	}
	if _, err := readProjectFile(root, "../outside.txt"); err == nil {
		t.Fatal("parent path was allowed")
	}
}

func TestListProjectDir(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	dir, entries, err := listProjectDir(root, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []projectDirEntry{{Name: "src", Path: "src", IsDir: true}, {Name: "README.md", Path: "README.md"}}
	if dir != "" || !reflect.DeepEqual(entries, want) {
		t.Fatalf("listProjectDir(root) = %q, %#v; want empty dir, %#v", dir, entries, want)
	}
	dir, entries, err = listProjectDir(root, "src")
	if err != nil {
		t.Fatal(err)
	}
	want = []projectDirEntry{{Name: "main.go", Path: "src/main.go"}}
	if dir != "src" || !reflect.DeepEqual(entries, want) {
		t.Fatalf("listProjectDir(src) = %q, %#v; want src, %#v", dir, entries, want)
	}
}

func TestProjectFileRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectFile(root, "escape.txt"); err == nil {
		t.Fatal("escaping symlink was allowed")
	}
}

func TestProjectFileRejectsGitInternals(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectFile(root, ".git/config"); err == nil {
		t.Fatal(".git internals were allowed")
	}
	_, entries, err := listProjectDir(root, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name == ".git" {
			t.Fatal(".git directory was listed")
		}
	}
}
