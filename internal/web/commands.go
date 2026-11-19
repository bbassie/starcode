package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/agent"
	"starcode/internal/app"
	"starcode/internal/domain"
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
	// Appearance is the theme's half; see Server.appearanceMode.
	Appearance string `json:"appearance"`
	Effort     string `json:"effort"`
	// Mode is nil when the page carried no mode signal at all (a "new
	// thread" hotkey on the settings page, say), which is different from
	// the agent's default mode picked on purpose.
	Mode   *string      `json:"mode"`
	Title  string       `json:"title"`
	Attach []UploadFile `json:"_attach"`
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

// newThreadForProject is the new-thread buttons and hotkey. The page's
// composer settings ride along, so a new thread carries the model of the
// one on screen, unless the project names its own (Settings > Projects).
func (s *Server) newThreadForProject(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	if p, err := s.App.Store.Project(r.Context(), r.PathValue("id")); err == nil && p.HasDefaults() {
		if _, ok := s.App.Agent(p.Agent); ok {
			mode := p.Mode
			sig.Agent, sig.Model, sig.Effort, sig.Mode = p.Agent, p.Model, p.Effort, &mode
		}
	}
	s.createThread(w, r, r.PathValue("id"), sig.Agent, sig.Model, sig.Effort, sig.Mode, "", nil)
}

func (s *Server) newThread(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // attachments ride the signals as base64
	sig := s.readSignals(r)
	s.createThread(w, r, sig.Project, sig.Agent, sig.Model, sig.Effort, sig.Mode, sig.Prompt, sig.Attach)
}

