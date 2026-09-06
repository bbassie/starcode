// Package fake is a scripted agent used for UI development and tests. Every
// Send replays the same little scenario: some thinking, a tool call that
// needs approval, streamed prose, a result.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"starcode/internal/agent"
)

type Agent struct {
	// Delay between streamed chunks. Zero means as fast as possible.
	Delay time.Duration
}

func New() *Agent { return &Agent{Delay: 40 * time.Millisecond} }

func (a *Agent) Name() string { return "fake" }

func (a *Agent) Capabilities(ctx context.Context) (agent.Capabilities, error) {
	return agent.Capabilities{
		Models:          []agent.Model{{ID: "fake-1", DisplayName: "Fake 1", Default: true, Efforts: []string{"low", "high"}}},
		Efforts:         []agent.Choice{{ID: "", Label: "default"}, {ID: "low", Label: "low"}, {ID: "high", Label: "high"}},
		PermissionModes: []agent.Choice{{ID: "", Label: "default"}, {ID: "yolo", Label: "yolo"}},
	}, nil
}

func (a *Agent) Provider(ctx context.Context) (agent.ProviderInfo, error) {
	yes := true
	return agent.ProviderInfo{Binary: "built in", Version: "0.0.0", LoggedIn: &yes, Account: "no account needed"}, nil
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
		stop:    make(chan struct{}, 1),
	}
	id := cfg.ResumeID
	if id == "" {
		id = fmt.Sprintf("fake-%d", time.Now().UnixNano())
	}
	s.emit(agent.Event{Kind: agent.KindSessionInfo, SessionInfo: &agent.SessionInfo{ExternalID: id, Model: "fake-1"}})
	go func() {
		<-ctx.Done()
		s.closeOnce.Do(func() {
			s.emit(agent.Event{Kind: agent.KindClosed, Closed: &agent.Closed{}})
			close(s.events)
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
	stop      chan struct{}
	mu        sync.Mutex
	turn      int
	running   bool
	closeOnce sync.Once
}

func (s *session) emit(e agent.Event) {
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
			TurnID: turnID, Status: status, DurationMS: time.Since(start).Milliseconds(),
			CostUSD: 0.0042, InputTokens: 1234, OutputTokens: 321,
		}})
	}
	s.emit(agent.Event{Kind: agent.KindTurnStarted, TurnStarted: &agent.TurnStarted{TurnID: turnID}})

	msg := turnID + "-m1"
	for _, w := range strings.Fields("Let me think about " + firstWords(prompt, 6) + " for a moment before I touch anything.") {
		s.emit(agent.Event{Kind: agent.KindThinkingDelta, ThinkingDelta: &agent.ThinkingDelta{MessageID: msg, Text: w + " "}})
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

	// A tool that needs approval.
	t2 := turnID + "-t2"
	in2, _ := json.Marshal(map[string]any{"command": "go test ./...", "description": "Run the test suite"})
	s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{ID: t2, Name: "Bash", Input: in2, Summary: "go test ./..."}})
	s.emit(agent.Event{Kind: agent.KindApproval, Approval: &agent.ApprovalRequested{ID: turnID + "-a1", ToolID: t2, ToolName: "Bash", Description: "Run the test suite", Input: in2}})
	var d agent.Decision
	select {
	case d = <-s.decided:
	case <-s.stop:
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{ID: t2, Status: "declined", Output: "interrupted"}})
		finish("interrupted")
		return
	case <-s.ctx.Done():
		return
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

func (s *session) Interrupt(ctx context.Context) error {
	select {
	case s.stop <- struct{}{}:
	default:
	}
	return nil
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
