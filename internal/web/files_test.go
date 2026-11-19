package web

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSaveAttachmentsRejects(t *testing.T) {
	dir := t.TempDir()
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	paths, err := saveAttachments(dir, []UploadFile{{Name: "../escape.txt", Contents: b64("x")}})
	if err != nil || len(paths) != 1 || filepath.Dir(paths[0]) != dir {
		t.Fatalf("a climbing name is flattened into dir: %v, %v", paths, err)
	}
	if _, err := saveAttachments(dir, []UploadFile{{Name: ".git", Contents: b64("x")}}); err == nil {
		t.Fatal(".git name was allowed")
	}
	if _, err := saveAttachments(dir, []UploadFile{{Name: "bad", Contents: "!!!"}}); err == nil {
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