// createThread makes a thread, applies settings, and when the home
// composer carried a prompt, sends it as the first turn before redirecting.
func (s *Server) createThread(w http.ResponseWriter, r *http.Request, projectID, agent, model, effort string, mode *string, prompt string, attach []UploadFile) {
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
	// No project: a folder of the thread's own (App.CreateScratchThread).
	var id string
	var err error
	if projectID == "" {
		id, err = s.App.CreateScratchThread(r.Context(), agent, model, prompt)
	} else {
		id, err = s.App.CreateThread(r.Context(), projectID, agent, model)
	}
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
		s.sent.mark("home", prompt)
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
	s.sent.mark(r.PathValue("id"), sig.Prompt)
	prompt, err := s.attachToPrompt(r.PathValue("id"), sig.Prompt, sig.Attach)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// busy says what a send does while a turn runs: into the turn
	// (steer), after it (queue, the default), or first after stopping it.
	send := s.App.SendPrompt
	switch {
	case r.URL.Query().Get("busy") == "steer":
		send = s.App.SteerPrompt
	case r.URL.Query().Get("busy") == "stop":
		send = s.App.StopAndSend
	case r.URL.Query().Get("compact") == "1":
		send = s.App.CompactAndSend
	}
	if err := send(r.Context(), r.PathValue("id"), prompt); err != nil {
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

func (s *Server) stopSubagent(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StopSubagent(r.Context(), r.PathValue("id"), r.PathValue("item")); err != nil {
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

// editQueuedPrompt saves the queue row's edit box; empty text removes
// the prompt.
func (s *Server) editQueuedPrompt(w http.ResponseWriter, r *http.Request) {
	var sig struct {
		Body string `json:"qbody"`
	}
	datastar.ReadSignals(r, &sig)
	if err := s.App.EditQueuedPrompt(r.Context(), r.PathValue("id"), r.PathValue("qid"), sig.Body); err != nil {
		s.fail(w, r, err)
		return
	}
	datastar.NewSSE(w, r).MarshalAndPatchSignals(map[string]any{"qedit": "", "qbody": ""})
}

// moveQueuedPrompt is the queue row's up and down buttons (by=-1, 1).
func (s *Server) moveQueuedPrompt(w http.ResponseWriter, r *http.Request) {
	by, err := strconv.Atoi(r.URL.Query().Get("by"))
	if err != nil {
		s.fail(w, r, errors.New("by is a number of places"))
		return
	}
	if err := s.App.MoveQueuedPrompt(r.Context(), r.PathValue("id"), r.PathValue("qid"), by); err != nil {
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
	if isUndo(r) {
		s.ok(w, r)
		return
	}
	id := r.PathValue("id")
	s.undoToast(w, r, "Settled “"+s.threadTitle(r, id)+"”", time.Time{}, "@post('/api/threads/"+id+"/unarchive?undo=1')")
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
// A draft that is the text just sent from that composer is dropped: the
// draft post fires on a pause in typing and can land after the send
// cleared the draft (a slow connection, or Enter pressed within the
// pause), which would bring the sent message back on the next page load.
func (s *Server) saveDraft(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var sig struct {
		Prompt string `json:"prompt"`
	}
	datastar.ReadSignals(r, &sig)
	if s.sent.was(r.PathValue("key"), sig.Prompt) {
		s.ok(w, r)
		return
	}
	if err := s.App.SaveDraft(r.Context(), r.PathValue("key"), sig.Prompt); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) pinThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pin := strings.HasSuffix(r.URL.Path, "/pin")
	if err := s.App.PinThread(r.Context(), id, pin); err != nil {
		s.fail(w, r, err)
		return
	}
	if pin || isUndo(r) {
		s.ok(w, r)
		return
	}
	s.undoToast(w, r, "Unpinned “"+s.threadTitle(r, id)+"”", time.Time{}, "@post('/api/threads/"+id+"/pin?undo=1')")
}

// markUnread puts the unread mark back on a thread. From the thread's own
// page (?leave=1) it goes home too, or the open page would mark it read
// again on the next event.
func (s *Server) markUnread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.MarkUnread(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	if r.URL.Query().Get("leave") == "1" {
		datastar.NewSSE(w, r).Redirect("/")
		return
	}
	s.ok(w, r)
}

// setSidebarMode stores the sidebar's list order and its density in two
// cookies, so they are per-browser choices like the theme: "inbox" is
// one list with the newest activity on top, "project" groups threads
// under their project, and dense is "1" or "0". A request without the
// sidebar signal keeps the mode the cookie holds. Open sidebars redraw
// on the seen event, which already marks them dirty.
func (s *Server) setSidebarMode(w http.ResponseWriter, r *http.Request) {
	var sig struct {
		Sidebar string `json:"sidebar"`
		Dense   bool   `json:"dense"`
	}
	datastar.ReadSignals(r, &sig)
	if sig.Sidebar == "" {
		if c, err := r.Cookie("sidebar"); err == nil {
			sig.Sidebar = c.Value
		}
	}
	dense := "0"
	if sig.Dense {
		dense = "1"
	}
	switch sig.Sidebar {
	case "inbox", "project":
		http.SetCookie(w, &http.Cookie{Name: "sidebar", Value: sig.Sidebar, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 365})
	case "":
		// No signal and no cookie yet: the page's default mode stays.
	default:
		s.fail(w, r, errors.New("unknown sidebar mode"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "dense", Value: dense, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 365})
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
		SettleKeepPR bool `json:"settleKeepPr"`
		Resume       bool `json:"resume"`
		ResumeLimit  bool `json:"resumeLimit"`
		AgentTools   bool `json:"agentTools"`
	}
	datastar.ReadSignals(r, &sig)
	days := min(max(sig.SettleDays, 0), 365)
	ctx := r.Context()
	flag := func(on bool) string {
		if on {
			return "1"
		}
		return "0"
	}
	for k, v := range map[string]string{
		"settle_merged":        flag(sig.SettleMerged),
		"settle_keep_pr":       flag(sig.SettleKeepPR),
		"resume_after_restart": flag(sig.Resume),
		"resume_after_limit":   flag(sig.ResumeLimit),
		"agent_tools":          flag(sig.AgentTools),
	} {
		if err := s.App.Store.SetSetting(ctx, k, v); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	before := s.App.Store.SettleIdleDays(ctx)
	keptBefore := s.App.Store.SettleKeepPR(ctx)
	if err := s.App.Store.SetSetting(ctx, "settle_idle_days", strconv.Itoa(days)); err != nil {
		s.fail(w, r, err)
		return
	}
	if days != before || (keptBefore && !sig.SettleKeepPR) {
		go s.App.SettleIdle(context.Background())
	}
	s.ok(w, r)
}

// steerQueuedPrompt is "now" on a queued message: into the running turn
// when the agent can take it, else the turn stops so it goes next.
func (s *Server) steerQueuedPrompt(w http.ResponseWriter, r *http.Request) {
	if err := s.App.SteerQueued(r.Context(), r.PathValue("id"), r.PathValue("qid")); err != nil {
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

// retitleThread has the thread's agent write a title from the
// conversation; the rename box closes when it lands.
func (s *Server) retitleThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.RetitleThread(r.Context(), r.PathValue("id")); err != nil {
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

// answer takes the question card's form: field qN holds the picked
// option labels of question N, qN-other what the human wrote.
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	tid, aid := r.PathValue("id"), r.PathValue("aid")
	ap, err := s.App.Store.Approval(r.Context(), tid, aid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.App.AnswerQuestions(r.Context(), tid, aid, formAnswers(domain.Questions(ap.ToolName, ap.Input), r.PostForm)); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// formAnswers turns the question card's fields into the answer of each
// question. Picked labels join with ", " as Claude Code's own picker does.
// Written text is the whole answer to a single choice question and one
// more item of a multiple choice one.
func formAnswers(qs []domain.Question, form url.Values) map[string]string {
	out := make(map[string]string, len(qs))
	for i, q := range qs {
		key := "q" + strconv.Itoa(i)
		var picked []string
		for _, v := range form[key] {
			if v = strings.TrimSpace(v); v != "" {
				picked = append(picked, v)
			}
		}
		if other := strings.TrimSpace(form.Get(key + "-other")); other != "" {
			if !q.MultiSelect {
				picked = nil
			}
			picked = append(picked, other)
		}
		if len(picked) > 0 {
			out[q.Question] = strings.Join(picked, ", ")
		}
	}
	return out
}

// setTheme saves the theme and its appearance; the page has already
// switched to them.
func (s *Server) setTheme(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	if !slices.Contains(Themes, sig.Theme) || !slices.Contains(Appearances, sig.Appearance) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for name, v := range map[string]string{"theme": sig.Theme, "appearance": sig.Appearance} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: v, Path: "/", Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 365})
	}
	s.ok(w, r)
}

// sentPrompts is the last text sent from each composer (a thread id, or
// "home"), kept so a late draft post cannot save it again. It lives in
// memory only: a restart forgets it, and the race it guards is seconds
// long. An entry stays until the next send from that composer, so a
// draft typed to be exactly the last message is not kept; that costs
// little next to a sent message coming back.
type sentPrompts struct {
	mu   sync.Mutex
	last map[string]string
}

func (p *sentPrompts) mark(key, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		p.last = map[string]string{}
	}
	p.last[key] = strings.TrimSpace(text)
}

func (p *sentPrompts) was(key, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last[key] == text
}
