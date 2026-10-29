// Package fake is a scripted agent used for UI development and tests. Every
// Send replays the same little scenario: some thinking, a line of
// narration, a tool call that needs approval (except in the yolo mode),
// streamed prose, a result. A
// prompt with "ask me" in it first puts a question to the human, the way
// Claude Code's AskUserQuestion does, in every mode.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"starcode/internal/agent"
)

// fakeWindow is the context limit the scripted agent pretends to have. It
// is small on purpose: a handful of turns fills the meter, which is what
// makes it worth looking at while working on the UI.
const fakeWindow = 100_000

type Agent struct {
	// Delay between streamed chunks. Zero means as fast as possible.
	Delay time.Duration
}

func New() *Agent { return &Agent{Delay: 40 * time.Millisecond} }

func (a *Agent) Name() string { return "fake" }

func (a *Agent) Capabilities(ctx context.Context) (agent.Capabilities, error) {
	return agent.Capabilities{
		Models: []agent.Model{
			{ID: "fake-1", DisplayName: "Fake 1", Default: true, Efforts: []string{"low", "high"}, ContextWindow: fakeWindow},
			{ID: "fake-1-long", DisplayName: "Fake 1 (1M context)", Description: "Same model, room for ten times as much", Efforts: []string{"low", "high"}, ContextWindow: 1_000_000},
		},
		Efforts:         []agent.Choice{{ID: "", Label: "default"}, {ID: "low", Label: "low"}, {ID: "high", Label: "high"}},
		PermissionModes: []agent.Choice{{ID: "", Label: "default"}, {ID: "yolo", Label: "yolo"}},
	}, nil
}

func (a *Agent) Provider(ctx context.Context) (agent.ProviderInfo, error) {
	yes := true
	return agent.ProviderInfo{Binary: "built in", Version: "0.0.0", LoggedIn: &yes, Account: "no account needed"}, nil
}

// Ask answers a one-off prompt without a model: a title-shaped line made
// from the last "Me:" line of the prompt, so retitling can be tried in
// the UI for free.
func (a *Agent) Ask(ctx context.Context, prompt string) (string, error) {
	last := "the conversation"
	for _, l := range strings.Split(prompt, "\n") {
		if t, ok := strings.CutPrefix(l, "Me: "); ok && strings.TrimSpace(t) != "" {
			last = strings.TrimSpace(t)
		}
	}
	if r := []rune(last); len(r) > 40 {
		last = string(r[:40])
	}
	return "Fake title: " + last, nil
}

func (a *Agent) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &session{
		a:       a,
		cfg:     cfg,
		ctx:     ctx,
		cancel:  cancel,
		events:  make(chan agent.Event, 256),
		decided: make(chan agent.Decision, 1),
		answers: make(chan map[string]string, 1),
		stop:    make(chan struct{}, 1),
	}
	id := cfg.ResumeID
	if id == "" || cfg.ForkAt != "" {
		// A new conversation, or a rewind's copy of an old one.
		id = fmt.Sprintf("fake-%d", time.Now().UnixNano())
	}
	s.emit(agent.Event{Kind: agent.KindSessionInfo, SessionInfo: &agent.SessionInfo{ExternalID: id, Model: "fake-1"}})
	go func() {
		<-ctx.Done()
		s.closeOnce.Do(func() {
			s.emit(agent.Event{Kind: agent.KindClosed, Closed: &agent.Closed{}})
			s.emitMu.Lock()
			s.closed = true
			close(s.events)
			s.emitMu.Unlock()
		})
	}()
	return s, nil
}

type session struct {
	a         *Agent
	cfg       agent.Config
	ctx       context.Context
	cancel    context.CancelFunc
	events    chan agent.Event
	decided   chan agent.Decision
	answers   chan map[string]string
	stop      chan struct{}
	mu        sync.Mutex
	turn      int
	ctxTokens int64
	running   bool
	// task is the subagent running now and taskStop whether StopTask
	// asked it to end.
	task      string
	taskStop  bool
	closeOnce sync.Once
	// emitMu and closed keep emit off the channel once it is closed: the
	// turn's goroutine can still be emitting when the session closes, and
	// a send on a closed channel panics even inside a select.
	emitMu sync.RWMutex
	closed bool
}

