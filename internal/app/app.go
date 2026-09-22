// Package app is the write side: every user action is a command here. A
// command validates input, appends events to the store, and returns. Agent
// sessions run in the background and feed their events through the same
// store, so the SSE read side only ever watches the bus.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"starcode/internal/agent"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
)

type App struct {
	Store *store.Store
	Bus   *bus.Bus
	Log   *slog.Logger

	// agents is the live registry, keyed by the name threads store in
	// their Agent field. Provider settings swap entries at runtime.
	agentsMu sync.RWMutex
	agents   map[string]agent.Agent

	mu       sync.Mutex
	sessions map[string]*live
	promptMu sync.Mutex // serializes send-vs-turn-complete queue handoffs

	// WorktreeRoot is where threads' own checkouts go (<data>/worktrees),
	// one directory per project; empty disables worktrees.
	WorktreeRoot string

	// KeepWorktree, when set, names checkouts the automatic cleanup must
	// leave alone whatever their state: the one this process runs its
	// binary from, say.
	KeepWorktree func(dir string) bool

	// idleTimeout is how long a session may sit between turns before
	// reapIdle closes its process; stop ends the sweeper.
	idleTimeout time.Duration
	stop        chan struct{}
	stopOnce    sync.Once
	// closing is set by Shutdown: from then on the pumps drop what the
	// dying sessions report, so a turn cut off by the shutdown still
	// reads as running when the next start looks (see Recover).
	closing atomic.Bool
	// cutOff are the threads Recover found cut off by a restart and left
	// for ResumeCutOff.
	cutOff []string
}

// idleTimeout is how long a thread's agent process stays alive after its
// turn ends. Every live process shares the CLI's credentials file, and when
// the OAuth access token runs out they all try to refresh it at once; the
// refresh token rotates under the losers and the CLI wipes the file, which
// every other thread then reports as "OAuth session expired and could not
// be refreshed". A process that stayed alive across the token's whole
// lifetime is the usual loser. Closing idle sessions keeps the number of
// contenders to the threads actually working; the next prompt resumes the
// session from its id at the cost of one process start.
const idleTimeout = 30 * time.Minute

// checkpointTimeout bounds the worktree checkpoint taken as a prompt
// goes out (see startPromptMeta).
const checkpointTimeout = 20 * time.Second

// idleSweep is how often reapIdle runs.
const idleSweep = time.Minute

// settleSweep is how often SettleIdle runs. The threshold is in days, so
// an hour is plenty.
const settleSweep = time.Hour

func New(st *store.Store, b *bus.Bus, agents map[string]agent.Agent, log *slog.Logger) *App {
	if agents == nil {
		agents = map[string]agent.Agent{}
	}
	a := &App{Store: st, Bus: b, agents: agents, Log: log, sessions: map[string]*live{}, idleTimeout: idleTimeout, stop: make(chan struct{})}
	st.Published = func(events []domain.Event) {
		msgs := make([]any, len(events))
		for i, e := range events {
			msgs[i] = e
		}
		b.Publish(msgs...)
	}
	return a
}

// Start runs the sweeps (idle sessions, snoozes, settling, worktree
// cleanup) until Shutdown. main calls it once everything the sweeps read
// is set: WorktreeRoot, KeepWorktree.
func (a *App) Start() {
	go a.reapLoop()
}

func (a *App) reapLoop() {
	tick := time.NewTicker(idleSweep)
	defer tick.Stop()
	settle := time.NewTicker(settleSweep)
	defer settle.Stop()
	a.SettleIdle(context.Background())
	a.CleanWorktrees(context.Background())
	for {
		select {
		case <-a.stop:
			return
		case now := <-tick.C:
			a.reapIdle(now)
			a.wakeDue(context.Background(), now)
		case <-settle.C:
			a.SettleIdle(context.Background())
			a.CleanWorktrees(context.Background())
		}
	}
}

// SettleIdle archives every thread that has had no activity for the
// number of days in the settle_idle_days setting, so the sidebar trims
// itself. Pinned and snoozed threads, threads mid-turn or waiting on an
// approval, and threads already archived are left alone; a setting of 0
// turns the sweep off. A thread brought back by hand counts from then
// (Thread.QuietSince), and any new activity unarchives it (see SendPrompt).
func (a *App) SettleIdle(ctx context.Context) {
	a.settleIdle(ctx, time.Now())
}

func (a *App) settleIdle(ctx context.Context, now time.Time) {
	days := a.Store.SettleIdleDays(ctx)
	if days <= 0 {
		return
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	ts, err := a.Store.Threads(ctx)
	if err != nil {
		a.Log.Warn("settle idle threads", "err", err)
		return
	}
	for _, t := range ts {
		if t.Archived || t.Pinned || t.Snoozed() || t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval || !t.QuietSince().Before(cutoff) {
			continue
		}
		if err := a.ArchiveThread(ctx, t.ID); err != nil {
			a.Log.Warn("settle idle thread", "thread", t.ID, "err", err)
			continue
		}
		a.Log.Debug("settled idle thread", "thread", t.ID, "idle_since", t.UpdatedAt)
	}
}

// reapIdle closes every session whose last turn ended idleTimeout or
// longer before now. promptMu is held from the check until the session
// is unregistered, so no prompt can start on it in between; the next
// prompt on that thread starts a fresh session that resumes by id. A
// thread mid-turn or waiting on an approval has no idle stamp and is
// left alone.
func (a *App) reapIdle(now time.Time) {
	a.promptMu.Lock()
	a.mu.Lock()
	var idle []*live
	for _, l := range a.sessions {
		l.mu.Lock()
		since := l.idleSince
		l.mu.Unlock()
		if !since.IsZero() && now.Sub(since) >= a.idleTimeout {
			idle = append(idle, l)
		}
	}
	a.mu.Unlock()
	ctx := context.Background()
	// Unregister under promptMu, so a prompt arriving next starts a fresh
	// session, but close outside it: closing waits on the agent process
	// and a prompt on any other thread should not wait with it.
	var closing []*live
	for _, l := range idle {
		if t, err := a.Store.Thread(ctx, l.threadID); err == nil && (t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval) {
			continue
		}
		a.mu.Lock()
		if a.sessions[l.threadID] == l {
			delete(a.sessions, l.threadID)
		}
		a.mu.Unlock()
		closing = append(closing, l)
	}
	a.promptMu.Unlock()
	for _, l := range closing {
		a.Log.Debug("closing idle session", "thread", l.threadID, "agent", l.agentName)
		l.sess.Close()
	}
}

// resumePrompt is what a turn cut off by a restart is resumed with.
const resumePrompt = "starcode restarted while you were working, which stopped your process in the middle of the turn. Carry on with the task from where you left off. A command or tool call that was running when it stopped did not finish; run it again if you still need its result."

// Recover is called once at startup. No session survives a restart, so any
// thread the projection still shows as busy is really idle. A turn the
// restart cut off is left as it is for ResumeCutOff to pick up (unless
// the resume_after_restart setting is off, or the thread never had a
// session to resume). Otherwise the thread goes idle with a note, and its
// next queued prompt starts, as it would have.
func (a *App) Recover(ctx context.Context) error {
	threads, err := a.Store.Threads(ctx)
	if err != nil {
		return err
	}
	resume := a.Store.ResumeAfterRestart(ctx)
	for _, t := range threads {
		queued, err := a.Store.QueuedPrompts(ctx, t.ID)
		if err != nil {
			return err
		}
		resumed := false
		if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
			var evs []any
			aps, _ := a.Store.PendingApprovals(ctx, t.ID)
			for _, ap := range aps {
				evs = append(evs, domain.ApprovalResolved{ID: ap.ID, Decision: domain.DecisionDeny, Auto: true})
			}
			_, agentOK := a.Agent(t.Agent)
			resumed = resume && agentOK && t.ExternalSessionID != ""
			if resumed {
				a.cutOff = append(a.cutOff, t.ID)
				continue
			}
			notice := "starcode restarted while this turn was running; send a new prompt to continue"
			if len(queued) > 0 {
				notice = "starcode restarted while this turn was running; continuing with the next queued prompt"
			}
			evs = append(evs,
				domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Body: notice},
				domain.ThreadStatusChanged{Status: domain.StatusIdle})
			if _, err := a.Store.Append(ctx, t.ID, evs...); err != nil {
				return err
			}
		}
		if len(queued) > 0 {
			a.promptMu.Lock()
			fresh, err := a.Store.Thread(ctx, t.ID)
			if err == nil {
				var l *live
				l, err = a.session(ctx, fresh)
				if err == nil {
					err = a.startPromptLocked(ctx, l, fresh, queued[0].ID, queued[0].Body)
				}
			}
			a.promptMu.Unlock()
			if err != nil {
				return fmt.Errorf("resume queued prompt: %w", err)
			}
		}
	}
	return nil
}

