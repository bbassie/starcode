package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/agent"
	"starcode/internal/app"
	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
	"starcode/internal/web/views"
)

// Command handlers. Each one reads the signals Datastar posted, runs the
// command, and answers with either 204 (the SSE stream will show the
// result) or a small SSE response of its own for things that are local to
// the caller: clearing the composer, showing an error, redirecting.

type signals struct {
	Path    string `json:"path"`
	Prompt  string `json:"prompt"`
	Agent   string `json:"agent"`
	Model   string `json:"model"`
	Project string `json:"project"`
	Theme   string `json:"theme"`
	Effort  string `json:"effort"`
	// Mode is nil when the page carried no mode signal at all (a "new
	// thread" hotkey on the settings page, say), which is different from
	// the agent's default mode picked on purpose.
	Mode   *string      `json:"mode"`
	File   string       `json:"_file"`
	Title  string       `json:"title"`
	Attach []UploadFile `json:"_attach"`
	// WT is the home composer's "Start in" choice (see app.ThreadStart):
	// empty for the project's default, "local", "worktree" with WTBase
	// as the branch to start from, or "reuse" with WTFrom the thread
	// whose worktree the new one shares.
	WT     string `json:"wt"`
	WTBase string `json:"wtbase"`
	WTFrom string `json:"wtfrom"`
}

func (s *Server) readSignals(r *http.Request) signals {
	var sig signals
	datastar.ReadSignals(r, &sig)
	return sig
}

// modeOr is the mode signal, or def when the page had none.
func (sig signals) modeOr(def string) string {
	if sig.Mode == nil {
		return def
	}
	return *sig.Mode
}

// defaultMode is the permission mode a new thread gets when the request
// did not say: the agent's never-ask mode (see views.DefaultMode).
func (s *Server) defaultMode(ctx context.Context, agent string) string {
	caps, _ := s.capabilities(ctx)
	return views.DefaultMode(caps[agent])
}

// homeSettings is the composer state of the home page: the first agent
// with its default permission mode.
func (s *Server) homeSettings(caps map[string]agent.Capabilities, capsErrs map[string]error) views.SettingsData {
	first := views.FirstOr(s.agentNames(), "claude")
	return views.SettingsData{Agents: s.agentNames(), Looks: s.agentLooks(), Caps: caps, CapsErrs: capsErrs, Agent: first, Mode: views.DefaultMode(caps[first])}
}

func newSSE(w http.ResponseWriter, r *http.Request) *datastar.ServerSentEventGenerator {
	return datastar.NewSSE(w, r)
}

// ok answers a command that has nothing to say: an empty event stream, which
// Datastar closes cleanly (a bare 204 makes it abort the fetch).
func (s *Server) ok(w http.ResponseWriter, r *http.Request) {
	datastar.NewSSE(w, r)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Warn("command failed", "path", r.URL.Path, "err", err)
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.Toast(err.Error()))
}

func (s *Server) addProject(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	if _, err := s.App.AddProject(r.Context(), sig.Path); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"path": ""})
}

func (s *Server) removeProject(w http.ResponseWriter, r *http.Request) {
	if err := s.App.RemoveProject(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// projectFor is the project a panel request is about, seen from the
// thread the page is on: when that thread works in a worktree, Path is
// the worktree, so the tree, the diffs, the editor and the PR list all
// look at the checkout the agent is changing. The thread comes from the
// tid signal every page sends (see the layout's data-signals).
func (s *Server) projectFor(r *http.Request) (store.Project, error) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		return p, err
	}
	var sig struct {
		TID string `json:"tid"`
	}
	datastar.ReadSignals(r, &sig)
	if sig.TID != "" {
		if t, err := s.App.Store.Thread(r.Context(), sig.TID); err == nil && t.ProjectID == p.ID {
			p.Path = t.Dir(p)
		}
	}
	return p, nil
}

