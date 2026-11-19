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
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/app"
	"starcode/internal/gitx"
	"starcode/internal/keys"
	"starcode/internal/providers"
	"starcode/internal/push"
	"starcode/internal/store"
	"starcode/internal/term"
	usagex "starcode/internal/usage"
	"starcode/internal/web/views"
)

//go:embed static
var staticFS embed.FS

// Themes are the palettes, each with a dark and a light half;
// Appearances pick the half, "system" the one the device is in.
var (
	Themes      = []string{"starcode", "black", "ocean", "amber", "green"}
	Appearances = []string{"system", "dark", "light"}
)

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
	Keys      *keys.Store
	mux       *http.ServeMux
	cache     capsCache
	providers providerCache
	limits    limitsCache
	prs       prCache
	files     fileListCache
	sent      sentPrompts
	assets    string
	// Push sends Web Push messages; nil turns them off (push.go).
	Push     *push.Sender
	presence presence
	// agentKey is the secret behind the agents' keys (agentapi.go).
	agentOnce sync.Once
	agentKey  []byte
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
	// The icon sprite (views.Sprite) is versioned by its own hash, so it
	// can be cached for good.
	s.mux.HandleFunc("GET /static/icons.svg", func(w http.ResponseWriter, r *http.Request) {
		body, _ := views.Sprite()
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write([]byte(body))
	})
	// Mermaid (diagrams in replies, loaded on demand) is 3.5 MB of script
	// and ships gzipped; every browser takes gzip.
	s.mux.Handle("GET /static/mermaid.js", cacheStatic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		http.ServeFileFS(w, r, static, "mermaid.js.gz")
	})))
	// The service worker (notifications) must be served from the root
	// to control every page; it is never cached, so a rebuild replaces
	// it on the next page load.
	s.mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, "sw.js")
	})

	s.mux.HandleFunc("GET /{$}", s.home)
	s.mux.HandleFunc("GET /settings", s.settings)
	s.mux.HandleFunc("GET /settings/appearance", s.appearance)
	s.mux.HandleFunc("GET /settings/schedules", s.schedulesPage)
	s.mux.HandleFunc("GET /settings/usage", s.usage)
	s.mux.HandleFunc("GET /settings/providers", s.providersPage)
	s.mux.HandleFunc("GET /settings/keys", s.keysPage)
	s.mux.HandleFunc("GET /settings/projects", s.projectsPage)
	s.read("/api/keys", s.keysFragment)
	s.mux.HandleFunc("POST /api/keys/reset", s.resetKeys)
	s.mux.HandleFunc("POST /api/keys/{id}", s.setKey)
	s.mux.HandleFunc("POST /api/keys/{id}/reset", s.resetKey)
	s.read("/api/providers/refresh", s.refreshProviders)
	s.mux.HandleFunc("POST /api/providers", s.addProvider)
	s.mux.HandleFunc("POST /api/providers/interval", s.setCheckInterval)
	s.mux.HandleFunc("POST /api/providers/{name}", s.saveProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/toggle", s.toggleProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/delete", s.deleteProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/update", s.updateProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/login", s.loginProvider)
	s.mux.HandleFunc("POST /api/providers/{name}/login/code", s.loginCode)
	s.mux.HandleFunc("POST /api/providers/{name}/login/cancel", s.loginCancel)
	s.mux.HandleFunc("POST /api/providers/{name}/models/toggle", s.toggleModel)
	s.mux.HandleFunc("POST /api/providers/{name}/models/add", s.addModel)
	s.mux.HandleFunc("POST /api/restart", s.restart)
	s.mux.HandleFunc("GET /threads/{id}", s.thread)
	s.read("/api/threads/{id}/items", s.earlierItems)
	s.read("/events", s.events)
	s.read("/api/search", s.search)

	s.mux.HandleFunc("POST /api/projects", s.addProject)
	s.mux.HandleFunc("GET /api/project-paths", s.projectPaths)
	s.mux.HandleFunc("GET /api/attachments/{id}/{name}", s.attachmentFile)
	s.mux.HandleFunc("POST /api/projects/{id}/remove", s.removeProject)
	s.mux.HandleFunc("POST /api/projects/{id}/threads", s.newThreadForProject)
	s.mux.HandleFunc("POST /api/threads", s.newThread)
	s.mux.HandleFunc("POST /api/threads/{id}/send", s.send)
	s.mux.HandleFunc("POST /api/threads/{id}/queue/{qid}/remove", s.removeQueuedPrompt)
	s.mux.HandleFunc("POST /api/threads/{id}/queue/{qid}/edit", s.editQueuedPrompt)
	s.mux.HandleFunc("POST /api/threads/{id}/queue/{qid}/move", s.moveQueuedPrompt)
	s.mux.HandleFunc("POST /api/threads/{id}/queue/{qid}/steer", s.steerQueuedPrompt)
	s.mux.HandleFunc("POST /api/threads/{id}/interrupt", s.interrupt)
	s.mux.HandleFunc("POST /api/threads/{id}/items/{item}/stop", s.stopSubagent)
	s.mux.HandleFunc("POST /api/threads/{id}/compact", s.compactContext)
	s.mux.HandleFunc("POST /api/threads/{id}/retitle", s.retitleThread)
	s.mux.HandleFunc("POST /api/threads/{id}/delete", s.deleteThread)
	s.mux.HandleFunc("POST /api/threads/{id}/archive", s.archiveThread)
	s.mux.HandleFunc("POST /api/threads/{id}/unarchive", s.unarchiveThread)
	s.mux.HandleFunc("POST /api/threads/{id}/pin", s.pinThread)
	s.mux.HandleFunc("POST /api/threads/{id}/unpin", s.pinThread)
	s.mux.HandleFunc("POST /api/threads/{id}/unread", s.markUnread)
	s.mux.HandleFunc("POST /api/limits/refresh", s.refreshLimits)
	s.mux.HandleFunc("POST /api/limits/{name}/redeem", s.redeemReset)
	s.mux.HandleFunc("POST /api/sidebar", s.setSidebarMode)
	s.mux.HandleFunc("POST /api/pull-main", s.pullMain)
	s.mux.HandleFunc("POST /api/settle", s.setSettle)
	s.bulkRoutes()
	s.slashRoutes()
	s.threadCmdRoutes()
	s.pushRoutes()
	s.scheduleRoutes()
	s.mux.HandleFunc("POST /api/threads/{id}/rename", s.renameThread)
	s.mux.HandleFunc("POST /api/threads/{id}/pr/unlink", s.unlinkPR)
	s.mux.HandleFunc("POST /api/threads/{id}/pr/watch", s.watchPR)
	s.mux.HandleFunc("POST /api/threads/{id}/pr/unwatch", s.unwatchPR)
	s.mux.HandleFunc("POST /api/threads/{id}/pr/check", s.checkPR)
	s.mux.HandleFunc("POST /api/threads/{id}/rules/revoke", s.revokeRule)
	s.mux.HandleFunc("POST /api/drafts/{key}", s.saveDraft)
	s.mux.HandleFunc("POST /api/threads/{id}/settings", s.setThreadSettings)
	s.mux.HandleFunc("POST /api/threads/{id}/approvals/{aid}/{decision}", s.approve)
	s.mux.HandleFunc("POST /api/threads/{id}/approvals/{aid}/answer", s.answer)
	s.mux.HandleFunc("POST /api/theme", s.setTheme)
	s.mux.HandleFunc("GET /api/term/{id}/stream", s.termStream)
	s.mux.HandleFunc("POST /api/term/{id}/input", s.termInput)
	s.mux.HandleFunc("POST /api/term/{id}/resize", s.termResize)
	s.mux.HandleFunc("POST /api/term/{id}/kill", s.termKill)
	s.mux.HandleFunc("GET /api/term/{id}/panes", s.termPanes)
	s.Usage.SetRoots(s.usageRoots())
	s.warm()
	return s
}

