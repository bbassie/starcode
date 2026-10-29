package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/app"
	"starcode/internal/store"
	"starcode/internal/web/views"
)

// Thread commands that answer with an undo: settling, unpinning and
// snoozing each show a toast whose button posts the opposite (with
// ?undo=1, so that post shows no toast of its own), and mod+z presses
// the newest one (see the undo hotkey).

func (s *Server) threadCmdRoutes() {
	s.mux.HandleFunc("POST /api/threads/{id}/snooze", s.snoozeThread)
	s.mux.HandleFunc("POST /api/threads/{id}/wake", s.wakeThread)
	s.mux.HandleFunc("POST /api/threads/{id}/restart-session", s.restartSession)
	s.mux.HandleFunc("POST /api/threads/{id}/rewind/{item}", s.rewindThread)
	s.mux.HandleFunc("POST /api/threads/{id}/branch", s.branchThread)
	s.mux.HandleFunc("POST /api/threads/{id}/handoff", s.handOffThread)
	s.mux.HandleFunc("POST /api/stash", s.stashPrompt)
	s.read("/api/stash", s.stashList)
	s.mux.HandleFunc("POST /api/stash/{id}/restore", s.restoreStash)
	s.mux.HandleFunc("POST /api/stash/{id}/delete", s.deleteStash)
	s.mux.HandleFunc("POST /api/projects/{id}/settings", s.setProjectSettings)
}

// isUndo is whether the request is an undo toast's own post.
func isUndo(r *http.Request) bool { return r.URL.Query().Get("undo") == "1" }

// undoToast answers a command with the toast that undoes it.
func (s *Server) undoToast(w http.ResponseWriter, r *http.Request, msg string, at time.Time, undo string) {
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.UndoToast(msg, at, undo))
}

// threadTitle is the thread's title for a toast, "" when it is gone.
func (s *Server) threadTitle(r *http.Request, id string) string {
	t, err := s.App.Store.Thread(r.Context(), id)
	if err != nil {
		return ""
	}
	return t.Title
}

// snoozeThread parks the thread until ?until= (RFC 3339). The browser
// works the time out, since "tomorrow morning" is in the reader's time
// zone, not the server's.
func (s *Server) snoozeThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	until, err := time.Parse(time.RFC3339, r.URL.Query().Get("until"))
	if err != nil {
		s.fail(w, r, errors.New("the snooze time did not come through"))
		return
	}
	if err := s.App.Snooze(r.Context(), id, until); err != nil {
		s.fail(w, r, err)
		return
	}
	if isUndo(r) {
		s.ok(w, r)
		return
	}
	s.undoToast(w, r, "Snoozed “"+s.threadTitle(r, id)+"” until", until, "@post('/api/threads/"+id+"/wake?undo=1')")
}

