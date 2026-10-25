package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFindInDiff(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	git("init", "-q")
	write("a.go", "package a\n\nfunc Old() {}\n\nvar keep = 1\n")
	write("gone.txt", "Watch this line\n")
	git("add", ".")
	git("commit", "-qm", "init")
	write("a.go", "package a\n\nfunc NewWatch() {}\n\nvar keep = 1\n")
	os.Remove(filepath.Join(dir, "gone.txt"))
	write("new.md", "first\nwatch me\n")
	write("blob.bin", "watch\x00")

	ctx := context.Background()
	hits, more := FindInDiff(ctx, dir, "watch", 10)
	want := []DiffHit{
		{Path: "a.go", Line: 3, Op: '+', Text: "func NewWatch() {}"},
		{Path: "gone.txt", Line: 1, Op: '-', Text: "Watch this line"},
		{Path: "new.md", Line: 2, Op: '+', Text: "watch me"},
	}
	if !reflect.DeepEqual(hits, want) || more {
		t.Fatalf("FindInDiff = %+v, more %v", hits, more)
	}
	// A capital letter makes the search match case.
	if hits, _ := FindInDiff(ctx, dir, "Watch", 10); len(hits) != 2 {
		t.Errorf("smart case: %+v", hits)
	}
	// Context lines count too, with their new line number.
	if hits, _ := FindInDiff(ctx, dir, "keep", 10); !reflect.DeepEqual(hits, []DiffHit{{Path: "a.go", Line: 5, Op: ' ', Text: "var keep = 1"}}) {
		t.Errorf("context: %+v", hits)
	}
	if hits, more := FindInDiff(ctx, dir, "watch", 1); len(hits) != 1 || !more {
		t.Errorf("limit: %+v %v", hits, more)
	}
}