// ResumeCutOff resumes the turns Recover found cut off by a restart: the
// pending approvals are over, a note says what happened, and the session
// comes back from its id with a prompt that tells the agent. The queued
// prompts follow when that turn ends. main calls it once the server
// listens, so a start that fails early (the port taken) leaves the turns
// marked running for the next one rather than resuming them every time.
func (a *App) ResumeCutOff(ctx context.Context) {
	ids := a.cutOff
	a.cutOff = nil
	for _, id := range ids {
		var evs []any
		aps, _ := a.Store.PendingApprovals(ctx, id)
		for _, ap := range aps {
			evs = append(evs, domain.ApprovalResolved{ID: ap.ID, Decision: domain.DecisionDeny, Auto: true})
		}
		evs = append(evs,
			domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Body: "starcode restarted while this turn was running; resuming it"},
			domain.ThreadStatusChanged{Status: domain.StatusIdle})
		if _, err := a.Store.Append(ctx, id, evs...); err != nil {
			a.Log.Warn("resume after restart", "thread", id, "err", err)
			continue
		}
		a.promptMu.Lock()
		t, err := a.Store.Thread(ctx, id)
		if err == nil {
			var l *live
			l, err = a.session(ctx, t)
			if err == nil {
				err = a.startPromptMeta(ctx, l, t, "", resumePrompt, map[string]any{"auto": "resume"})
			}
		}
		a.promptMu.Unlock()
		if err != nil {
			a.Log.Warn("resume after restart", "thread", id, "err", err)
			a.note(ctx, id, "Could not resume the turn: "+err.Error())
		}
	}
}

// Agent looks up a registered agent by name.
func (a *App) Agent(name string) (agent.Agent, bool) {
	a.agentsMu.RLock()
	defer a.agentsMu.RUnlock()
	ag, ok := a.agents[name]
	return ag, ok
}

