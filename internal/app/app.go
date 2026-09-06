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
	"sort"
	"strings"
	"sync"
	"time"

	"starcode/internal/agent"
	"starcode/internal/bus"
	"starcode/internal/domain"
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
	// rules holds per-thread "allow for session" decisions: thread id ->
	// rule key -> true. See ruleKey.
	rules map[string]map[string]bool
}

func New(st *store.Store, b *bus.Bus, agents map[string]agent.Agent, log *slog.Logger) *App {
	if agents == nil {
		agents = map[string]agent.Agent{}
	}
	a := &App{Store: st, Bus: b, agents: agents, Log: log, sessions: map[string]*live{}, rules: map[string]map[string]bool{}}
	st.Published = func(events []domain.Event) {
		msgs := make([]any, len(events))
		for i, e := range events {
			msgs[i] = e
		}
		b.Publish(msgs...)
	}
	return a
}

// Recover is called once at startup. No session survives a restart, so any
// thread the projection still shows as busy is really idle.
func (a *App) Recover(ctx context.Context) error {
	threads, err := a.Store.Threads(ctx)
	if err != nil {
		return err
	}
	for _, t := range threads {
		queued, err := a.Store.QueuedPrompts(ctx, t.ID)
		if err != nil {
			return err
		}
		if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
			var evs []any
			aps, _ := a.Store.PendingApprovals(ctx, t.ID)
			for _, ap := range aps {
				evs = append(evs, domain.ApprovalResolved{ID: ap.ID, Decision: domain.DecisionDeny, Auto: true})
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
// process.
func (a *App) Shutdown() {
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

func (a *App) CreateThread(ctx context.Context, projectID, agentName, model string) (string, error) {
	if _, err := a.Store.Project(ctx, projectID); err != nil {
		return "", fmt.Errorf("project: %w", err)
	}
	if _, ok := a.Agent(agentName); !ok {
		return "", fmt.Errorf("unknown agent %q", agentName)
	}
	id := newID()
	_, err := a.Store.Append(ctx, id, domain.ThreadCreated{ID: id, ProjectID: projectID, Title: "new thread", Agent: agentName, Model: strings.TrimSpace(model)})
	return id, err
}

func (a *App) RenameThread(ctx context.Context, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is required")
	}
	_, err := a.Store.Append(ctx, id, domain.ThreadRenamed{Title: title})
	return err
}

// ThreadSettings is what SetThreadSettings accepts.
type ThreadSettings struct {
	Agent, Model, Effort, PermissionMode string
}

// SetThreadSettings changes how the thread's agent runs. The agent itself
// can only change before the first prompt (the transcript belongs to one
// agent's session); model, effort and permission mode apply from the next
// turn, which is why the idle session is closed and later resumed with the
// new flags.
func (a *App) SetThreadSettings(ctx context.Context, id string, st ThreadSettings) error {
	if _, ok := a.Agent(st.Agent); !ok {
		return fmt.Errorf("unknown agent %q", st.Agent)
	}
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
		return errors.New("wait for the current turn to finish")
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
	a.closeSession(id)
	_, err = a.Store.Append(ctx, id, domain.ThreadSettingsChanged{
		Agent: st.Agent, Model: strings.TrimSpace(st.Model), Effort: strings.TrimSpace(st.Effort), PermissionMode: strings.TrimSpace(st.PermissionMode),
	})
	return err
}

func (a *App) DeleteThread(ctx context.Context, id string) error {
	a.closeSession(id)
	_, err := a.Store.Append(ctx, id, domain.ThreadDeleted{})
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
	if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
		_, err := a.Store.Append(ctx, threadID, domain.PromptQueued{ID: newID(), Body: text})
		return err
	}
	l, err := a.session(ctx, t)
	if err != nil {
		return err
	}
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
	// Serialize the initial fallback with native title notifications. If a
	// native title won the race, prompt text must not overwrite it.
	l.mu.Lock()
	var evs []any
	if queuedID != "" {
		evs = append(evs, domain.PromptDequeued{ID: queuedID})
	}
	evs = append(evs, domain.ItemStarted{ID: newID(), Kind: domain.KindUser, Body: text, Status: domain.ItemDone})
	if t.Title == "new thread" && !l.nativeTitle {
		evs = append(evs, domain.ThreadRenamed{Title: titleFrom(text)})
	}
	evs = append(evs, domain.ThreadStatusChanged{Status: domain.StatusRunning})
	if _, err := a.Store.Append(ctx, t.ID, evs...); err != nil {
		l.mu.Unlock()
		return err
	}
	l.mu.Unlock()
	if err := l.sess.Send(ctx, text); err != nil {
		a.Store.Append(ctx, t.ID, domain.ThreadStatusChanged{Status: domain.StatusError, Detail: err.Error()})
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
		d = agent.Allow
		a.mu.Lock()
		if a.rules[threadID] == nil {
			a.rules[threadID] = map[string]bool{}
		}
		a.rules[threadID][ruleKey(ap.ToolName, ap.Input)] = true
		a.mu.Unlock()
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
func (a *App) SessionRules(threadID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for k := range a.rules[threadID] {
		out = append(out, k)
	}
	return out
}

// ruleKey reduces an approval to something worth remembering: Bash commands
// by their first word, everything else by tool name.
func ruleKey(tool string, input json.RawMessage) string {
	if tool == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		json.Unmarshal(input, &in)
		if f := strings.Fields(in.Command); len(f) > 0 {
			return "Bash:" + f[0]
		}
	}
	return tool
}

// ---- sessions ----

type live struct {
	threadID  string
	agentName string
	sess      agent.Session
	mu        sync.Mutex
	// turn-scoped bookkeeping
	interrupted bool
	turnID      string
	nativeTitle bool
	seenMsgs    map[string]string // agent message id -> item id
	seenTools   map[string]bool
	toolOutput  map[string]bool // tool id -> streamed output seen
	pending     map[string]agent.ApprovalRequested
}

func (a *App) session(ctx context.Context, t store.Thread) (*live, error) {
	a.mu.Lock()
	if l := a.sessions[t.ID]; l != nil {
		a.mu.Unlock()
		return l, nil
	}
	a.mu.Unlock()

	ag, ok := a.Agent(t.Agent)
	if !ok {
		return nil, fmt.Errorf("agent %q is not configured (see Settings > Providers)", t.Agent)
	}
	p, err := a.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return nil, err
	}
	// Sessions outlive the request that started them.
	sess, err := ag.Start(context.Background(), agent.Config{Cwd: p.Path, Model: t.Model, ResumeID: t.ExternalSessionID, PermissionMode: t.PermissionMode, Effort: t.Effort})
	if err != nil {
		return nil, err
	}
	l := &live{threadID: t.ID, agentName: t.Agent, sess: sess, seenMsgs: map[string]string{}, seenTools: map[string]bool{}, toolOutput: map[string]bool{}, pending: map[string]agent.ApprovalRequested{}}
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
			a.delta(l, append_, itemID, domain.KindAssistant, e.TextDelta.MessageID, e.TextDelta.Text)
		case agent.KindThinkingDelta:
			a.delta(l, append_, itemID, domain.KindThinking, e.ThinkingDelta.MessageID, e.ThinkingDelta.Text)
		case agent.KindToolStarted:
			append_(a.closeMessages(l, "")...)
			ts := e.ToolStarted
			meta, _ := json.Marshal(map[string]any{"input": nilIfEmpty(ts.Input), "summary": ts.Summary})
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
			l.mu.Lock()
			l.toolOutput[e.ToolOutput.ID] = true
			l.mu.Unlock()
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
			a.Bus.Publish(domain.GitChanged{})
		case agent.KindApproval:
			ap := *e.Approval
			a.mu.Lock()
			auto := a.rules[tid][ruleKey(ap.ToolName, ap.Input)]
			a.mu.Unlock()
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
			l.pending = map[string]agent.ApprovalRequested{}
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
				domain.TurnCompleted{TurnID: tc.TurnID, Status: tc.Status, DurationMS: tc.DurationMS, CostUSD: tc.CostUSD, InputTok: tc.InputTokens, OutputTok: tc.OutputTokens, Error: tc.Error},
			)
			if len(queued) == 0 {
				completed = append(completed, domain.ThreadStatusChanged{Status: status, Detail: detail})
			}
			append_(completed...)
			if len(queued) > 0 {
				t, err := a.Store.Thread(ctx, tid)
				if err != nil {
					log.Error("load thread for queued prompt", "err", err)
				} else if err := a.startPromptLocked(ctx, l, t, queued[0].ID, queued[0].Body); err != nil {
					log.Error("start queued prompt", "err", err)
				}
			}
			a.promptMu.Unlock()
			a.Bus.Publish(domain.GitChanged{})
		case agent.KindNotice:
			append_(domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Body: e.Notice.Text})
		case agent.KindClosed:
			a.mu.Lock()
			if a.sessions[tid] == l {
				delete(a.sessions, tid)
			}
			a.mu.Unlock()
			t, err := a.Store.Thread(ctx, tid)
			if err != nil {
				continue
			}
			if e.Closed.Err != nil {
				append_(domain.ItemStarted{ID: newID(), Kind: domain.KindError, Body: "agent exited: " + e.Closed.Err.Error()},
					domain.ThreadStatusChanged{Status: domain.StatusError, Detail: e.Closed.Err.Error()})
			} else if t.Status != domain.StatusIdle {
				append_(domain.ThreadStatusChanged{Status: domain.StatusIdle})
			}
		}
	}
}

func (a *App) delta(l *live, append_ func(...any), itemID func(string) string, kind, msgID, text string) {
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
		evs = append(evs, domain.ItemStarted{ID: id, Kind: kind, Status: domain.ItemRunning, Body: text})
		append_(evs...)
		return
	}
	append_(domain.ItemDelta{ID: id, Text: text})
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
