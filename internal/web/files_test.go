package web

import (
	"encoding/base64"
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

func TestSaveUploads(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	files := []UploadFile{
		{Name: "shot.png", Contents: b64("one")},
		{Name: "C:\\fakepath\\shot.png", Contents: b64("two")}, // same base name dedupes
		{Name: "../escape.txt", Contents: b64("x")},            // flattened, not an escape
	}
	if err := saveUploads(root, "sub", files); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"shot.png": "one", "shot-1.png": "two", "escape.txt": "x"} {
		got, err := os.ReadFile(filepath.Join(root, "sub", name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escape.txt")); err == nil {
		t.Fatal("file landed outside the target directory")
	}

	if err := saveUploads(root, "../..", []UploadFile{{Name: "a", Contents: b64("x")}}); err == nil {
		t.Fatal("directory escape was allowed")
	}
	if err := saveUploads(root, "", []UploadFile{{Name: ".git", Contents: b64("x")}}); err == nil {
		t.Fatal(".git name was allowed")
	}
	if err := saveUploads(root, "", []UploadFile{{Name: "bad", Contents: "!!!"}}); err == nil {
		t.Fatal("bad base64 was accepted")
	}
}

func TestSaveAttachmentsAndPrompt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments", "thread-1")
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	paths, err := saveAttachments(dir, []UploadFile{
		{Name: "shot.png", Contents: b64("img")},
		{Name: "shot.png", Contents: b64("img2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "shot.png"), filepath.Join(dir, "shot-1.png")}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}

	files := []UploadFile{{Name: "shot.png", Mime: "image/png"}, {Name: "shot.png", Mime: "text/plain"}}
	got := promptWithAttachments("look at this", files, paths)
	wantPrompt := "look at this\n\n" +
		`[Attached image "shot.png" is saved at: ` + paths[0] + "]\n" +
		`[Attached file "shot-1.png" is saved at: ` + paths[1] + "]"
	if got != wantPrompt {
		t.Fatalf("prompt = %q", got)
	}
	if bare := promptWithAttachments("", files[:1], paths[:1]); bare != `[Attached image "shot.png" is saved at: `+paths[0]+"]" {
		t.Fatalf("bare prompt = %q", bare)
	}
}