// AgentNames lists the registered agents, sorted.
func (a *App) AgentNames() []string {
	a.agentsMu.RLock()
	defer a.agentsMu.RUnlock()
	names := make([]string, 0, len(a.agents))
	for n := range a.agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// shutdowner is what the Codex adapter implements to stop its shared
// app-server; the Claude adapter has no process of its own.
type shutdowner interface{ Shutdown() error }

// SetAgent registers (or replaces) the agent behind name. Idle sessions on
// that name are closed so their next prompt runs on the new configuration;
// a running turn keeps its process. A replaced agent with nothing left
// running is shut down.
func (a *App) SetAgent(name string, ag agent.Agent) {
	a.agentsMu.Lock()
	old := a.agents[name]
	a.agents[name] = ag
	a.agentsMu.Unlock()
	a.retireAgent(name, old)
}

// RemoveAgent unregisters name; see SetAgent for what happens to sessions.
func (a *App) RemoveAgent(name string) {
	a.agentsMu.Lock()
	old := a.agents[name]
	delete(a.agents, name)
	a.agentsMu.Unlock()
	a.retireAgent(name, old)
}

func (a *App) retireAgent(name string, old agent.Agent) {
	if old == nil {
		return
	}
	ctx := context.Background()
	a.mu.Lock()
	var idle []string
	busy := 0
	for tid, l := range a.sessions {
		if l.agentName != name {
			continue
		}
		if t, err := a.Store.Thread(ctx, tid); err == nil && (t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval) {
			busy++
			continue
		}
		idle = append(idle, tid)
	}
	a.mu.Unlock()
	for _, tid := range idle {
		a.closeSession(tid)
	}
	if sd, ok := old.(shutdowner); ok && busy == 0 {
		if err := sd.Shutdown(); err != nil {
			a.Log.Warn("shut down replaced agent", "agent", name, "err", err)
		}
	}
}

// Shutdown closes every live session and stops agents that run a shared
// process. The pumps stop writing first, so the turns this cuts off stay
// marked running and Recover resumes them on the next start.
func (a *App) Shutdown() {
	a.closing.Store(true)
	a.stopOnce.Do(func() { close(a.stop) })
	a.mu.Lock()
	ls := make([]*live, 0, len(a.sessions))
	for _, l := range a.sessions {
		ls = append(ls, l)
	}
	a.mu.Unlock()
	for _, l := range ls {
		l.sess.Close()
	}
	a.agentsMu.RLock()
	defer a.agentsMu.RUnlock()
	for name, ag := range a.agents {
		if sd, ok := ag.(shutdowner); ok {
			if err := sd.Shutdown(); err != nil {
				a.Log.Warn("shut down agent", "agent", name, "err", err)
			}
		}
	}
}

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---- projects ----

func (a *App) AddProject(ctx context.Context, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	var err error
	path, err = expandProjectPath(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	path = filepath.Clean(path)
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", errors.New("path is not a directory")
	}
	existing, err := a.Store.Projects(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range existing {
		if p.Path == path {
			return p.ID, nil
		}
	}
	id := newID()
	_, err = a.Store.Append(ctx, "", domain.ProjectAdded{ID: id, Path: path, Name: filepath.Base(path)})
	return id, err
}

// expandProjectPath expands the current user's home shorthand. We deliberately
// do not implement shell expansion beyond this: project paths are not commands.
func expandProjectPath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func (a *App) RemoveProject(ctx context.Context, id string) error {
	threads, err := a.Store.Threads(ctx)
	if err != nil {
		return err
	}
	for _, t := range threads {
		if t.ProjectID == id {
			a.closeSession(t.ID)
		}
	}
	_, err = a.Store.Append(ctx, "", domain.ProjectRemoved{ID: id})
	return err
}

// ---- threads ----

// ThreadStart is how a new thread gets its checkout: the project's
// default, the project checkout, a new worktree from Base (empty for the
// repository's default branch), or the worktree of thread ReuseFrom.
type ThreadStart struct {
	Mode      string // "" (project default) | "local" | "worktree" | "reuse"
	Base      string
	ReuseFrom string
}

// CreateThread opens a thread on a project. start says where it works;
// the zero value takes the project's default. A reuse of a thread on
// another project, or of one without a worktree, is treated as "local".
func (a *App) CreateThread(ctx context.Context, projectID, agentName, model string, start ThreadStart) (string, error) {
	p, err := a.Store.Project(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("project: %w", err)
	}
	if _, ok := a.Agent(agentName); !ok {
		return "", fmt.Errorf("unknown agent %q", agentName)
	}
	var reuse store.Thread
	if start.Mode == "reuse" {
		src, err := a.Store.Thread(ctx, start.ReuseFrom)
		if err != nil {
			return "", fmt.Errorf("thread to reuse: %w", err)
		}
		if src.ProjectID == projectID {
			reuse = src
		}
	}
	id := newID()
	if _, err := a.Store.Append(ctx, id, domain.ThreadCreated{ID: id, ProjectID: projectID, Title: "new thread", Agent: agentName, Model: strings.TrimSpace(model)}); err != nil {
		return "", err
	}
	switch {
	case reuse.Worktree != "":
		// Same directory and branch, no git command (T3's "new thread in
		// this worktree"). A worktree two threads share is never removed
		// with one of them.
		if _, err := a.Store.Append(ctx, id, domain.ThreadWorktreeSet{Path: reuse.Worktree, Branch: reuse.WorktreeBranch}); err != nil {
			return "", err
		}
	case start.Mode == "worktree" || (start.Mode == "" && p.Worktrees):
		if a.WorktreeRoot == "" {
			break
		}
		if err := a.addWorktree(ctx, id, p, start.Base); err != nil {
			// The thread stands; it works on the main checkout and says why.
			a.note(ctx, id, "Could not make a worktree, working in "+p.Path+": "+err.Error())
		}
	}
	return id, nil
}

// CreateThreadIn opens a thread on the worktree another thread uses:
// CreateThread with Mode "reuse".
func (a *App) CreateThreadIn(ctx context.Context, from, agentName, model string) (string, error) {
	src, err := a.Store.Thread(ctx, from)
	if err != nil {
		return "", err
	}
	return a.CreateThread(ctx, src.ProjectID, agentName, model, ThreadStart{Mode: "reuse", ReuseFrom: from})
}

// note puts a line in the transcript that is not from the agent.
func (a *App) note(ctx context.Context, id, text string) {
	a.Store.Append(ctx, id, domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Body: text})
}

// addWorktree gives thread id a checkout under WorktreeRoot on a new
// branch from base, or from the repository's default branch when base is
// empty. The branch and directory carry the thread's short id: the title
// is not known yet, and a name that never changes is what a pushed
// branch wants.
func (a *App) addWorktree(ctx context.Context, id string, p store.Project, base string) error {
	if !gitx.Read(ctx, p.Path).IsRepo {
		return fmt.Errorf("%s is not a git repository", p.Path)
	}
	if base = strings.TrimSpace(base); base == "" {
		base = gitx.DefaultBranch(ctx, p.Path)
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	dir := filepath.Join(a.WorktreeRoot, gitx.Slug(p.Name), short)
	branch, err := gitx.AddWorktree(ctx, p.Path, dir, "starcode/"+short, base)
	if err != nil {
		return err
	}
	if _, err := a.Store.Append(ctx, id, domain.ThreadWorktreeSet{Path: dir, Branch: branch}); err != nil {
		return err
	}
	// The project's setup script (t3.json) runs in the new checkout; an
	// async one runs alongside the first turn, the other holds it.
	if sc, ok := gitx.ReadSetupScript(p.Path); ok {
		run := func() {
			out, err := gitx.RunSetup(context.Background(), sc, p.Path, dir)
			msg := "Setup script " + sc.Name + " finished"
			if err != nil {
				msg = "Setup script " + sc.Name + " failed: " + err.Error()
			}
			if out != "" {
				msg += "\n" + out
			}
			a.note(context.Background(), id, msg)
		}
		a.note(ctx, id, "Running the setup script from t3.json in "+dir+": "+sc.Command)
		if sc.Async {
			go run()
		} else {
			run()
		}
	}
	return nil
}

// temporaryBranch is whether the worktree still has the name it was
// made with, before the thread had a title.
func temporaryBranch(t store.Thread) bool {
	short := t.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return t.WorktreeBranch == "starcode/"+short || strings.HasPrefix(t.WorktreeBranch, "starcode/"+short+"-")
}

// nameWorktreeBranch gives the worktree's branch a name from the title
// once there is one, the way T3 renames its throwaway branch. Only the
// name made at creation is renamed, and only once.
func (a *App) nameWorktreeBranch(ctx context.Context, t store.Thread, title string) {
	if t.Worktree == "" || !temporaryBranch(t) {
		return
	}
	slug := gitx.Slug(title)
	if slug == "thread" || slug == "new-thread" {
		return
	}
	name, err := gitx.RenameBranch(ctx, t.Worktree, t.WorktreeBranch, "starcode/"+slug)
	if err != nil {
		a.Log.Info("worktree branch kept", "thread", t.ID, "err", err)
		return
	}
	a.Store.Append(ctx, t.ID, domain.ThreadWorktreeSet{Path: t.Worktree, Branch: name})
}

// sharedWorktree is whether another thread works in t's directory.
func (a *App) sharedWorktree(ctx context.Context, t store.Thread) bool {
	ts, err := a.Store.Threads(ctx)
	if err != nil {
		return true
	}
	for _, o := range ts {
		if o.ID != t.ID && o.Worktree == t.Worktree {
			return true
		}
	}
	return false
}

// SetProjectWorktrees sets whether the project's new threads start in a
// worktree of their own.
func (a *App) SetProjectWorktrees(ctx context.Context, id string, on bool) error {
	p, err := a.Store.Project(ctx, id)
	if err != nil {
		return err
	}
	if p.Worktrees == on {
		return nil
	}
	ps := settingsOf(p)
	ps.Worktrees = on
	return a.SetProjectSettings(ctx, id, ps)
}

// RemoveWorktree drops a thread's checkout and puts the thread back on
// the project's; an unpushed branch stays in the repository. Without
// force a checkout with changes is refused. With force its modified and
// untracked files go with it. Called by hand from the thread's menu.
func (a *App) RemoveWorktree(ctx context.Context, id string, force bool) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.Worktree == "" {
		return nil
	}
	p, err := a.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return err
	}
	if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
		return errors.New("a turn is running in this worktree")
	}
	a.closeSession(id)
	if a.sharedWorktree(ctx, t) {
		// Another thread works there; this one just steps off it.
		_, err = a.Store.Append(ctx, id, domain.ThreadWorktreeSet{Path: ""})
		return err
	}
	if err := gitx.RemoveWorktree(ctx, p.Path, t.Worktree, force); err != nil {
		return err
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadWorktreeSet{Path: ""})
	return err
}

