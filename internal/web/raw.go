package web

import (
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// The file panel shows some files rendered instead of as text: web pages,
// PDFs and pictures. The page is served by projectRaw and framed.

// previewKind is how the panel shows path: "html", "pdf", "image", or ""
// for the text editor.
func previewKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		return "html"
	case ".pdf":
		return "pdf"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp", ".ico", ".svg":
		return "image"
	}
	return ""
}

// rawURL is where the panel loads a file from. The thread rides in the
// query, since a frame's request carries no signals, and picks the
// thread's checkout the way projectFor does.
func rawURL(projectID, path, threadID string) string {
	q := url.Values{"path": {path}}
	if threadID != "" {
		q.Set("tid", threadID)
	}
	return "/api/projects/" + projectID + "/raw?" + q.Encode()
}

// projectRaw serves a project file as it is, for the panel's preview.
// Pages and SVGs come with a sandbox policy: an agent wrote them, and
// with scripts and this origin they could act as the reader. Nothing is
// cached, since the agent may change the file any moment.
func (s *Server) projectRaw(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if tid := r.URL.Query().Get("tid"); tid != "" {
		if t, err := s.App.Store.Thread(r.Context(), tid); err == nil && t.ProjectID == p.ID {
			p.Path = t.Dir(p)
		}
	}
	path := r.URL.Query().Get("path")
	// Only what the panel previews. Anything else could be a type the
	// browser runs as a page (.shtml, .xhtml, .xml) without the sandbox.
	if previewKind(path) == "" {
		http.NotFound(w, r)
		return
	}
	file, _, err := projectFile(p.Path, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	ext := strings.ToLower(filepath.Ext(path))
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	switch previewKind(path) {
	case "html":
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'")
	case "image":
		if ext == ".svg" {
			h.Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'")
		}
	}
	http.ServeFile(w, r, file)
}
