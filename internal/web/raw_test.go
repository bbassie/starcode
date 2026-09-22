package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"starcode/internal/app"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// rawServer is a Server with a store holding project p1 at dir.
func rawServer(t *testing.T, dir string) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "raw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Append(context.Background(), "", domain.ProjectAdded{ID: "p1", Path: dir, Name: "p"}); err != nil {
		t.Fatal(err)
	}
	a := app.New(st, bus.New(16), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.Shutdown)
	return &Server{App: a}
}

func TestProjectRaw(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"page.html": "<h1>hi</h1><script>alert(1)</script>",
		"doc.pdf":   "%PDF-1.4",
		"pic.svg":   "<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>",
		"pic.png":   "\x89PNG",
		"page.xml":  "<?xml version='1.0'?><x/>",
		".env":      "SECRET=1",
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	s := rawServer(t, dir)
	get := func(path string) *http.Response {
		r := httptest.NewRequest("GET", "/api/projects/p1/raw?path="+path, nil)
		r.SetPathValue("id", "p1")
		w := httptest.NewRecorder()
		s.projectRaw(w, r)
		return w.Result()
	}
	for _, c := range []struct {
		path    string
		status  int
		ctype   string
		sandbox bool
	}{
		{"page.html", 200, "text/html", true},
		{"pic.svg", 200, "image/svg+xml", true},
		{"doc.pdf", 200, "application/pdf", false},
		{"pic.png", 200, "image/png", false},
		// Not a preview kind: a browser could run it as a page.
		{"page.xml", 404, "", false},
		{".env", 404, "", false},
		// Outside the project.
		{"../etc/passwd.html", 404, "", false},
		{"missing.html", 404, "", false},
	} {
		res := get(c.path)
		if res.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.path, res.StatusCode, c.status)
			continue
		}
		if c.status != 200 {
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.ctype) {
			t.Errorf("%s: content type %q, want %s", c.path, ct, c.ctype)
		}
		if got := strings.HasPrefix(res.Header.Get("Content-Security-Policy"), "sandbox"); got != c.sandbox {
			t.Errorf("%s: sandboxed = %v, want %v", c.path, got, c.sandbox)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", c.path)
		}
	}
}
