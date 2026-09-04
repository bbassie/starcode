// Package web serves the UI: pages rendered with templ, one SSE stream per
// page for updates, and POST endpoints for commands.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"starcode/internal/app"
	"starcode/internal/gitx"
	"starcode/internal/store"
	"starcode/internal/term"
	"starcode/internal/web/views"
)

//go:embed static
var staticFS embed.FS

var Themes = []string{"dark", "amber", "green", "light"}

const tokenCookie = "starcode_token"

type Server struct {
	App       *app.App
	Log       *slog.Logger
	Token     string // empty disables auth
	Term      *term.Manager
	AttachDir string // where prompt attachments are saved, per thread
	mux       *http.ServeMux
	cache     capsCache
}

func New(a *app.App, log *slog.Logger, token, attachDir string) *Server {
	s := &Server{App: a, Log: log, Token: token, Term: term.NewManager(log), AttachDir: attachDir, mux: http.NewServeMux()}
	views.SetAssetVersion(staticHash())
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	s.mux.HandleFunc("GET /{$}", s.home)
	s.mux.HandleFunc("GET /threads/{id}", s.thread)
	s.mux.HandleFunc("GET /events", s.events)

	s.mux.HandleFunc("POST /api/projects", s.addProject)
	s.mux.HandleFunc("GET /api/project-paths", s.projectPaths)
	s.mux.HandleFunc("GET /api/projects/{id}/files", s.projectFiles)
	s.mux.HandleFunc("POST /api/projects/{id}/upload", s.uploadFiles)
	s.mux.HandleFunc("GET /api/attachments/{id}/{name}", s.attachmentFile)
	s.mux.HandleFunc("POST /api/projects/{id}/remove", s.removeProject)
	s.mux.HandleFunc("POST /api/projects/{id}/threads", s.newThreadForProject)
	s.mux.HandleFunc("POST /api/threads", s.newThread)
	s.mux.HandleFunc("POST /api/threads/{id}/send", s.send)
	s.mux.HandleFunc("POST /api/threads/{id}/interrupt", s.interrupt)
	s.mux.HandleFunc("POST /api/threads/{id}/delete", s.deleteThread)
	s.mux.HandleFunc("POST /api/threads/{id}/settings", s.setThreadSettings)
	s.mux.HandleFunc("POST /api/threads/{id}/approvals/{aid}/{decision}", s.approve)
	s.mux.HandleFunc("POST /api/theme", s.setTheme)
	s.mux.HandleFunc("GET /api/git/{id}/refresh", s.gitRefresh)
	s.mux.HandleFunc("GET /api/git/{id}/diff", s.gitDiff)
	s.mux.HandleFunc("GET /api/git/{id}/file", s.gitFile)
	s.mux.HandleFunc("POST /api/git/{id}/file", s.saveGitFile)
	s.mux.HandleFunc("GET /api/term/{id}/stream", s.termStream)
	s.mux.HandleFunc("POST /api/term/{id}/input", s.termInput)
	s.mux.HandleFunc("POST /api/term/{id}/resize", s.termResize)
	s.mux.HandleFunc("POST /api/term/{id}/kill", s.termKill)
	s.warm()
	return s
}

// Close ends every terminal shell; their ptys give them their own process
// session, so they would outlive the server otherwise.
func (s *Server) Close() {
	s.Term.Shutdown()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Token != "" {
		switch {
		case r.URL.Path == "/login":
			s.login(w, r)
			return
		case strings.HasPrefix(r.URL.Path, "/static/"):
			// The login page needs the stylesheet.
		case !s.authed(w, r):
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

func (s *Server) setTokenCookie(w http.ResponseWriter, t string) {
	http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 365})
}

// authed enforces the shared token. A cookie is the normal proof; ?token=
// still works for links, and everything else lands on the login page (or a
// 401 for API and stream requests, which are never typed by hand).
func (s *Server) authed(w http.ResponseWriter, r *http.Request) bool {
	if t := r.URL.Query().Get("token"); t != "" && s.tokenOK(t) {
		s.setTokenCookie(w, t)
		q := r.URL.Query()
		q.Del("token")
		r.URL.RawQuery = q.Encode()
		http.Redirect(w, r, r.URL.String(), http.StatusSeeOther)
		return false
	}
	if c, err := r.Cookie(tokenCookie); err == nil && s.tokenOK(c.Value) {
		return true
	}
	if r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/events" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	return false
}

// login serves the form (GET) and checks it (POST).
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	if r.Method == http.MethodPost {
		if s.tokenOK(r.FormValue("token")) {
			s.setTokenCookie(w, r.FormValue("token"))
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		views.Login(next, true, s.theme(r)).Render(r.Context(), w)
		return
	}
	views.Login(next, false, s.theme(r)).Render(r.Context(), w)
}

// staticHash fingerprints the embedded assets. It feeds the ?v= parameter on
// every /static URL, so a rebuild busts browser caches on a normal refresh.
func staticHash() string {
	h := sha256.New()
	fs.WalkDir(staticFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		h.Write([]byte(path))
		data, _ := staticFS.ReadFile(path)
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Versioned URLs change when the content does, so they can be
		// cached forever; anything else must be revalidated.
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) theme(r *http.Request) string {
	if c, err := r.Cookie("theme"); err == nil {
		for _, t := range Themes {
			if c.Value == t {
				return t
			}
		}
	}
	return Themes[0]
}

func (s *Server) agentNames() []string {
	names := make([]string, 0, len(s.App.Agents))
	for n := range s.App.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ---- pages ----

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	views.Layout("home", views.Page{View: "home", Theme: s.theme(r)}).Render(r.Context(), w)
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.App.Store.Thread(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	views.Layout(t.Title, views.Page{View: "thread", ThreadID: id, Theme: s.theme(r)}).Render(r.Context(), w)
}

// ---- helpers shared by events and commands ----

func (s *Server) sidebarData(ctx context.Context, current string, theme string) (views.SidebarData, error) {
	ps, err := s.App.Store.Projects(ctx)
	if err != nil {
		return views.SidebarData{}, err
	}
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return views.SidebarData{}, err
	}
	return views.SidebarData{Projects: ps, Threads: ts, Current: current, Agents: s.agentNames(), Themes: Themes, Theme: theme}, nil
}

func (s *Server) threadData(ctx context.Context, id string) (views.ThreadData, error) {
	t, err := s.App.Store.Thread(ctx, id)
	if err != nil {
		return views.ThreadData{}, err
	}
	p, err := s.App.Store.Project(ctx, t.ProjectID)
	if err != nil && err != store.ErrNotFound {
		return views.ThreadData{}, err
	}
	items, err := s.App.Store.Items(ctx, id)
	if err != nil {
		return views.ThreadData{}, err
	}
	aps, err := s.App.Store.PendingApprovals(ctx, id)
	if err != nil {
		return views.ThreadData{}, err
	}
	caps, capsErrs := s.capabilities(ctx)
	branch := ""
	if p.Path != "" {
		branch = gitx.Read(ctx, p.Path).Branch
	}
	return views.ThreadData{Thread: t, Project: p, Items: items, Approvals: aps, Rules: s.App.SessionRules(id), Branch: branch,
		Settings: views.SettingsData{Agents: s.agentNames(), Caps: caps, CapsErrs: capsErrs, Agent: t.Agent, Model: t.Model, Effort: t.Effort, Mode: t.PermissionMode}}, nil
}
