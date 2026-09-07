// Package web serves the UI: pages rendered with templ, one SSE stream per
// page for updates, and POST endpoints for commands.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/app"
	"starcode/internal/gitx"
	"starcode/internal/keys"
	"starcode/internal/providers"
	"starcode/internal/store"
	"starcode/internal/term"
	usagex "starcode/internal/usage"
	"starcode/internal/web/views"
)

//go:embed static
var staticFS embed.FS

var Themes = []string{"dark", "ocean", "amber", "green", "light"}

const tokenCookie = "starcode_token"

type Server struct {
	App       *app.App
	Log       *slog.Logger
	Token     string // empty disables auth
	Term      *term.Manager
	AttachDir string // where prompt attachments are saved, per thread
	Usage     *usagex.Scanner
	Providers *providers.Store
	// Update watches the executable; OnRestart is main's hook that shuts
	// the server down and re-execs it. Nil disables the restart button.
	Update    *SelfUpdate
	OnRestart func()
	// Secure is whether the server speaks TLS: cookies get the Secure
	// flag and plain HTTP requests are redirected. CA is the PEM of the
	// instance's own certificate authority, served for devices to
	// install; nil when the certificate came from elsewhere.
	Secure bool
	CA     []byte
	// Keys are the shortcut overrides; nil serves the defaults.
	Keys *keys.Store
	mux       *http.ServeMux
	cache     capsCache
	providers providerCache
	prs       prCache
	files     fileListCache
	assets    string
}

var errRestartUnavailable = errors.New("restart is not available in this build")

func New(a *app.App, log *slog.Logger, token, attachDir string, prov *providers.Store) *Server {
	usageCacheDir := ""
	if attachDir != "" {
		usageCacheDir = filepath.Dir(attachDir)
	}
	s := &Server{App: a, Log: log, Token: token, Term: term.NewManager(log), AttachDir: attachDir, Usage: usagex.New(usageCacheDir), Providers: prov, mux: http.NewServeMux()}
	if usageCacheDir != "" {
		s.cache.path = filepath.Join(usageCacheDir, "capabilities.json")
		s.cache.load()
	}
	s.assets = staticHash()
	views.SetAssetVersion(s.assets)
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	s.mux.HandleFunc("GET /{$}", s.home)
	s.mux.HandleFunc("GET /settings", s.settings)
	s.mux.HandleFunc("GET /settings/usage", s.usage)
	s.mux.HandleFunc("GET /settings/providers", s.providersPage)
	s.mux.HandleFunc("GET /settings/keys", s.keysPage)
	s.mux.HandleFunc("GET /api/keys", s.keysFragment)
	s.mux.HandleFunc("POST /api/keys/reset", s.resetKeys)
	s.mux.HandleFunc("POST /api/keys/{id}", s.setKey)
	s.mux.HandleFunc("POST /api/keys/{id}/reset", s.resetKey)
	s.mux.HandleFunc("GET /api/providers/refresh", s.refreshProviders)
	s.mux.HandleFunc("POST /api/providers", s.addProvider)
	s.mux.HandleFunc("POST /api/providers/interval", s.setCheckInterval)
	s.mux.HandleFunc("POST /api/providers/{name}", s.saveProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/toggle", s.toggleProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/delete", s.deleteProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/update", s.updateProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/models/toggle", s.toggleModel)
	s.mux.HandleFunc("POST /api/providers/{name}/models/add", s.addModel)
	s.mux.HandleFunc("POST /api/restart", s.restart)
	s.mux.HandleFunc("GET /threads/{id}", s.thread)
	s.mux.HandleFunc("GET /events", s.events)
	s.mux.HandleFunc("GET /api/search", s.search)

	s.mux.HandleFunc("POST /api/projects", s.addProject)
	s.mux.HandleFunc("GET /api/project-paths", s.projectPaths)
	s.mux.HandleFunc("GET /api/projects/{id}/files", s.projectFiles)
	s.mux.HandleFunc("GET /api/projects/{id}/prs", s.pullRequests)
	s.mux.HandleFunc("GET /api/projects/{id}/prs/{n}", s.pullRequest)
	s.mux.HandleFunc("POST /api/projects/{id}/prs/{n}/draft", s.pullRequestDraft)
	s.mux.HandleFunc("POST /api/projects/{id}/prs/{n}/thread", s.pullRequestThread)
	s.mux.HandleFunc("POST /api/projects/{id}/upload", s.uploadFiles)
	s.mux.HandleFunc("GET /api/attachments/{id}/{name}", s.attachmentFile)
	s.mux.HandleFunc("POST /api/projects/{id}/remove", s.removeProject)
	s.mux.HandleFunc("POST /api/projects/{id}/threads", s.newThreadForProject)
	s.mux.HandleFunc("POST /api/threads", s.newThread)
	s.mux.HandleFunc("POST /api/threads/{id}/send", s.send)
	s.mux.HandleFunc("POST /api/threads/{id}/queue/{qid}/remove", s.removeQueuedPrompt)
	s.mux.HandleFunc("POST /api/threads/{id}/interrupt", s.interrupt)
	s.mux.HandleFunc("POST /api/threads/{id}/delete", s.deleteThread)
	s.mux.HandleFunc("POST /api/threads/{id}/archive", s.archiveThread)
	s.mux.HandleFunc("POST /api/threads/{id}/unarchive", s.unarchiveThread)
	s.mux.HandleFunc("POST /api/threads/{id}/rename", s.renameThread)
	s.mux.HandleFunc("POST /api/threads/{id}/rules/revoke", s.revokeRule)
	s.mux.HandleFunc("POST /api/drafts/{key}", s.saveDraft)
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
	s.mux.HandleFunc("GET /api/term/{id}/panes", s.termPanes)
	s.Usage.SetRoots(s.usageRoots())
	s.warm()
	return s
}