func (a *App) RenameThread(ctx context.Context, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is required")
	}
	if t, err := a.Store.Thread(ctx, id); err == nil {
		a.nameWorktreeBranch(ctx, t, title)
	}
	_, err := a.Store.Append(ctx, id, domain.ThreadRenamed{Title: title})
	return err
}

// LinkPR ties the thread to a pull request. Linking the one it already
// has is a no-op, so the pump can call it after every gh output.
func (a *App) LinkPR(ctx context.Context, id, repo string, number int, url string) error {
	if repo == "" || number <= 0 {
		return errors.New("a repository and a pull request number are required")
	}
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.PRRepo == repo && t.PRNumber == number {
		return nil
	}
	if url == "" {
		url = gitx.PullURL(repo, number)
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadPRLinked{Repo: repo, Number: number, URL: url})
	return err
}

func (a *App) UnlinkPR(ctx context.Context, id string) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.PRNumber == 0 {
		return nil
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadPRUnlinked{})
	return err
}

// ThreadSettings is what SetThreadSettings accepts.
type ThreadSettings struct {
	Agent, Model, Effort, PermissionMode string
}

// SetThreadSettings changes how the thread's agent runs. The agent itself
// can only change before the first prompt (the transcript belongs to one
// agent's session). Model, effort and permission mode are flags of the
// agent process, so between turns the idle session is closed and the next
// turn resumes it with the new ones. During a turn the session stays: the
// permission mode is pushed into it when the agent takes that (see
// agent.ModeSetter), and whatever the agent cannot take live marks the
// session stale, so the next turn starts a fresh one. A note in the
// transcript says which of the two happened.
func (a *App) SetThreadSettings(ctx context.Context, id string, st ThreadSettings) error {
	if _, ok := a.Agent(st.Agent); !ok {
		return fmt.Errorf("unknown agent %q", st.Agent)
	}
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if st.Agent != t.Agent {
		items, err := a.Store.Items(ctx, id)
		if err != nil {
			return err
		}
		if len(items) > 0 {
			return errors.New("agent can only be changed before the first prompt")
		}
	}
	st.Model, st.Effort, st.PermissionMode = strings.TrimSpace(st.Model), strings.TrimSpace(st.Effort), strings.TrimSpace(st.PermissionMode)
	changed := domain.ThreadSettingsChanged{Agent: st.Agent, Model: st.Model, Effort: st.Effort, PermissionMode: st.PermissionMode}
	if t.Status != domain.StatusRunning && t.Status != domain.StatusAwaitingApproval {
		a.closeSession(id)
		_, err = a.Store.Append(ctx, id, changed)
		return err
	}

	a.mu.Lock()
	l := a.sessions[id]
	a.mu.Unlock()
	evs := []any{changed}
	stale := false
	note := func(text string) {
		evs = append(evs, domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Body: text})
	}
	if st.PermissionMode != t.PermissionMode {
		name := st.PermissionMode
		if name == "" {
			name = "default"
		}
		switch err := a.setLiveMode(ctx, l, st.PermissionMode); {
		case err == nil:
			note("permission mode is now " + name)
		case errors.Is(err, agent.ErrModeNextTurn):
			stale = true
			note("permission mode " + name + " applies from the next turn; this turn keeps its current mode")
		default:
			a.Log.Warn("set permission mode", "thread", id, "mode", st.PermissionMode, "err", err)
			stale = true
			note("permission mode " + name + " applies from the next turn (" + err.Error() + ")")
		}
	}
	if st.Model != t.Model || st.Effort != t.Effort {
		stale = true
		note("model and effort changes apply from the next turn")
	}
	if stale && l != nil {
		l.mu.Lock()
		l.stale = true
		l.mu.Unlock()
	}
	_, err = a.Store.Append(ctx, id, evs...)
	return err
}

// setLiveMode pushes a permission mode into a running session. Agents whose
// sessions cannot take one report agent.ErrModeNextTurn, as does a thread
// with no session behind it.
func (a *App) setLiveMode(ctx context.Context, l *live, mode string) error {
	if l == nil {
		return agent.ErrModeNextTurn
	}
	ms, ok := l.sess.(agent.ModeSetter)
	if !ok {
		return agent.ErrModeNextTurn
	}
	return ms.SetPermissionMode(ctx, mode)
}

func (a *App) DeleteThread(ctx context.Context, id string) error {
	a.closeSession(id)
	// A clean worktree goes with the thread; a dirty one is left on disk
	// with its branch, since deleting the thread should not lose work.
	// So this never forces.
	if t, err := a.Store.Thread(ctx, id); err == nil && t.Worktree != "" {
		if p, err := a.Store.Project(ctx, t.ProjectID); err == nil {
			if !a.sharedWorktree(ctx, t) {
				if err := gitx.RemoveWorktree(ctx, p.Path, t.Worktree, false); err != nil {
					a.Log.Info("worktree kept", "thread", id, "dir", t.Worktree, "err", err)
				}
			}
			gitx.DropCheckpoints(ctx, p.Path, id)
		}
	}
	_, err := a.Store.Append(ctx, id, domain.ThreadDeleted{})
	return err
}

// LimitsUpdate is bus-only: the windows an agent reported mid-turn for
// the instance named.
type LimitsUpdate struct {
	Agent  string
	Limits agent.Limits
}

// ArchiveThread hides a thread from the sidebar and the project cards. An
// idle session is closed so the agent process goes away; a running turn
// keeps going and the thread stays listed under Running until it ends.
func (a *App) ArchiveThread(ctx context.Context, id string) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.Archived {
		return nil
	}
	if t.Status != domain.StatusRunning && t.Status != domain.StatusAwaitingApproval {
		a.closeSession(id)
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadArchived{})
	return err
}

// PinThread keeps a thread at the top of its project's list; UnpinThread
// lets it fall back into date order.
func (a *App) PinThread(ctx context.Context, id string, pinned bool) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.Pinned == pinned {
		return nil
	}
	if pinned {
		_, err = a.Store.Append(ctx, id, domain.ThreadPinned{})
	} else {
		_, err = a.Store.Append(ctx, id, domain.ThreadUnpinned{})
	}
	return err
}