// setProjectWorktrees flips the project's default for new threads.
func (s *Server) setProjectWorktrees(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.App.SetProjectWorktrees(r.Context(), p.ID, !p.Worktrees); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// newThreadInWorktree opens a thread on the same checkout as this one.
func (s *Server) newThreadInWorktree(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	agent := sig.Agent
	if agent == "" {
		agent = views.FirstOr(s.agentNames(), "")
	}
	id, err := s.App.CreateThreadIn(r.Context(), r.PathValue("id"), agent, sig.Model)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	mode := sig.modeOr(s.defaultMode(r.Context(), agent))
	if sig.Effort != "" || mode != "" {
		s.App.SetThreadSettings(r.Context(), id, app.ThreadSettings{Agent: agent, Model: sig.Model, Effort: sig.Effort, PermissionMode: mode})
	}
	sse := datastar.NewSSE(w, r)
	sse.Redirect("/threads/" + id)
}

// removeWorktree puts the thread back on the project's checkout.
func (s *Server) removeWorktree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.App.RemoveWorktree(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Term.KillPrefix(termPrefix(id))
	s.ok(w, r)
}

func (s *Server) projectFiles(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	s.patchDir(sse, p, r.URL.Query().Get("path"))
}

// patchDir renders one directory of the file tree: the whole explorer
// for the root, the children list of a folder node otherwise.
func (s *Server) patchDir(sse *datastar.ServerSentEventGenerator, p store.Project, path string) error {
	dir, entries, err := listProjectDir(p.Path, path)
	if err != nil {
		return sse.PatchElementTempl(views.Toast(err.Error()))
	}
	files := make([]views.ProjectFile, len(entries))
	for i, entry := range entries {
		files[i] = views.ProjectFile{Name: entry.Name, Path: entry.Path, IsDir: entry.IsDir}
	}
	d := views.ExplorerData{Project: p, Dir: dir, Files: files}
	if dir == "" {
		return sse.PatchElementTempl(views.FileExplorer(d))
	}
	return sse.PatchElementTempl(views.TreeChildren(d))
}

// uploadFiles saves browser-picked files (base64 in the "_upload" signal, put
// there by data-bind on the file input) into the explorer's current
// directory. The signal is cleared in every outcome, so a morphed explorer
// re-running the effect cannot post the same files again.
func (s *Server) uploadFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // decoded cap is checked in saveUploads
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var sig struct {
		Upload []UploadFile `json:"_upload"`
	}
	datastar.ReadSignals(r, &sig)
	if len(sig.Upload) == 0 {
		s.ok(w, r)
		return
	}
	saveErr := saveUploads(p.Path, r.URL.Query().Get("path"), sig.Upload)
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"_upload": []any{}})
	if saveErr != nil {
		s.Log.Warn("upload failed", "project", p.ID, "err", saveErr)
		sse.PatchElementTempl(views.Toast(saveErr.Error()))
		return
	}
	s.App.Bus.Publish(domain.GitChanged{ProjectID: p.ID})
	s.patchDir(sse, p, r.URL.Query().Get("path"))
}

func (s *Server) newThreadForProject(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	s.createThread(w, r, r.PathValue("id"), sig.Agent, sig.Model, sig.Effort, sig.Mode, "", nil, app.ThreadStart{})
}

func (s *Server) newThread(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // attachments ride the signals as base64
	sig := s.readSignals(r)
	s.createThread(w, r, sig.Project, sig.Agent, sig.Model, sig.Effort, sig.Mode, sig.Prompt, sig.Attach, app.ThreadStart{Mode: sig.WT, Base: sig.WTBase, ReuseFrom: sig.WTFrom})
}