// Watch runs the background checks (provider versions, linked pull
// requests, a rebuilt binary) until ctx ends.
func (s *Server) Watch(ctx context.Context) {
	go s.watchProviders(ctx)
	go s.watchPRs(ctx)
	go s.watchPush(ctx)
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
		views.Login(next, true, s.theme(r), s.appearanceMode(r), s.CA != nil).Render(r.Context(), w)
		return
	}
	views.Login(next, false, s.theme(r), s.appearanceMode(r), s.CA != nil).Render(r.Context(), w)
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

// sidebarMode is the sidebar layout the browser chose (Settings >
// Appearance > Threads): "inbox", one list, or "project", grouped.
func (s *Server) sidebarMode(r *http.Request) string {
	if c, err := r.Cookie("sidebar"); err == nil && c.Value == "project" {
		return "project"
	}
	return "inbox"
}

// denseMode is whether this browser wants one-line sidebar rows.
func (s *Server) denseMode(r *http.Request) bool {
	c, err := r.Cookie("dense")
	return err == nil && c.Value == "1"
}

// read registers a handler the page calls with Datastar's @query: the
// QUERY method carries the page's signals in the body, where a GET put
// them in the URL, the composer's draft among them, on every panel click
// and every stream reconnect. GET stays for links, curl and old pages.
func (s *Server) read(pattern string, h http.HandlerFunc) {
	s.mux.HandleFunc("GET "+pattern, h)
	s.mux.HandleFunc("QUERY "+pattern, h)
}

// theme is this browser's palette. The theme cookie once held "dark"
// and "light" too, from before a theme had two halves; they read as
// Starcode.
func (s *Server) theme(r *http.Request) string {
	if c, err := r.Cookie("theme"); err == nil && slices.Contains(Themes, c.Value) {
		return c.Value
	}
	return Themes[0]
}

// appearanceMode is this browser's half of the theme: dark, light, or
// whichever the device is in. A browser that picked the old Light theme
// and nothing since stays light.
func (s *Server) appearanceMode(r *http.Request) string {
	if c, err := r.Cookie("appearance"); err == nil && slices.Contains(Appearances, c.Value) {
		return c.Value
	}
	if c, err := r.Cookie("theme"); err == nil && c.Value == "light" {
		return "light"
	}
	return Appearances[0]
}

