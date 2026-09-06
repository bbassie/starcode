package gitx

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestListAndMatchFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "internal", "web"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".hidden"), 0o755)
	for _, f := range []string{"main.go", "internal/web/server.go", "internal/web/server_test.go", "README.md", ".hidden/x"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	got := ListFiles(context.Background(), dir)
	want := []string{"README.md", "internal/web/server.go", "internal/web/server_test.go", "main.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListFiles (walk) = %v", got)
	}
	if m := MatchFiles(got, "server", 10); !reflect.DeepEqual(m, []string{"internal/web/server.go", "internal/web/server_test.go"}) {
		t.Errorf("match server = %v", m)
	}
	if m := MatchFiles(got, "web test", 10); !reflect.DeepEqual(m, []string{"internal/web/server_test.go"}) {
		t.Errorf("match two words = %v", m)
	}
	if m := MatchFiles(got, "GO", 1); !reflect.DeepEqual(m, []string{"main.go"}) {
		t.Errorf("match limit and case = %v", m)
	}
	if m := MatchFiles(got, "  ", 10); m != nil {
		t.Errorf("empty query = %v", m)
	}
}