func (a *App) UnarchiveThread(ctx context.Context, id string) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if !t.Archived {
		return nil
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadUnarchived{})
	return err
}

// SendPrompt starts a turn when the thread is idle. While a turn or approval
// is active it persists the prompt in the per-thread FIFO instead, allowing
// the composer to accept follow-ups without disturbing transcript order.
func (a *App) SendPrompt(ctx context.Context, threadID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("prompt is empty")
	}
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		return err
	}
	queued, err := a.Store.QueuedPrompts(ctx, threadID)
	if err != nil {
		return err
	}
	if t.Archived {
		// A reply brings the thread back where it can be found.
		if _, err := a.Store.Append(ctx, threadID, domain.ThreadUnarchived{}); err != nil {
			return err
		}
	}
	if t.Snoozed() {
		if _, err := a.Store.Append(ctx, threadID, domain.ThreadWoken{}); err != nil {
			return err
		}
	}
	if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
		if _, err := a.Store.Append(ctx, threadID, domain.PromptQueued{ID: newID(), Body: text}); err != nil {
			return err
		}
		return a.Store.SaveDraft(ctx, threadID, "")
	}
	l, err := a.session(ctx, t)
	if err != nil {
		return err
	}
	a.Store.SaveDraft(ctx, threadID, "")
	// A queue can survive an agent process closing unexpectedly. Preserve
	// FIFO order: append this new follow-up, then restart the oldest one.
	if len(queued) > 0 {
		if _, err := a.Store.Append(ctx, threadID, domain.PromptQueued{ID: newID(), Body: text}); err != nil {
			return err
		}
		return a.startPromptLocked(ctx, l, t, queued[0].ID, queued[0].Body)
	}
	return a.startPromptLocked(ctx, l, t, "", text)
}

// CancelQueuedPrompt removes a follow-up which has not started yet.
func (a *App) CancelQueuedPrompt(ctx context.Context, threadID, promptID string) error {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	queued, err := a.Store.QueuedPrompts(ctx, threadID)
	if err != nil {
		return err
	}
	for _, q := range queued {
		if q.ID == promptID {
			_, err = a.Store.Append(ctx, threadID, domain.PromptDequeued{ID: promptID})
			return err
		}
	}
	return errors.New("queued prompt not found")
}

// startPromptLocked moves an optional queued prompt into the transcript and
// sends it. promptMu must be held so an arriving HTTP send cannot overtake a
// turn-completion handoff.
func (a *App) startPromptLocked(ctx context.Context, l *live, t store.Thread, queuedID, text string) error {
	return a.startPromptMeta(ctx, l, t, queuedID, text, nil)
}