func (s *Server) agentNames() []string { return s.App.AgentNames() }

// ---- pages ----

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "home", views.Page{View: "home", Theme: s.theme(r), Appearance: s.appearanceMode(r), Sidebar: s.sidebarMode(r), Dense: s.denseMode(r), Draft: s.draft(r, "home")}).Render(r.Context(), w)
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

func (s *Server) appearance(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "appearance", views.Page{View: "appearance", Theme: s.theme(r), Appearance: s.appearanceMode(r), Sidebar: s.sidebarMode(r), Dense: s.denseMode(r)}).Render(r.Context(), w)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "settings", views.Page{View: "settings", Theme: s.theme(r), Appearance: s.appearanceMode(r), Sidebar: s.sidebarMode(r), Dense: s.denseMode(r), PairURL: s.pairURL(r)}).Render(r.Context(), w)
}

// pairURL is the link a phone can open to sign in: the address this
// browser used when it is one a phone can reach, else the machine's first
// private IPv4, with the token in the query (authed turns that into the
// cookie). Empty without a token, when there is nothing to pair.
func (s *Server) pairURL(r *http.Request) string {
	if s.Token == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		host = ""
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
					host = ipn.IP.String()
					break
				}
			}
		}
		if host == "" {
			return ""
		}
	}
	scheme := "http"
	if s.Secure {
		scheme = "https"
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host + "/?token=" + url.QueryEscape(s.Token)
}

func (s *Server) keysPage(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "keys", views.Page{View: "keys", Theme: s.theme(r), Appearance: s.appearanceMode(r), Sidebar: s.sidebarMode(r), Dense: s.denseMode(r)}).Render(r.Context(), w)
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
	s.page(r.Context(), "usage", views.Page{View: "usage", Theme: s.theme(r), Appearance: s.appearanceMode(r), Sidebar: s.sidebarMode(r), Dense: s.denseMode(r), UsageDays: days, UsageMetric: metric}).Render(r.Context(), w)
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.App.Store.Thread(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Before the look below counts: what came in since the last one gets
	// a line above it (views.NewMark).
	var since time.Time
	if at, ok, _ := s.App.Store.SeenAt(r.Context(), id); ok && t.UpdatedAt.After(at) {
		since = at
	}
	s.App.MarkSeen(r.Context(), id)
	s.page(r.Context(), views.TabTitle(t), views.Page{View: "thread", ThreadID: id, NewSince: since, Theme: s.theme(r), Appearance: s.appearanceMode(r), Sidebar: s.sidebarMode(r), Dense: s.denseMode(r), Draft: s.draft(r, id)}).Render(r.Context(), w)
}

// ---- helpers shared by events and commands ----

func (s *Server) sidebarData(ctx context.Context, current, view, mode string, dense bool) (views.SidebarData, error) {
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
	prs, err := s.App.Store.PRStates(ctx)
	if err != nil {
		return views.SidebarData{}, err
	}
	return views.SidebarData{Projects: ps, Threads: ts, Current: current, Mode: mode, Dense: dense, Agents: s.agentNames(), View: view, Updates: s.updateCount(), SignedOut: s.signedOutCount(), Looks: s.agentLooks(), Seen: seen, PRs: prs}, nil
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
	return views.ThreadData{Thread: t, Limits: s.agentLimits(t.Agent), Settings: views.SettingsData{
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
	steering, err := s.App.Store.SteeredPrompts(ctx, id)
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
		branch = gitx.Branch(ctx, p.Path)
	}
	var pr store.PRState
	if ref, ok := t.Linked(); ok {
		states, err := s.App.Store.PRStates(ctx)
		if err != nil {
			return views.ThreadData{}, err
		}
		pr = states[ref]
	}
	var parent store.Thread
	if t.Parent != "" {
		parent, _ = s.App.Store.Thread(ctx, t.Parent)
	}
	scheduled, err := s.App.Store.Schedules(ctx, id)
	if err != nil {
		return views.ThreadData{}, err
	}
	bg, _ := s.App.Store.Background(ctx, id)
	return views.ThreadData{Thread: t, Project: p, Items: items, Parent: parent, Scheduled: scheduled, Notify: s.App.Store.NotifyLevel(ctx, id), Queued: queued, Steering: steering, Background: bg, Approvals: aps, Rules: s.App.SessionRules(ctx, id), Branch: branch, PR: pr, PRNext: app.PRWatchNext(t, pr.CheckedAt), Limits: s.agentLimits(t.Agent), Stashed: s.App.Store.StashCount(ctx),
		Settings: views.SettingsData{Agents: s.agentNames(), Looks: s.agentLooks(), Caps: caps, CapsErrs: capsErrs, Agent: t.Agent, Model: t.Model, Effort: t.Effort, Mode: t.PermissionMode}}, nil
}