func (s *session) emit(e agent.Event) {
	s.emitMu.RLock()
	defer s.emitMu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.events <- e:
	case <-s.ctx.Done():
	}
}

func (s *session) sleep() bool {
	select {
	case <-time.After(s.a.Delay):
		return true
	case <-s.stop:
		return false
	case <-s.ctx.Done():
		return false
	}
}

func (s *session) Send(ctx context.Context, text string) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("fake: turn already running")
	}
	s.running = true
	s.turn++
	turnID := fmt.Sprintf("turn-%d", s.turn)
	s.mu.Unlock()
	go s.run(turnID, text)
	return nil
}

func (s *session) run(turnID, prompt string) {
	start := time.Now()
	markIdle := func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}
	defer markIdle()
	finish := func(status string) {
		// A TurnCompleted event promises callers may start the next turn.
		// Mark this one idle before publishing it so queued prompts can hand
		// off immediately.
		markIdle()
		s.emit(agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{
			TurnID: turnID, Anchor: fmt.Sprintf("%s-%d", turnID, start.UnixNano()), Status: status, DurationMS: time.Since(start).Milliseconds(),
			CostUSD: 0.0042, InputTokens: 1234, OutputTokens: 321,
		}})
	}
	s.emit(agent.Event{Kind: agent.KindTurnStarted, TurnStarted: &agent.TurnStarted{TurnID: turnID}})
	// Each turn takes another bite out of the window, in two readings, so
	// the meter both climbs and moves mid-turn.
	s.context(9_000)

	msg := turnID + "-m1"
	for _, w := range strings.Fields("Let me think about " + firstWords(prompt, 6) + " for a moment before I touch anything.") {
		s.emit(agent.Event{Kind: agent.KindThinkingDelta, ThinkingDelta: &agent.ThinkingDelta{MessageID: msg, Text: w + " "}})
		if !s.sleep() {
			finish("interrupted")
			return
		}
	}

	// A line of narration before the work, as Claude writes; with it the
	// finished turn has something to fold besides its last reply.
	for _, w := range strings.SplitAfter("Looking around first.", " ") {
		s.emit(agent.Event{Kind: agent.KindTextDelta, TextDelta: &agent.TextDelta{MessageID: turnID + "-m0", Text: w}})
		if !s.sleep() {
			finish("interrupted")
			return
		}
	}

	// A tool that runs without approval.
	t1 := turnID + "-t1"
	in1, _ := json.Marshal(map[string]any{"pattern": "**/*.go"})
	s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: t1, Name: "Glob", Input: in1, Summary: "**/*.go"}})
	if !s.sleep() {
		finish("interrupted")
		return
	}
	s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: t1, Status: "done", Output: "main.go\ninternal/app/app.go\ninternal/web/server.go"}})

	if strings.Contains(strings.ToLower(prompt), "ask me") && !s.ask(turnID) {
		finish("interrupted")
		return
	}

	// A tool that needs approval.
	t2 := turnID + "-t2"
	in2, _ := json.Marshal(map[string]any{"command": "go test ./...", "description": "Run the test suite"})
	s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: t2, Name: "Bash", Input: in2, Summary: "go test ./..."}})
	// yolo is the fake's never-ask mode, as bypassPermissions is Claude's.
	d := agent.Allow
	if s.cfg.PermissionMode != "yolo" {
		s.emit(agent.Event{Kind: agent.KindApproval, Approval: &agent.ApprovalRequested{ID: turnID + "-a1", ToolID: t2, ToolName: "Bash", Description: "Run the test suite", Input: in2}})
		select {
		case d = <-s.decided:
		case <-s.stop:
			s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: t2, Status: "declined", Output: "interrupted"}})
			finish("interrupted")
			return
		case <-s.ctx.Done():
			return
		}
	}
	if d == agent.Deny {
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: t2, Status: "declined", Output: "User denied this action", IsError: true}})
	} else {
		for _, line := range []string{"ok  \tstarcode/internal/bus\t0.004s\n", "ok  \tstarcode/internal/store\t0.121s\n", "ok  \tstarcode/internal/app\t0.033s\n"} {
			s.emit(agent.Event{Kind: agent.KindToolOutput, ToolOutput: &agent.ToolOutput{ID: t2, Text: line}})
			if !s.sleep() {
				finish("interrupted")
				return
			}
		}
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: t2, Status: "done"}})
	}

	// An edit, which the transcript shows as a diff and the turn's
	// changed-files card counts. Nothing is written: it is only the call.
	t4 := turnID + "-t4"
	in4, _ := json.Marshal(map[string]any{"file_path": filepath.Join(s.cfg.Cwd, "README.md"), "old_string": "# project\nold line", "new_string": "# project\nnew line\nanother line"})
	s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: t4, Name: "Edit", Input: in4, Summary: "README.md"}})
	s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: t4, Status: "done", Output: "The file README.md has been updated."}})

	// A subagent: a Task tool whose nested calls carry its id as Parent,
	// the way Claude Code's parent_tool_use_id does.
	t3 := turnID + "-t3"
	in3, _ := json.Marshal(map[string]any{"description": "Look over the tests", "subagent_type": "Explore", "prompt": "List the test files and say what they cover."})
	s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: t3, Name: "Task", Input: in3, Summary: "Look over the tests"}})
	s.mu.Lock()
	s.task, s.taskStop = t3, false
	s.mu.Unlock()
	switch s.subagent(t3) {
	case "done":
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: t3, Status: "done", Output: "The tests cover the queue, restarts and the store."}})
	case "stopped":
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: t3, Status: "failed", IsError: true, Output: "[Request interrupted by user for tool use]"}})
	default:
		finish("interrupted")
		return
	}
	s.mu.Lock()
	s.task = ""
	s.mu.Unlock()

	s.context(4_500)
	msg2 := turnID + "-m2"
	prose := "You asked: *" + firstWords(prompt, 12) + "*\n\nHere is what I did:\n\n1. Listed the Go files with `Glob`.\n2. Ran the tests (decision: **" + string(d) + "**).\n\n```go\nfunc main() {\n\tfmt.Println(\"hello from the fake agent\")\n}\n```\n\nNothing else needed changing."
	for _, w := range strings.SplitAfter(prose, " ") {
		s.emit(agent.Event{Kind: agent.KindTextDelta, TextDelta: &agent.TextDelta{MessageID: msg2, Text: w}})
		if !s.sleep() {
			finish("interrupted")
			return
		}
	}
	finish("done")
}

