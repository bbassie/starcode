package web

import (
	"os"
	"path/filepath"
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