// PromptMeta is what a prompt's item records about the moment it went
// out, for a later rewind to it: the agent session and the point to fork
// it at to keep everything before the prompt, and the checkpoint of the
// worktree's files. Auto marks a prompt starcode sent itself ("resume").
type PromptMeta struct {
	// Agent is the instance the session belongs to; a rewind across a
	// switch to another agent could not fork it.
	Agent      string `json:"agent,omitempty"`
	Session    string `json:"session,omitempty"`
	Anchor     string `json:"anchor,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
	Auto       string `json:"auto,omitempty"`
}

// promptMeta reads a prompt item's PromptMeta.
func promptMeta(it store.Item) PromptMeta {
	var m PromptMeta
	json.Unmarshal(it.Meta, &m)
	return m
}

// startPromptMeta is startPromptLocked with extra fields for the prompt
// item's meta.
func (a *App) startPromptMeta(ctx context.Context, l *live, t store.Thread, queuedID, text string, extra map[string]any) error {
	meta := map[string]any{}
	for k, v := range extra {
		meta[k] = v
	}
	if t.ExternalSessionID != "" {
		meta["agent"] = t.Agent
		meta["session"] = t.ExternalSessionID
		if t.Anchor != "" {
			meta["anchor"] = t.Anchor
		}
	}
	// A worktree of its own gets its files recorded, so a rewind to this
	// prompt can put them back. A shared one is left out: restoring it
	// would undo the other thread's work too. promptMu is held here, and
	// every thread's prompts wait on it, so a checkpoint that takes long
	// is given up: the prompt goes out without one.
	if t.Worktree != "" && !a.sharedWorktree(ctx, t) {
		if _, err := os.Stat(t.Worktree); err == nil {
			cctx, cancel := context.WithTimeout(ctx, checkpointTimeout)
			sha, err := gitx.Checkpoint(cctx, t.Worktree, t.ID)
			cancel()
			if err == nil {
				meta["checkpoint"] = sha
			} else {
				a.Log.Info("no checkpoint", "thread", t.ID, "err", err)
			}
		}
	}
	rawMeta, _ := json.Marshal(meta)
	if len(meta) == 0 {
		rawMeta = nil
	}
	// Serialize the initial fallback with native title notifications. If a
	// native title won the race, prompt text must not overwrite it.
	l.mu.Lock()
	var evs []any
	if queuedID != "" {
		evs = append(evs, domain.PromptDequeued{ID: queuedID})
	}
	evs = append(evs, domain.ItemStarted{ID: newID(), Kind: domain.KindUser, Body: text, Status: domain.ItemDone, Meta: rawMeta})
	if t.Title == "new thread" && !l.nativeTitle {
		evs = append(evs, domain.ThreadRenamed{Title: titleFrom(text)})
	}
	evs = append(evs, domain.ThreadStatusChanged{Status: domain.StatusRunning})
	if _, err := a.Store.Append(ctx, t.ID, evs...); err != nil {
		l.mu.Unlock()
		return err
	}
	l.idleSince = time.Time{}
	l.mu.Unlock()
	if err := l.sess.Send(ctx, text); err != nil {
		a.Store.Append(ctx, t.ID, domain.ThreadStatusChanged{Status: domain.StatusError, Detail: err.Error()})
		return err
	}
	return nil
}

// CompactContext has the agent fold the conversation so far into a
// summary, which frees most of the context window. It runs as a turn: the
// thread must be idle, and the session's own TurnStarted and TurnCompleted
// bracket it as they do a prompt, so the status, the meter and the prompt
// queue follow without more help. The session is started (or resumed) if
// there is none, since compacting is a thing to do before the next prompt.
func (a *App) CompactContext(ctx context.Context, threadID string) error {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		return err
	}
	if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
		return errors.New("wait for the current turn to finish")
	}
	l, err := a.session(ctx, t)
	if err != nil {
		return err
	}
	c, ok := l.sess.(agent.Compactor)
	if !ok {
		return fmt.Errorf("%s cannot compact its context", t.Agent)
	}
	if _, err := a.Store.Append(ctx, threadID,
		domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Body: "Compacting the context…"},
		domain.ThreadStatusChanged{Status: domain.StatusRunning}); err != nil {
		return err
	}
	l.mu.Lock()
	l.idleSince = time.Time{}
	l.mu.Unlock()
	if err := c.Compact(ctx); err != nil {
		a.Store.Append(ctx, threadID, domain.ThreadStatusChanged{Status: domain.StatusError, Detail: err.Error()})
		return err
	}
	return nil
}

func (a *App) Interrupt(ctx context.Context, threadID string) error {
	a.mu.Lock()
	l := a.sessions[threadID]
	a.mu.Unlock()
	if l == nil {
		// Nothing running; make sure the projection agrees.
		_, err := a.Store.Append(ctx, threadID, domain.ThreadStatusChanged{Status: domain.StatusIdle})
		return err
	}
	l.mu.Lock()
	l.interrupted = true
	l.mu.Unlock()
	return l.sess.Interrupt(ctx)
}

// ResolveApproval answers a pending approval. decision is one of the
// domain.Decision* constants.
func (a *App) ResolveApproval(ctx context.Context, threadID, approvalID, decision string) error {
	ap, err := a.Store.Approval(ctx, threadID, approvalID)
	if err != nil {
		return err
	}
	if ap.Decision != "" {
		return errors.New("already resolved")
	}
	a.mu.Lock()
	l := a.sessions[threadID]
	a.mu.Unlock()
	if l == nil {
		return errors.New("no live session for this thread")
	}
	d := agent.Deny
	switch decision {
	case domain.DecisionAllow:
		d = agent.Allow
	case domain.DecisionAllowSession:
		// The store turns the resolved event into a session rule.
		d = agent.Allow
	case domain.DecisionDeny:
	default:
		return fmt.Errorf("unknown decision %q", decision)
	}
	if err := l.sess.Resolve(ctx, approvalID, d); err != nil {
		return err
	}
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		return err
	}
	evs := []any{domain.ApprovalResolved{ID: approvalID, Decision: decision}}
	// Only a thread that is really waiting goes back to running; answering
	// a card whose turn has already ended must not fake a live turn.
	if t.Status == domain.StatusAwaitingApproval {
		evs = append(evs, domain.ThreadStatusChanged{Status: domain.StatusRunning})
	}
	_, err = a.Store.Append(ctx, threadID, evs...)
	return err
}

// SessionRules lists the "allow for session" rules active on a thread.
// They are a projection of the approval answers, so they survive a
// restart and a revoke is an event like the answer was.
func (a *App) SessionRules(ctx context.Context, threadID string) []string {
	rules, err := a.Store.SessionRules(ctx, threadID)
	if err != nil {
		a.Log.Warn("read session rules", "thread", threadID, "err", err)
	}
	return rules
}

// RevokeRule drops one session rule; the next matching request asks.
func (a *App) RevokeRule(ctx context.Context, threadID, key string) error {
	if !slices.Contains(a.SessionRules(ctx, threadID), key) {
		return errors.New("no such rule")
	}
	_, err := a.Store.Append(ctx, threadID, domain.RuleRevoked{Key: key})
	return err
}

// SaveDraft keeps composer text under key (a thread id, or "home") for
// every browser to find; see Store.SaveDraft for why it is not an event.
func (a *App) SaveDraft(ctx context.Context, key, body string) error {
	if key == "" {
		return errors.New("draft key is required")
	}
	return a.Store.SaveDraft(ctx, key, body)
}

// MarkSeen records that threadID is on someone's screen now. Other pages
// hear about it only when a row's unread mark goes away.
func (a *App) MarkSeen(ctx context.Context, threadID string) {
	changed, err := a.Store.MarkSeen(ctx, threadID, time.Now())
	if err != nil {
		a.Log.Warn("mark seen", "thread", threadID, "err", err)
		return
	}
	if changed {
		a.Bus.Publish(domain.SeenChanged{ThreadID: threadID})
	}
}

// Compact folds streamed deltas (see Store.Compact) and logs the count.
func (a *App) Compact(ctx context.Context, threadID string) {
	n, err := a.Store.Compact(ctx, threadID)
	if err != nil {
		a.Log.Warn("compact", "thread", threadID, "err", err)
	} else if n > 0 {
		a.Log.Debug("compacted", "thread", threadID, "rows", n)
	}
}

// ---- sessions ----

type live struct {
	threadID  string
	projectID string
	agentName string
	sess      agent.Session
	mu        sync.Mutex
	// stale is set when settings changed during a turn in a way the session
	// cannot take: it is retired when the turn ends and the next turn
	// starts a fresh session with the new flags.
	stale bool
	// ctxTokens and ctxWindow are the last context reading written to the
	// log, so a repeat costs nothing.
	ctxTokens, ctxWindow int64
	// idleSince is when the last turn ended, zero while one runs. reapIdle
	// closes the session once it has been set for idleTimeout.
	idleSince time.Time
	// turn-scoped bookkeeping
	interrupted bool
	turnID      string
	nativeTitle bool
	seenMsgs    map[string]string // agent message id -> item id
	seenTools   map[string]bool
	pending     map[string]agent.ApprovalRequested
}

func (a *App) session(ctx context.Context, t store.Thread) (*live, error) {
	a.mu.Lock()
	cur := a.sessions[t.ID]
	a.mu.Unlock()
	if cur != nil {
		cur.mu.Lock()
		stale := cur.stale
		cur.mu.Unlock()
		if !stale {
			return cur, nil
		}
		// Settings changed after its turn ended and before it was retired.
		a.retire(cur)
	}

	ag, ok := a.Agent(t.Agent)
	if !ok {
		return nil, fmt.Errorf("agent %q is not configured (see Settings > Providers)", t.Agent)
	}
	p, err := a.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return nil, err
	}
	if t.Worktree != "" && t.WorktreeBranch != "" {
		// A worktree deleted outside starcode comes back on its branch.
		if err := gitx.EnsureWorktree(ctx, p.Path, t.Worktree, t.WorktreeBranch); err != nil {
			a.note(ctx, t.ID, "The worktree "+t.Worktree+" is gone and could not be put back, working in "+p.Path+": "+err.Error())
		}
	}
	// Sessions outlive the request that started them.
	sess, err := ag.Start(context.Background(), agent.Config{Cwd: t.Dir(p), Model: t.Model, ResumeID: t.ExternalSessionID, ForkAt: t.ForkAt, PermissionMode: t.PermissionMode, Effort: t.Effort})
	if err != nil {
		return nil, err
	}
	l := &live{threadID: t.ID, projectID: t.ProjectID, agentName: t.Agent, sess: sess, seenMsgs: map[string]string{}, seenTools: map[string]bool{}, pending: map[string]agent.ApprovalRequested{}}
	a.mu.Lock()
	if existing := a.sessions[t.ID]; existing != nil {
		a.mu.Unlock()
		sess.Close()
		return existing, nil
	}
	a.sessions[t.ID] = l
	a.mu.Unlock()
	go a.pump(l)
	return l, nil
}

func (a *App) closeSession(threadID string) {
	a.mu.Lock()
	l := a.sessions[threadID]
	delete(a.sessions, threadID)
	a.mu.Unlock()
	if l != nil {
		l.sess.Close()
	}
}

// retire closes l and forgets it, unless the thread has moved on to another
// session already, which is then left alone.
func (a *App) retire(l *live) {
	a.mu.Lock()
	if a.sessions[l.threadID] == l {
		delete(a.sessions, l.threadID)
	}
	a.mu.Unlock()
	l.sess.Close()
}

// pump translates one session's agent events into domain events.
func (a *App) pump(l *live) {
	ctx := context.Background()
	tid := l.threadID
	log := a.Log.With("thread", tid)
	append_ := func(evs ...any) {
		if len(evs) == 0 {
			return
		}
		if _, err := a.Store.Append(ctx, tid, evs...); err != nil {
			log.Error("append", "err", err)
		}
	}
	itemID := func(agentID string) string { return tid[:8] + "-" + agentID }

	for e := range l.sess.Events() {
		if a.closing.Load() {
			// Shutting down: what a dying session says last (an error, a
			// closed turn) must not overwrite the running status Recover
			// resumes from.
			continue
		}
		switch e.Kind {
		case agent.KindSessionInfo:
			t, err := a.Store.Thread(ctx, tid)
			if err == nil && (t.ExternalSessionID != e.SessionInfo.ExternalID || (e.SessionInfo.Model != "" && t.ResolvedModel != e.SessionInfo.Model)) {
				append_(domain.AgentSessionBound{ExternalID: e.SessionInfo.ExternalID, Model: e.SessionInfo.Model})
			}
		case agent.KindThreadTitle:
			title := strings.TrimSpace(e.ThreadTitle.Title)
			l.mu.Lock()
			l.nativeTitle = title != ""
			l.mu.Unlock()
			t, err := a.Store.Thread(ctx, tid)
			if title != "" && err == nil && title != t.Title {
				append_(domain.ThreadRenamed{Title: title})
				a.nameWorktreeBranch(ctx, t, title)
			}
		case agent.KindTurnStarted:
			l.mu.Lock()
			l.turnID = e.TurnStarted.TurnID
			l.interrupted = false
			l.mu.Unlock()
			append_(domain.TurnStarted{TurnID: e.TurnStarted.TurnID}, domain.ThreadStatusChanged{Status: domain.StatusRunning})
		case agent.KindTextDelta:
			a.delta(l, append_, itemID, domain.KindAssistant, e.TextDelta.MessageID, e.TextDelta.Text, e.TextDelta.Parent)
		case agent.KindThinkingDelta:
			a.delta(l, append_, itemID, domain.KindThinking, e.ThinkingDelta.MessageID, e.ThinkingDelta.Text, e.ThinkingDelta.Parent)
		case agent.KindToolStarted:
			append_(a.closeMessages(l, "")...)
			ts := e.ToolStarted
			// parent names the subagent's tool call (a Task) this call
			// was made under; the transcript nests it there.
			meta, _ := json.Marshal(map[string]any{"input": nilIfEmpty(ts.Input), "summary": ts.Summary, "parent": nilIfBlank(itemIDOr(itemID, ts.Parent))})
			id := itemID(ts.ID)
			l.mu.Lock()
			seen := l.seenTools[ts.ID]
			l.seenTools[ts.ID] = true
			l.mu.Unlock()
			if seen {
				if len(ts.Input) > 0 || ts.Summary != "" {
					append_(domain.ItemCompleted{ID: id, Status: domain.ItemRunning, Meta: meta})
				}
			} else {
				append_(domain.ItemStarted{ID: id, Kind: domain.KindTool, ToolName: ts.Name, Status: domain.ItemRunning, Meta: meta})
			}
		case agent.KindToolOutput:
			append_(domain.ItemDelta{ID: itemID(e.ToolOutput.ID), Field: "output", Text: e.ToolOutput.Text})
		case agent.KindToolCompleted:
			tc := e.ToolCompleted
			var evs []any
			if tc.Output != "" {
				evs = append(evs, domain.ItemDelta{ID: itemID(tc.ID), Field: "output", Text: tc.Output})
			}
			status := tc.Status
			if status == "" {
				status = domain.ItemDone
			}
			if tc.IsError && status == domain.ItemDone {
				status = domain.ItemFailed
			}
			evs = append(evs, domain.ItemCompleted{ID: itemID(tc.ID), Status: status})
			append_(evs...)
			a.Bus.Publish(domain.GitChanged{ProjectID: l.projectID})
			// The agent opening a pull request links the thread to it.
			if it, err := a.Store.Item(ctx, itemID(tc.ID)); err == nil {
				if repo, n, url, ok := createdPR(it); ok {
					if err := a.LinkPR(ctx, tid, repo, n, url); err != nil {
						log.Error("link pull request", "err", err)
					}
				}
			}
		case agent.KindApproval:
			ap := *e.Approval
			auto := slices.Contains(a.SessionRules(ctx, tid), domain.RuleKey(ap.ToolName, ap.Input))
			req := domain.ApprovalRequested{ID: ap.ID, ItemID: itemID(ap.ToolID), ToolName: ap.ToolName, Description: ap.Description, Input: ap.Input}
			if auto {
				if err := l.sess.Resolve(ctx, ap.ID, agent.Allow); err != nil {
					log.Error("auto-resolve", "err", err)
					append_(req, domain.ThreadStatusChanged{Status: domain.StatusAwaitingApproval})
					continue
				}
				append_(req, domain.ApprovalResolved{ID: ap.ID, Decision: domain.DecisionAllowSession, Auto: true})
				continue
			}
			l.mu.Lock()
			l.pending[ap.ID] = ap
			l.mu.Unlock()
			append_(req, domain.ThreadStatusChanged{Status: domain.StatusAwaitingApproval})
		case agent.KindLimits:
			// Subscription windows are account state, not thread history;
			// the web layer keeps them per instance.
			if e.Limits != nil {
				a.Bus.Publish(LimitsUpdate{Agent: l.agentName, Limits: *e.Limits})
			}
		case agent.KindContextUsage:
			cu := e.ContextUsage
			l.mu.Lock()
			same := cu.Tokens == l.ctxTokens && cu.Window == l.ctxWindow
			l.ctxTokens, l.ctxWindow = cu.Tokens, cu.Window
			l.mu.Unlock()
			// Adapters may report the same reading more than once (Claude
			// splits one message across several lines); only a change is
			// worth a row in the log.
			if !same {
				append_(domain.ContextUsed{Tokens: cu.Tokens, Window: cu.Window})
			}
		case agent.KindTurnCompleted:
			tc := e.TurnCompleted
			l.mu.Lock()
			if l.interrupted && tc.Status == "done" {
				tc.Status = "interrupted"
			}
			var closers []any
			for _, id := range l.seenMsgs {
				closers = append(closers, domain.ItemCompleted{ID: id, Status: domain.ItemDone})
			}
			l.seenMsgs = map[string]string{}
			// seenTools is per turn like seenMsgs: a tool id from last turn
			// must not turn this turn's start into an update, and the map
			// would otherwise grow for the life of the session.
			l.seenTools = map[string]bool{}
			l.pending = map[string]agent.ApprovalRequested{}
			l.idleSince = time.Now()
			l.mu.Unlock()
			append_(closers...)
			status := domain.StatusIdle
			detail := ""
			if tc.Status == "error" {
				status = domain.StatusError
				detail = tc.Error
			}
			body := fmt.Sprintf("%s in %s", tc.Status, (time.Duration(tc.DurationMS) * time.Millisecond).Round(100*time.Millisecond))
			if tc.CostUSD > 0 {
				body += fmt.Sprintf(" · $%.4f", tc.CostUSD)
			}
			if tc.InputTokens > 0 || tc.OutputTokens > 0 {
				body += fmt.Sprintf(" · %s in / %s out", kilo(tc.InputTokens), kilo(tc.OutputTokens))
			}
			if tc.Error != "" {
				body += "\n" + tc.Error
			}
			a.promptMu.Lock()
			queued, queueErr := a.Store.QueuedPrompts(ctx, tid)
			if queueErr != nil {
				log.Error("read prompt queue", "err", queueErr)
			}
			// An approval the turn never got an answer for (Stop was pressed,
			// or the agent gave up) is over with the turn. Left pending, its
			// card would come back on every reload and answering it would
			// resolve a request nobody is waiting on.
			var completed []any
			if aps, err := a.Store.PendingApprovals(ctx, tid); err == nil {
				for _, ap := range aps {
					completed = append(completed, domain.ApprovalResolved{ID: ap.ID, Decision: domain.DecisionDeny, Auto: true})
				}
			}
			completed = append(completed,
				domain.ItemStarted{ID: newID(), Kind: domain.KindResult, Status: tc.Status, Body: body},
				domain.TurnCompleted{TurnID: tc.TurnID, Anchor: tc.Anchor, Status: tc.Status, DurationMS: tc.DurationMS, CostUSD: tc.CostUSD, InputTok: tc.InputTokens, OutputTok: tc.OutputTokens, Error: tc.Error},
			)
			if len(queued) == 0 {
				completed = append(completed, domain.ThreadStatusChanged{Status: status, Detail: detail})
			}
			append_(completed...)
			// Settings that changed during the turn want a fresh session;
			// a queued prompt starts on that one instead of this.
			l.mu.Lock()
			stale := l.stale
			l.mu.Unlock()
			next := l
			if stale {
				a.retire(l)
				next = nil
			}
			if len(queued) > 0 {
				t, err := a.Store.Thread(ctx, tid)
				if err == nil && next == nil {
					next, err = a.session(ctx, t)
				}
				if err != nil {
					log.Error("start queued prompt", "err", err)
					append_(domain.ThreadStatusChanged{Status: domain.StatusError, Detail: err.Error()})
				} else if err := a.startPromptLocked(ctx, next, t, queued[0].ID, queued[0].Body); err != nil {
					log.Error("start queued prompt", "err", err)
				}
			}
			a.promptMu.Unlock()
			a.Bus.Publish(domain.GitChanged{ProjectID: l.projectID})
		case agent.KindNotice:
			append_(domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Body: e.Notice.Text})
		case agent.KindClosed:
			a.mu.Lock()
			current := a.sessions[tid] == l
			if current {
				delete(a.sessions, tid)
			}
			a.mu.Unlock()
			t, err := a.Store.Thread(ctx, tid)
			if err != nil {
				continue
			}
			busy := t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval
			if e.Closed.Err != nil {
				append_(domain.ItemStarted{ID: newID(), Kind: domain.KindError, Body: "agent exited: " + e.Closed.Err.Error()},
					domain.ThreadStatusChanged{Status: domain.StatusError, Detail: e.Closed.Err.Error()})
			} else if !current && busy {
				// A retired session going away while its successor runs
				// the next turn; that turn's status stands.
			} else if t.Status != domain.StatusIdle {
				append_(domain.ThreadStatusChanged{Status: domain.StatusIdle})
			}
		}
	}
}

func (a *App) delta(l *live, append_ func(...any), itemID func(string) string, kind, msgID, text, parent string) {
	key := kind + ":" + msgID
	l.mu.Lock()
	id, ok := l.seenMsgs[key]
	if !ok {
		id = itemID(kind[:1] + "-" + msgID)
		l.seenMsgs[key] = id
	}
	l.mu.Unlock()
	if !ok {
		// A new message means the previous streamed one is finished.
		evs := a.closeMessages(l, id)
		var meta json.RawMessage
		if parent != "" {
			// A subagent's text sits under its Task in the transcript.
			meta, _ = json.Marshal(map[string]any{"parent": itemID(parent)})
		}
		evs = append(evs, domain.ItemStarted{ID: id, Kind: kind, Status: domain.ItemRunning, Body: text, Meta: meta})
		append_(evs...)
		return
	}
	append_(domain.ItemDelta{ID: id, Text: text})
}

// itemIDOr maps an agent-side tool id to its item id, keeping "" as "".
func itemIDOr(itemID func(string) string, id string) string {
	if id == "" {
		return ""
	}
	return itemID(id)
}

// nilIfBlank keeps an empty string out of the meta JSON.
func nilIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// closeMessages marks every streamed message item except keep as done and
// forgets it, so a later delta for it would start a fresh item. Streaming
// adapters do not send an explicit end for text or thinking blocks; the
// next block starting is the signal.
func (a *App) closeMessages(l *live, keep string) []any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var evs []any
	for key, id := range l.seenMsgs {
		if id == keep {
			continue
		}
		evs = append(evs, domain.ItemCompleted{ID: id, Status: domain.ItemDone})
		delete(l.seenMsgs, key)
	}
	return evs
}

// createdPR reads the pull request a finished tool call opened: a command
// with `gh pr create` in it whose output has the new PR's URL. Reading a
// PR (`gh pr view`) prints URLs too, which is why the command is checked.
func createdPR(it store.Item) (repo string, number int, url string, ok bool) {
	if it.Kind != domain.KindTool || it.Status != domain.ItemDone {
		return "", 0, "", false
	}
	var m struct {
		Input json.RawMessage `json:"input"`
	}
	json.Unmarshal(it.Meta, &m)
	if !strings.Contains(string(m.Input), "pr create") {
		return "", 0, "", false
	}
	return gitx.FindPullURL(it.Output)
}

func titleFrom(text string) string {
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	r := []rune(line)
	if len(r) > 60 {
		return string(r[:57]) + "..."
	}
	return line
}

func kilo(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func nilIfEmpty(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return r
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