// context grows the pretend conversation by n tokens and reports it. The
// long-context model gets the bigger window, so switching between the two
// in the picker moves the meter.
func (s *session) context(n int64) {
	s.mu.Lock()
	s.ctxTokens += n
	tokens := s.ctxTokens
	s.mu.Unlock()
	window := int64(fakeWindow)
	if strings.HasSuffix(s.cfg.Model, "-long") {
		window = 1_000_000
	}
	s.emit(agent.Event{Kind: agent.KindContextUsage, ContextUsage: &agent.ContextUsage{Tokens: tokens, Window: window}})
}

// Compact plays a compaction turn: a notice, then the conversation shrunk
// to a tenth of itself, then the turn's end.
func (s *session) Compact(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("fake: turn already running")
	}
	s.running = true
	s.turn++
	turnID := fmt.Sprintf("turn-%d", s.turn)
	dropped := s.ctxTokens - s.ctxTokens/10
	s.mu.Unlock()
	go func() {
		start := time.Now()
		s.emit(agent.Event{Kind: agent.KindTurnStarted, TurnStarted: &agent.TurnStarted{TurnID: turnID}})
		s.sleep()
		s.emit(agent.Event{Kind: agent.KindNotice, Notice: &agent.Notice{Text: "Context was compacted."}})
		s.context(-dropped)
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		s.emit(agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{
			TurnID: turnID, Status: "done", DurationMS: time.Since(start).Milliseconds(),
		}})
	}()
	return nil
}