// Watch runs the background checks (provider versions, a rebuilt binary)
// until ctx ends.
func (s *Server) Watch(ctx context.Context) {
	go s.watchProviders(ctx)
	if s.Update != nil {
		go s.Update.Watch(ctx, s.App.Bus.Publish)
	}
}

// Close ends every terminal shell; their ptys give them their own process
// session, so they would outlive the server otherwise.
func (s *Server) Close() {
	s.Term.Shutdown()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Secure && r.TLS == nil {
		// Same port, other scheme: the listener took a plain connection.
		http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		return
	}
	if r.URL.Path == caPath {
		s.caFile(w, r)
		return
	}
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
	http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: t, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 365})
}

const caPath = "/starcode-ca.crt"

// caFile serves the CA certificate, without auth: it is public, and a
// phone needs it before it can be trusted with the token page.
func (s *Server) caFile(w http.ResponseWriter, r *http.Request) {
	if s.CA == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="starcode-ca.crt"`)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(s.CA)
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
	// Only a local path may follow: "//host" is protocol-relative and
	// browsers read "/\host" the same way.
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}
	if r.Method == http.MethodPost {
		if s.tokenOK(r.FormValue("token")) {
			s.setTokenCookie(w, r.FormValue("token"))
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		views.Login(next, true, s.theme(r), s.CA != nil).Render(r.Context(), w)
		return
	}
	views.Login(next, false, s.theme(r), s.CA != nil).Render(r.Context(), w)
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

func (s *Server) agentNames() []string { return s.App.AgentNames() }

// ---- pages ----

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "home", views.Page{View: "home", Theme: s.theme(r), Draft: s.draft(r, "home")}).Render(r.Context(), w)
}

// draft is the saved composer text for key, for the page's initial
// prompt signal.
func (s *Server) draft(r *http.Request, key string) string {
	d, err := s.App.Store.Draft(r.Context(), key)
	if err != nil {
		s.Log.Warn("read draft", "key", key, "err", err)
	}
	return d
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "settings", views.Page{View: "settings", Theme: s.theme(r)}).Render(r.Context(), w)
}

func (s *Server) keysPage(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "keys", views.Page{View: "keys", Theme: s.theme(r)}).Render(r.Context(), w)
}

func (s *Server) keysData() views.KeysPageData {
	return views.KeysPageData{Bindings: s.Keys.All()}
}

func (s *Server) keysFragment(w http.ResponseWriter, r *http.Request) {
	datastar.NewSSE(w, r).PatchElementTempl(views.KeysSection(s.keysData()))
}

// setKey binds the posted combo to the action; the section re-renders
// either way, so a refused combo puts the old one back on screen.
func (s *Server) setKey(w http.ResponseWriter, r *http.Request) {
	if s.Keys == nil {
		s.fail(w, r, errors.New("shortcuts cannot be changed in this build"))
		return
	}
	var sig struct {
		Combo string `json:"combo"`
	}
	datastar.ReadSignals(r, &sig)
	err := s.Keys.Set(r.PathValue("id"), sig.Combo)
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.KeysSection(s.keysData()))
	if err != nil {
		sse.PatchElementTempl(views.Toast(err.Error()))
	}
}

func (s *Server) resetKey(w http.ResponseWriter, r *http.Request) {
	if s.Keys == nil {
		s.fail(w, r, errors.New("shortcuts cannot be changed in this build"))
		return
	}
	if err := s.Keys.Reset(r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	datastar.NewSSE(w, r).PatchElementTempl(views.KeysSection(s.keysData()))
}

func (s *Server) resetKeys(w http.ResponseWriter, r *http.Request) {
	if s.Keys == nil {
		s.fail(w, r, errors.New("shortcuts cannot be changed in this build"))
		return
	}
	if err := s.Keys.Reset(""); err != nil {
		s.fail(w, r, err)
		return
	}
	datastar.NewSSE(w, r).PatchElementTempl(views.KeysSection(s.keysData()))
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	days, metric := usageParams(r)
	s.page(r.Context(), "usage", views.Page{View: "usage", Theme: s.theme(r), UsageDays: days, UsageMetric: metric}).Render(r.Context(), w)
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.App.Store.Thread(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.App.MarkSeen(r.Context(), id)
	s.page(r.Context(), views.TabTitle(t), views.Page{View: "thread", ThreadID: id, Theme: s.theme(r), Draft: s.draft(r, id)}).Render(r.Context(), w)
}

// ---- helpers shared by events and commands ----

func (s *Server) sidebarData(ctx context.Context, current string, settings bool) (views.SidebarData, error) {
	ps, err := s.App.Store.Projects(ctx)
	if err != nil {
		return views.SidebarData{}, err
	}
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return views.SidebarData{}, err
	}
	seen, err := s.App.Store.Seen(ctx)
	if err != nil {
		return views.SidebarData{}, err
	}
	return views.SidebarData{Projects: ps, Threads: ts, Current: current, Agents: s.agentNames(), Settings: settings, Updates: s.updateCount(), Looks: s.agentLooks(), Seen: seen}, nil
}

// contextData is the little the window gauge needs: the thread and the
// catalog its model is looked up in. threadData reads the transcript and
// shells out for the branch, which is too much for a number that moves
// several times a turn.
func (s *Server) contextData(ctx context.Context, id string) (views.ThreadData, error) {
	t, err := s.App.Store.Thread(ctx, id)
	if err != nil {
		return views.ThreadData{}, err
	}
	caps, capsErrs := s.capabilities(ctx)
	return views.ThreadData{Thread: t, Settings: views.SettingsData{
		Agents: s.agentNames(), Looks: s.agentLooks(), Caps: caps, CapsErrs: capsErrs,
		Agent: t.Agent, Model: t.Model, Effort: t.Effort, Mode: t.PermissionMode,
	}}, nil
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
	queued, err := s.App.Store.QueuedPrompts(ctx, id)
	if err != nil {
		return views.ThreadData{}, err
	}
	aps, err := s.App.Store.Approvals(ctx, id)
	if err != nil {
		return views.ThreadData{}, err
	}
	caps, capsErrs := s.capabilities(ctx)
	branch := ""
	if p.Path != "" {
		branch = gitx.Read(ctx, p.Path).Branch
	}
	return views.ThreadData{Thread: t, Project: p, Items: items, Queued: queued, Approvals: aps, Rules: s.App.SessionRules(ctx, id), Branch: branch,
		Settings: views.SettingsData{Agents: s.agentNames(), Looks: s.agentLooks(), Caps: caps, CapsErrs: capsErrs, Agent: t.Agent, Model: t.Model, Effort: t.Effort, Mode: t.PermissionMode}}, nil
}