// wakeThread ends a snooze by hand. The undo of a wake is not offered:
// the snooze time is gone with it.
// restartSession closes the thread's agent process; the next prompt
// starts a fresh one that resumes the conversation.
func (s *Server) restartSession(w http.ResponseWriter, r *http.Request) {
	if err := s.App.RestartSession(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) wakeThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.Wake(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// rewindThread cuts the conversation back to before a prompt and puts
// the prompt in the composer; the files stay as they are. The page's
// stream redraws the transcript.
func (s *Server) rewindThread(w http.ResponseWriter, r *http.Request) {
	text, err := s.App.Rewind(r.Context(), r.PathValue("id"), r.PathValue("item"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": text})
	sse.ExecuteScript("promptFocus()")
}

// branchThread opens a branch of the thread and goes to it; ?before=
// names the prompt to branch off in front of, none branches after the
// last turn.
func (s *Server) branchThread(w http.ResponseWriter, r *http.Request) {
	id, _, err := s.App.Branch(r.Context(), r.PathValue("id"), r.URL.Query().Get("before"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	datastar.NewSSE(w, r).Redirect("/threads/" + id)
}

// handOffThread gives the thread's work to the agent ?agent= names, in a
// new thread, and goes to it. The new thread runs the way the project
// starts that agent's threads when it names it; another install of the
// same CLI keeps the thread's own settings, and anything else gets the
// agent's defaults.
func (s *Server) handOffThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.App.Store.Thread(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	to := app.Handoff{Agent: r.URL.Query().Get("agent")}
	looks := s.agentLooks()
	switch p, err := s.App.Store.Project(ctx, t.ProjectID); {
	case err == nil && p.Agent == to.Agent:
		to.Model, to.Effort, to.PermissionMode = p.Model, p.Effort, p.Mode
	case looks[to.Agent].Driver != "" && looks[to.Agent].Driver == looks[t.Agent].Driver:
		to.Model, to.Effort, to.PermissionMode = t.Model, t.Effort, t.PermissionMode
	default:
		to.PermissionMode = s.defaultMode(ctx, to.Agent)
	}
	id, err := s.App.HandOff(ctx, t.ID, to)
	if err != nil {
		if id == "" {
			s.fail(w, r, err)
			return
		}
		s.Log.Warn("handoff", "thread", t.ID, "to", id, "err", err)
	}
	datastar.NewSSE(w, r).Redirect("/threads/" + id)
}

// stashPrompt puts the composer's text aside and empties the composer.
// ?key= is the composer's draft key (a thread id, or "home"). The
// attachments are not stashed; ?att= says how many stay behind.
func (s *Server) stashPrompt(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var sig struct {
		Prompt string `json:"prompt"`
	}
	datastar.ReadSignals(r, &sig)
	body := strings.TrimSpace(sig.Prompt)
	if body == "" {
		s.fail(w, r, errors.New("there is nothing to stash"))
		return
	}
	key := r.URL.Query().Get("key")
	id := newStashID()
	if err := s.App.Store.AddStash(r.Context(), id, sig.Prompt, key); err != nil {
		s.fail(w, r, err)
		return
	}
	if key != "" {
		s.App.SaveDraft(r.Context(), key, "")
	}
	msg := "Stashed the prompt"
	if n, _ := strconv.Atoi(r.URL.Query().Get("att")); n > 0 {
		msg = fmt.Sprintf("Stashed the text; the %d %s stay in the composer", n, plural(n, "attachment", "attachments"))
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": "", "stashn": s.App.Store.StashCount(r.Context())})
	sse.PatchElementTempl(views.UndoToast(msg, time.Time{}, "@post('/api/stash/"+id+"/restore?key="+key+"')"))
}

// stashList renders the stash menu's rows.
func (s *Server) stashList(w http.ResponseWriter, r *http.Request) {
	st, err := s.App.Store.Stashes(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"stashn": len(st)})
	sse.PatchElementTempl(views.StashList(st, r.URL.Query().Get("key")))
}

// restoreStash puts a stashed prompt in the composer and drops it from
// the stash. The page asks before replacing text it holds.
func (s *Server) restoreStash(w http.ResponseWriter, r *http.Request) {
	st, err := s.App.Store.TakeStash(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, errors.New("that stashed prompt is gone"))
		return
	}
	if key := r.URL.Query().Get("key"); key != "" {
		s.App.SaveDraft(r.Context(), key, st.Body)
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": st.Body, "pick": "", "stashn": s.App.Store.StashCount(r.Context())})
	sse.ExecuteScript("promptFocus()")
}

func (s *Server) deleteStash(w http.ResponseWriter, r *http.Request) {
	s.App.Store.TakeStash(r.Context(), r.PathValue("id"))
	s.stashList(w, r)
}

// setProjectSettings saves a project's defaults from the projects page:
// its signals name the page's one project form.
func (s *Server) setProjectSettings(w http.ResponseWriter, r *http.Request) {
	var sig struct {
		Own    bool   `json:"pown"`
		Agent  string `json:"agent"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
		Mode   string `json:"mode"`
	}
	datastar.ReadSignals(r, &sig)
	var ps app.ProjectSettings
	if sig.Own {
		ps.Agent, ps.Model, ps.Effort, ps.Mode = sig.Agent, sig.Model, sig.Effort, sig.Mode
	}
	if err := s.App.SetProjectSettings(r.Context(), r.PathValue("id"), ps); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func newStashID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// projectsPage is Settings > Projects; ?p= picks the project, and only
// a project's id is taken.
func (s *Server) projectsPage(w http.ResponseWriter, r *http.Request) {
	sel := r.URL.Query().Get("p")
	if _, err := s.App.Store.Project(r.Context(), sel); err != nil {
		sel = ""
	}
	s.page(r.Context(), "projects", views.Page{View: "projects", Theme: s.theme(r), Appearance: s.appearanceMode(r), Sidebar: s.sidebarMode(r), Dense: s.denseMode(r), ProjectSel: sel}).Render(r.Context(), w)
}

// projectsData is Settings > Projects for project sel, or the first.
func (s *Server) projectsData(ctx context.Context, projects []store.Project, sel string) views.ProjectsPageData {
	d := views.ProjectsPageData{Projects: projects}
	for _, p := range projects {
		if p.ID == sel || d.Sel.ID == "" {
			d.Sel = p
		}
		if p.ID == sel {
			break
		}
	}
	caps, capsErrs := s.capabilities(ctx)
	d.Settings = s.homeSettings(caps, capsErrs)
	if d.Sel.HasDefaults() {
		d.Settings.Agent, d.Settings.Model, d.Settings.Effort, d.Settings.Mode = d.Sel.Agent, d.Sel.Model, d.Sel.Effort, d.Sel.Mode
	}
	return d
}