// createThread makes a thread, applies settings, and when the home
// composer carried a prompt, sends it as the first turn before redirecting.
func (s *Server) createThread(w http.ResponseWriter, r *http.Request, projectID, agent, model, effort string, mode *string, prompt string, attach []UploadFile, start app.ThreadStart) {
	if agent == "" {
		agent = views.FirstOr(s.agentNames(), "")
		if agent == "" {
			s.fail(w, r, errors.New("no provider is enabled (see Settings > Providers)"))
			return
		}
	}
	if mode == nil {
		m := s.defaultMode(r.Context(), agent)
		mode = &m
	}
	id, err := s.App.CreateThread(r.Context(), projectID, agent, model, start)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if effort != "" || *mode != "" {
		if err := s.App.SetThreadSettings(r.Context(), id, app.ThreadSettings{Agent: agent, Model: model, Effort: effort, PermissionMode: *mode}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if strings.TrimSpace(prompt) != "" || len(attach) > 0 {
		prompt, err := s.attachToPrompt(id, prompt, attach)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.App.SendPrompt(r.Context(), id, prompt); err != nil {
			s.fail(w, r, err)
			return
		}
		s.App.SaveDraft(r.Context(), "home", "")
	}
	sse := datastar.NewSSE(w, r)
	sse.Redirect("/threads/" + id)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // attachments ride the signals as base64
	sig := s.readSignals(r)
	if strings.TrimSpace(sig.Prompt) == "" && len(sig.Attach) == 0 {
		s.ok(w, r)
		return
	}
	prompt, err := s.attachToPrompt(r.PathValue("id"), sig.Prompt, sig.Attach)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.App.SendPrompt(r.Context(), r.PathValue("id"), prompt); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": "", "_attach": []any{}})
}

// attachmentFile serves a stored attachment for transcript previews. Both
// path segments are flattened to base names, so the lookup cannot leave the
// attachments directory.
func (s *Server) attachmentFile(w http.ResponseWriter, r *http.Request) {
	tid := filepath.Base(r.PathValue("id"))
	name := filepath.Base(r.PathValue("name"))
	if s.AttachDir == "" || tid == "." || tid == ".." || name == "." || name == ".." {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.AttachDir, tid, name))
}

// attachToPrompt saves the composer's attachments for threadID and appends
// their paths to the prompt text. No attachments returns the text as is.
func (s *Server) attachToPrompt(threadID, text string, files []UploadFile) (string, error) {
	if len(files) == 0 {
		return text, nil
	}
	if s.AttachDir == "" {
		return "", errors.New("attachments are not configured")
	}
	paths, err := saveAttachments(filepath.Join(s.AttachDir, threadID), files)
	if err != nil {
		return "", err
	}
	return promptWithAttachments(text, files, paths), nil
}

func (s *Server) compactContext(w http.ResponseWriter, r *http.Request) {
	if err := s.App.CompactContext(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.App.Interrupt(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) removeQueuedPrompt(w http.ResponseWriter, r *http.Request) {
	if err := s.App.CancelQueuedPrompt(r.Context(), r.PathValue("id"), r.PathValue("qid")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) setThreadSettings(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	st := app.ThreadSettings{Agent: sig.Agent, Model: sig.Model, Effort: sig.Effort, PermissionMode: sig.modeOr("")}
	if err := s.App.SetThreadSettings(r.Context(), r.PathValue("id"), st); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) deleteThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DeleteThread(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Term.KillPrefix(termPrefix(r.PathValue("id")))
	if s.AttachDir != "" {
		os.RemoveAll(filepath.Join(s.AttachDir, r.PathValue("id")))
	}
	sse := datastar.NewSSE(w, r)
	sse.Redirect("/")
}

func (s *Server) archiveThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.ArchiveThread(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	// Out of sight is out of the process table too: the shells would sit
	// there until a restart otherwise. Unarchiving starts fresh ones.
	s.Term.KillPrefix(termPrefix(r.PathValue("id")))
	s.ok(w, r)
}

func (s *Server) revokeRule(w http.ResponseWriter, r *http.Request) {
	if err := s.App.RevokeRule(r.Context(), r.PathValue("id"), r.URL.Query().Get("key")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// saveDraft keeps what a composer holds, keyed by thread id or "home".
// Only the prompt signal is posted (the composer filters the rest out).
func (s *Server) saveDraft(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var sig struct {
		Prompt string `json:"prompt"`
	}
	datastar.ReadSignals(r, &sig)
	if err := s.App.SaveDraft(r.Context(), r.PathValue("key"), sig.Prompt); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) pinThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.PinThread(r.Context(), r.PathValue("id"), r.URL.Path[len(r.URL.Path)-4:] == "/pin"); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// setSidebarMode stores the sidebar's list order in a cookie, so it is
// a per-browser choice like the theme: "inbox" is one list with the
// newest activity on top, "project" groups threads under their project.
// Open sidebars redraw on the seen event, which already marks them dirty.
func (s *Server) setSidebarMode(w http.ResponseWriter, r *http.Request) {
	var sig struct {
		Sidebar string `json:"sidebar"`
	}
	datastar.ReadSignals(r, &sig)
	if sig.Sidebar != "inbox" && sig.Sidebar != "project" {
		s.fail(w, r, errors.New("unknown sidebar mode"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sidebar", Value: sig.Sidebar, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 365})
	// The page's stream read the cookie when it connected; reopening it
	// draws the sidebar in the new layout. Other pages pick it up on
	// their next connect.
	sse := datastar.NewSSE(w, r)
	sse.ExecuteScript("window.dispatchEvent(new Event('stream'))")
}

// setSettle stores the two settle settings for the instance: whether a
// merged pull request archives its thread, and after how many idle days
// a thread archives on its own. A new day count runs the sweep right
// away, so the sidebar shows the effect without waiting for the hour.
func (s *Server) setSettle(w http.ResponseWriter, r *http.Request) {
	var sig struct {
		SettleMerged bool `json:"settleMerged"`
		SettleDays   int  `json:"settleDays"`
	}
	datastar.ReadSignals(r, &sig)
	days := min(max(sig.SettleDays, 0), 365)
	ctx := r.Context()
	merged := "1"
	if !sig.SettleMerged {
		merged = "0"
	}
	if err := s.App.Store.SetSetting(ctx, "settle_merged", merged); err != nil {
		s.fail(w, r, err)
		return
	}
	before := s.App.Store.SettleIdleDays(ctx)
	if err := s.App.Store.SetSetting(ctx, "settle_idle_days", strconv.Itoa(days)); err != nil {
		s.fail(w, r, err)
		return
	}
	if days != before {
		go s.App.SettleIdle(context.Background())
	}
	s.ok(w, r)
}

// steer stops the running turn so the queued prompts start now; the
// button sits on the first queued message, which is what goes out next.
func (s *Server) steer(w http.ResponseWriter, r *http.Request) {
	if err := s.App.Interrupt(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) unarchiveThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.UnarchiveThread(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) renameThread(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	if err := s.App.RenameThread(r.Context(), r.PathValue("id"), sig.Title); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"rename": false})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	if err := s.App.ResolveApproval(r.Context(), r.PathValue("id"), r.PathValue("aid"), r.PathValue("decision")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) setTheme(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	for _, t := range Themes {
		if t == sig.Theme {
			http.SetCookie(w, &http.Cookie{Name: "theme", Value: t, Path: "/", Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 365})
			s.ok(w, r)
			return
		}
	}
	w.WriteHeader(http.StatusBadRequest)
}

func (s *Server) gitRefresh(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	d := views.GitData{Project: p, Status: gitx.Read(r.Context(), p.Path)}
	sse.PatchElementTempl(views.GitHeader(d))
	sse.PatchElementTempl(views.GitFiles(d))
}

func (s *Server) gitDiff(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := r.URL.Query().Get("path")
	if !insideProject(path) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"gitPath": path, "gitEdit": false, "_file": ""})
	sse.PatchElementTempl(views.GitDetail(views.GitData{
		Project:  p,
		Selected: path,
		Diff:     gitx.Diff(r.Context(), p.Path, path),
	}))
}

func (s *Server) gitFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := r.URL.Query().Get("path")
	content, err := readProjectFile(p.Path, path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"gitPath": path, "gitEdit": true, "_file": content})
	sse.PatchElementTempl(views.GitDetail(views.GitData{Project: p, Selected: path, Diff: gitx.Diff(r.Context(), p.Path, path), Editing: true}))
}

func (s *Server) saveGitFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := r.URL.Query().Get("path")
	if err := writeProjectFile(p.Path, path, s.readSignals(r).File); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.GitChanged{ProjectID: p.ID})
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"gitPath": path, "gitEdit": false, "_file": ""})
	sse.PatchElementTempl(views.GitDetail(views.GitData{Project: p, Selected: path, Diff: gitx.Diff(r.Context(), p.Path, path)}))
}

// insideProject rejects paths that could name a file outside the project:
// absolute ones and ones that climb out with "..". git confines tracked
// paths on its own, but the untracked fallback in gitx.Diff runs git diff
// --no-index, which would happily print any file on the machine.
func insideProject(path string) bool {
	clean := filepath.Clean(filepath.FromSlash(path))
	if path == "" || clean == "." || filepath.IsAbs(clean) {
		return false
	}
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
