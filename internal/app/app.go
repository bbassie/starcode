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
	"maps"
	"os"
	"path/filepath"
	"regexp"
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

	// AttachDir is where prompt attachments are saved, one directory per
	// thread; a handoff writes the conversation it hands over there.
	AttachDir string

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
	// bg counts the work started after a turn ends (recordTurnFiles),
	// which tests wait for.
	bg sync.WaitGroup

	// Tools gives a thread's agent the starcode MCP server (main sets it
	// up; see internal/mcp). Nil, or the agent_tools setting off, leaves
	// the agents without it.
	Tools func(threadID string) *agent.MCPServer
	// kids tracks spawned threads for their parents (see children.go).
	kids kids
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

// Start runs the sweeps (idle sessions, snoozes, settling) until
// Shutdown.
func (a *App) Start() {
	go a.reapLoop()
}

func (a *App) reapLoop() {
	tick := time.NewTicker(idleSweep)
	defer tick.Stop()
	settle := time.NewTicker(settleSweep)
	defer settle.Stop()
	prs := time.NewTicker(prWatchEvery)
	defer prs.Stop()
	a.SettleIdle(context.Background())
	for {
		select {
		case <-a.stop:
			return
		case now := <-prs.C:
			// gh can take seconds per PR; the other sweeps do not wait.
			go a.watchPRs(context.Background(), now)
		case now := <-tick.C:
			a.reapIdle(now)
			a.wakeDue(context.Background(), now)
			a.runSchedules(context.Background(), now)
		case <-settle.C:
			a.SettleIdle(context.Background())
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
	// With settle_keep_pr on, a thread waiting on review stays. A PR the
	// poll has not read yet counts as open.
	var prs map[store.PR]store.PRState
	if a.Store.SettleKeepPR(ctx) {
		if prs, err = a.Store.PRStates(ctx); err != nil {
			a.Log.Warn("settle idle threads", "err", err)
			return
		}
	}
	for _, t := range ts {
		if t.Archived || t.Pinned || t.Snoozed() || t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval || !t.QuietSince().Before(cutoff) {
			continue
		}
		if pr, ok := t.Linked(); ok && prs != nil {
			if st := prs[pr].State; st != "merged" && st != "closed" {
				continue
			}
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
// thread mid-turn has no idle stamp, and one waiting on an approval is
// left alone too. A background subagent with a tool running stretches
// the wait to four times idleTimeout.
func (a *App) reapIdle(now time.Time) {
	a.promptMu.Lock()
	a.mu.Lock()
	var idle []*live
	for _, l := range a.sessions {
		l.mu.Lock()
		since := l.idleSince
		busy := len(l.subTools) > 0
		l.mu.Unlock()
		// A subagent tool that never reported back must not pin the
		// process for good, so a busy session gets a longer wait, not
		// an endless one.
		timeout := a.idleTimeout
		if busy {
			timeout *= 4
		}
		if !since.IsZero() && now.Sub(since) >= timeout {
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
			a.warn(ctx, id, "Could not resume the turn: "+err.Error())
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
	// A turn's changed files still being worked out go in before the
	// store closes.
	a.bg.Wait()
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

// CreateThread opens a thread on a project. It works in the project's
// checkout.
func (a *App) CreateThread(ctx context.Context, projectID, agentName, model string) (string, error) {
	if _, err := a.Store.Project(ctx, projectID); err != nil {
		return "", fmt.Errorf("project: %w", err)
	}
	if _, ok := a.Agent(agentName); !ok {
		return "", fmt.Errorf("unknown agent %q", agentName)
	}
	id := newID()
	if _, err := a.Store.Append(ctx, id, domain.ThreadCreated{ID: id, ProjectID: projectID, Title: "new thread", Agent: agentName, Model: strings.TrimSpace(model)}); err != nil {
		return "", err
	}
	return id, nil
}

// note puts a line in the transcript that is not from the agent, for the
// record: it does not count as activity (domain.ItemStarted Quiet).
func (a *App) note(ctx context.Context, id, text string) {
	a.Store.Append(ctx, id, domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Body: text, Quiet: true})
}

// warn is a note that wants the reader: something failed that they may
// have to act on, so the thread counts as active and goes unread.
func (a *App) warn(ctx context.Context, id, text string) {
	a.Store.Append(ctx, id, domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Body: text})
}

func (a *App) RenameThread(ctx context.Context, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is required")
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
		return a.startPromptMeta(ctx, l, t, queued[0].ID, queued[0].Body, nil)
	}
	return a.startPromptMeta(ctx, l, t, "", text, nil)
}

// CompactAndSend compacts the conversation first and sends text after
// the summary: the compaction runs as a turn, so the prompt waits in the
// queue and goes out when it ends. A thread that cannot compact now (a
// turn running, an agent without it) just gets the prompt.
func (a *App) CompactAndSend(ctx context.Context, threadID, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("prompt is empty")
	}
	if err := a.CompactContext(ctx, threadID); err != nil {
		a.Log.Debug("compact before send", "thread", threadID, "err", err)
	}
	return a.SendPrompt(ctx, threadID, text)
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

// EditQueuedPrompt changes the text of a prompt still in the queue.
func (a *App) EditQueuedPrompt(ctx context.Context, threadID, promptID, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return a.CancelQueuedPrompt(ctx, threadID, promptID)
	}
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	if _, err := a.queuedIndex(ctx, threadID, promptID); err != nil {
		return err
	}
	_, err := a.Store.Append(ctx, threadID, domain.PromptEdited{ID: promptID, Body: body})
	return err
}

// MoveQueuedPrompt moves a queued prompt by delta places, negative
// being earlier; a delta past either end stops there.
func (a *App) MoveQueuedPrompt(ctx context.Context, threadID, promptID string, delta int) error {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	return a.moveQueuedLocked(ctx, threadID, promptID, delta)
}

func (a *App) moveQueuedLocked(ctx context.Context, threadID, promptID string, delta int) error {
	i, err := a.queuedIndex(ctx, threadID, promptID)
	if err != nil {
		return err
	}
	queued, _ := a.Store.QueuedPrompts(ctx, threadID)
	j := min(max(i+delta, 0), len(queued)-1)
	if j == i {
		return nil
	}
	ids := make([]string, 0, len(queued))
	for _, q := range queued {
		if q.ID != promptID {
			ids = append(ids, q.ID)
		}
	}
	ids = slices.Insert(ids, j, promptID)
	_, err = a.Store.Append(ctx, threadID, domain.PromptsReordered{IDs: ids})
	return err
}

func (a *App) queuedIndex(ctx context.Context, threadID, promptID string) (int, error) {
	queued, err := a.Store.QueuedPrompts(ctx, threadID)
	if err != nil {
		return 0, err
	}
	for i, q := range queued {
		if q.ID == promptID {
			return i, nil
		}
	}
	return 0, errors.New("queued prompt not found")
}

// Steer sends text now: on a busy thread it goes to the front of the
// queue and the running turn is stopped, so it is the next thing the
// agent reads; on an idle one it is an ordinary send.
func (a *App) Steer(ctx context.Context, threadID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("prompt is empty")
	}
	a.promptMu.Lock()
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		a.promptMu.Unlock()
		return err
	}
	if !busyThread(t) {
		a.promptMu.Unlock()
		return a.SendPrompt(ctx, threadID, text)
	}
	id := newID()
	if _, err := a.Store.Append(ctx, threadID, domain.PromptQueued{ID: id, Body: text}); err != nil {
		a.promptMu.Unlock()
		return err
	}
	a.Store.SaveDraft(ctx, threadID, "")
	err = a.moveQueuedLocked(ctx, threadID, id, -1<<30)
	a.promptMu.Unlock()
	if err != nil {
		return err
	}
	return a.Interrupt(ctx, threadID)
}

// startPromptLocked moves an optional queued prompt into the transcript and
// sends it. promptMu must be held so an arriving HTTP send cannot overtake a
// turn-completion handoff.
func (a *App) startPromptLocked(ctx context.Context, l *live, t store.Thread, queuedID, text string) error {
	return a.startPromptMeta(ctx, l, t, queuedID, text, nil)
}

// PromptMeta is what a prompt's item records about the moment it went
// out, for a later rewind to it: the agent session and the point to fork
// it at to keep everything before the prompt. Auto marks a prompt
// starcode sent itself ("resume").
type PromptMeta struct {
	// Agent is the instance the session belongs to; a rewind across a
	// switch to another agent could not fork it.
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	Anchor  string `json:"anchor,omitempty"`
	Auto    string `json:"auto,omitempty"`
	// Fresh is a prompt that began a conversation (no session yet): a
	// rewind to it starts a fresh one again.
	Fresh bool `json:"fresh,omitempty"`
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
	} else {
		meta["fresh"] = true
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
	send := l.sess.Send
	if is, ok := l.sess.(agent.ImageSender); ok {
		if images := imagePaths(text); len(images) > 0 {
			send = func(ctx context.Context, text string) error { return is.SendImages(ctx, text, images) }
		}
	}
	if err := send(ctx, text); err != nil {
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

// StopSubagent stops the subagent that item itemID (a Task tool call)
// runs, and leaves the turn going.
func (a *App) StopSubagent(ctx context.Context, threadID, itemID string) error {
	a.mu.Lock()
	l := a.sessions[threadID]
	a.mu.Unlock()
	if l == nil {
		return agent.ErrNoTask
	}
	ts, ok := l.sess.(agent.TaskStopper)
	if !ok {
		return errors.New("this agent cannot stop one subagent; stop the turn instead")
	}
	// Item ids are the agent's tool id behind the thread's prefix (pump).
	return ts.StopTask(ctx, strings.TrimPrefix(itemID, threadID[:min(8, len(threadID))]+"-"))
}

// ResolveApproval answers a pending approval. decision is one of the
// domain.Decision* constants.
func (a *App) ResolveApproval(ctx context.Context, threadID, approvalID, decision string) error {
	ap, sess, err := a.pendingApproval(ctx, threadID, approvalID)
	if err != nil {
		return err
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
	// Allowing a question without answers lets the tool run with none, and
	// the model hears that the user did not answer.
	if d == agent.Allow && domain.Questions(ap.ToolName, ap.Input) != nil {
		return errors.New("pick an answer to each question first")
	}
	if err := sess.Resolve(ctx, approvalID, d); err != nil {
		return err
	}
	return a.approvalResolved(ctx, threadID, domain.ApprovalResolved{ID: approvalID, Decision: decision})
}

// AnswerQuestions answers a pending approval that puts questions to the
// human (see domain.Questions). answers maps each question's text to the
// answer, and every question needs one.
func (a *App) AnswerQuestions(ctx context.Context, threadID, approvalID string, answers map[string]string) error {
	ap, sess, err := a.pendingApproval(ctx, threadID, approvalID)
	if err != nil {
		return err
	}
	qs := domain.Questions(ap.ToolName, ap.Input)
	if qs == nil {
		return errors.New("this approval asks no questions")
	}
	for _, q := range qs {
		if strings.TrimSpace(answers[q.Question]) == "" {
			return fmt.Errorf("pick or write an answer to %q", q.Question)
		}
	}
	an, ok := sess.(agent.Answerer)
	if !ok {
		return errors.New("this agent cannot take answers")
	}
	if err := an.Answer(ctx, approvalID, answers); err != nil {
		return err
	}
	return a.approvalResolved(ctx, threadID, domain.ApprovalResolved{ID: approvalID, Decision: domain.DecisionAllow, Answers: answers})
}

// pendingApproval returns an unanswered approval and the live session
// that waits for its answer.
func (a *App) pendingApproval(ctx context.Context, threadID, approvalID string) (store.Approval, agent.Session, error) {
	ap, err := a.Store.Approval(ctx, threadID, approvalID)
	if err != nil {
		return ap, nil, err
	}
	if ap.Decision != "" {
		return ap, nil, errors.New("already resolved")
	}
	a.mu.Lock()
	l := a.sessions[threadID]
	a.mu.Unlock()
	if l == nil {
		return ap, nil, errors.New("no live session for this thread")
	}
	return ap, l.sess, nil
}

// approvalResolved records an answer the session has already been given.
func (a *App) approvalResolved(ctx context.Context, threadID string, res domain.ApprovalResolved) error {
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		return err
	}
	evs := []any{res}
	// Only a thread that is really waiting moves on, and only once nothing
	// else waits: to running mid-turn, or back to idle when the card was a
	// background subagent's after the turn ended. Answering a card whose
	// turn is over must not fake a live turn.
	if t.Status == domain.StatusAwaitingApproval && !a.othersPending(ctx, threadID, res.ID) {
		next := domain.StatusRunning
		a.mu.Lock()
		l := a.sessions[threadID]
		a.mu.Unlock()
		if l != nil {
			l.mu.Lock()
			if !l.idleSince.IsZero() {
				next = domain.StatusIdle
			}
			l.mu.Unlock()
		}
		evs = append(evs, domain.ThreadStatusChanged{Status: next})
	}
	_, err = a.Store.Append(ctx, threadID, evs...)
	return err
}

// othersPending says whether the thread has an unanswered approval other
// than id.
func (a *App) othersPending(ctx context.Context, threadID, id string) bool {
	aps, err := a.Store.PendingApprovals(ctx, threadID)
	if err != nil {
		return false
	}
	for _, ap := range aps {
		if ap.ID != id {
			return true
		}
	}
	return false
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

// MarkUnread puts the unread mark back on a thread, for one to come back
// to. Opening it marks it read again.
func (a *App) MarkUnread(ctx context.Context, threadID string) error {
	if _, err := a.Store.Thread(ctx, threadID); err != nil {
		return err
	}
	if err := a.Store.MarkUnread(ctx, threadID); err != nil {
		return err
	}
	a.Bus.Publish(domain.SeenChanged{ThreadID: threadID})
	return nil
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
	// idleSince is when the last turn ended, zero while one runs. Anything
	// the session says between turns (a background subagent at work, a
	// turn the agent started itself) moves it up. reapIdle closes the
	// session once it has been set for idleTimeout.
	idleSince time.Time
	// subTools holds the item ids of a subagent's tool calls that have not
	// finished. A background subagent goes on after the main turn ends;
	// while one has a tool running the session is not idle, and its
	// approvals outlive the turn.
	subTools map[string]bool
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
	cfg := agent.Config{Cwd: p.Path, Model: t.Model, ResumeID: t.ExternalSessionID, ForkAt: t.ForkAt, PermissionMode: t.PermissionMode, Effort: t.Effort}
	if a.Tools != nil && a.Store.AgentTools(ctx) {
		cfg.Tools = a.Tools(t.ID)
	}
	// Sessions outlive the request that started them.
	sess, err := ag.Start(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	l := &live{threadID: t.ID, projectID: t.ProjectID, agentName: t.Agent, sess: sess, seenMsgs: map[string]string{}, seenTools: map[string]bool{}, subTools: map[string]bool{}, pending: map[string]agent.ApprovalRequested{}}
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
		if e.Kind != agent.KindLimits {
			l.mu.Lock()
			if !l.idleSince.IsZero() {
				l.idleSince = time.Now()
			}
			l.mu.Unlock()
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
			if ts.Parent != "" {
				l.subTools[id] = true
			}
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
			l.mu.Lock()
			delete(l.subTools, itemID(tc.ID))
			l.mu.Unlock()
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
			// A question always waits for the human; no rule can answer it.
			auto := domain.Questions(ap.ToolName, ap.Input) == nil && slices.Contains(a.SessionRules(ctx, tid), domain.RuleKey(ap.ToolName, ap.Input))
			req := domain.ApprovalRequested{ID: ap.ID, ItemID: itemID(ap.ToolID), ToolName: ap.ToolName, Description: ap.Description, Reason: ap.Reason, Input: ap.Input}
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
			// A stopped turn takes its subagents with it; one that ended on
			// its own may have left some working in the background.
			if tc.Status == "interrupted" {
				clear(l.subTools)
			}
			maps.DeleteFunc(l.pending, func(_ string, ap agent.ApprovalRequested) bool { return !l.subTools[itemID(ap.ToolID)] })
			subTools := maps.Clone(l.subTools)
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
			// resolve a request nobody is waiting on. A background
			// subagent's request is the exception: the subagent still
			// waits for it, and the thread waits with it.
			var completed []any
			waiting := false
			if aps, err := a.Store.PendingApprovals(ctx, tid); err == nil {
				for _, ap := range aps {
					if subTools[ap.ItemID] {
						waiting = true
						continue
					}
					completed = append(completed, domain.ApprovalResolved{ID: ap.ID, Decision: domain.DecisionDeny, Auto: true})
				}
			}
			if waiting && status == domain.StatusIdle {
				status = domain.StatusAwaitingApproval
			}
			resultID := newID()
			completed = append(completed,
				domain.ItemStarted{ID: resultID, Kind: domain.KindResult, Status: tc.Status, Body: body},
				domain.TurnCompleted{TurnID: tc.TurnID, Anchor: tc.Anchor, Status: tc.Status, DurationMS: tc.DurationMS, CostUSD: tc.CostUSD, InputTok: tc.InputTokens, OutputTok: tc.OutputTokens, Error: tc.Error},
			)
			// A turn stopped by the plan's usage limit waits for the reset
			// on the Snoozed shelf and resumes then (see wakeDue). A queued
			// prompt would only hit the same limit, so it waits too.
			limited := tc.Status == "error" && !tc.LimitReset.IsZero() && a.Store.ResumeAfterLimit(ctx)
			if len(queued) == 0 || limited {
				completed = append(completed, domain.ThreadStatusChanged{Status: status, Detail: detail})
			}
			if limited {
				completed = append(completed, domain.ThreadSnoozed{Until: limitWake(tc.LimitReset, time.Now()), Resume: true},
					domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Body: "The plan's usage limit stopped this turn. The thread waits on the Snoozed shelf until the limit resets, and then starcode resumes the turn. Waking it or sending a prompt before then cancels that."})
			}
			append_(completed...)
			// What the turn changed goes on its result line; git may take
			// a moment on a big tree, so the line shows first.
			a.bg.Add(1)
			go func() {
				defer a.bg.Done()
				a.recordTurnFiles(tid, resultID)
			}()
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
			if len(queued) > 0 && !limited {
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
			if len(queued) == 0 || limited {
				a.childDone(tid)
			}
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

// attachedImage matches the line the web layer adds for an attached image
// (web.promptWithAttachments).
var attachedImage = regexp.MustCompile(`(?m)^\[Attached image ".*" is saved at: (.+)\]$`)

// imagePaths are the images attached to a prompt, which go to the agent
// as images where it takes them.
func imagePaths(text string) []string {
	var out []string
	for _, m := range attachedImage.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}