// subagent runs the Task t3's nested calls and text. It reports "done",
// "stopped" when StopTask ended it, or "interrupted" for the whole turn.
func (s *session) subagent(t3 string) string {
	stopped := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.taskStop
	}
	for i, name := range []string{"Glob", "Read"} {
		nid := fmt.Sprintf("%s-n%d", t3, i)
		nin, _ := json.Marshal(map[string]any{"pattern": "**/*_test.go"})
		s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: nid, Name: name, Input: nin, Summary: "**/*_test.go", Parent: t3}})
		if !s.sleep() {
			return "interrupted"
		}
		if stopped() {
			// The nested call ends with the subagent.
			s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: nid, Status: "failed", IsError: true}})
			return "stopped"
		}
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: nid, Status: "done", Output: "internal/app/app_test.go"}})
	}
	for _, w := range strings.SplitAfter("The tests cover the queue, restarts and the store. ", " ") {
		s.emit(agent.Event{Kind: agent.KindTextDelta, TextDelta: &agent.TextDelta{MessageID: t3 + "-m", Text: w, Parent: t3}})
		if !s.sleep() {
			return "interrupted"
		}
		if stopped() {
			return "stopped"
		}
	}
	return "done"
}

// StopTask ends the running subagent; the turn goes on.
func (s *session) StopTask(ctx context.Context, toolID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.task == "" || s.task != toolID {
		return agent.ErrNoTask
	}
	s.taskStop = true
	return nil
}

func (s *session) Interrupt(ctx context.Context) error {
	select {
	case s.stop <- struct{}{}:
	default:
	}
	return nil
}

// ask puts one single and one multiple choice question to the human and
// reports whether the turn may go on.
func (s *session) ask(turnID string) bool {
	id := turnID + "-q"
	in, _ := json.Marshal(map[string]any{"questions": []map[string]any{
		{"question": "Which colour should the button be?", "header": "Colour", "multiSelect": false, "options": []map[string]any{
			{"label": "Blue", "description": "Matches the accent"},
			{"label": "Grey", "description": "Quieter"},
		}},
		{"question": "Which pages need it?", "header": "Pages", "multiSelect": true, "options": []map[string]any{
			{"label": "Home", "description": ""},
			{"label": "Thread", "description": ""},
			{"label": "Settings", "description": ""},
		}},
	}})
	s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: id, Name: "AskUserQuestion", Input: in}})
	s.emit(agent.Event{Kind: agent.KindApproval, Approval: &agent.ApprovalRequested{ID: id + "-a", ToolID: id, ToolName: "AskUserQuestion", Input: in}})
	out := "The user did not answer the questions."
	select {
	case answers := <-s.answers:
		var b strings.Builder
		b.WriteString("User has answered your questions:")
		for _, q := range []string{"Which colour should the button be?", "Which pages need it?"} {
			fmt.Fprintf(&b, " %q=%q.", q, answers[q])
		}
		out = b.String()
	case <-s.decided:
	case <-s.stop:
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: id, Status: "declined", Output: "interrupted"}})
		return false
	case <-s.ctx.Done():
		return false
	}
	s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: id, Status: "done", Output: out}})
	return true
}

// Answer answers the question ask put.
func (s *session) Answer(ctx context.Context, approvalID string, answers map[string]string) error {
	select {
	case s.answers <- answers:
		return nil
	default:
		return fmt.Errorf("fake: no pending question")
	}
}

func (s *session) Resolve(ctx context.Context, approvalID string, d agent.Decision) error {
	select {
	case s.decided <- d:
		return nil
	default:
		return fmt.Errorf("fake: no pending approval")
	}
}

func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Close() error {
	s.cancel()
	return nil
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = append(f[:n], "...")
	}
	return strings.Join(f, " ")
}
