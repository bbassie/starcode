package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"starcode/internal/agent"
)

// eventBuffer is the capacity of the channel returned by Events.
const eventBuffer = 256

// unsubscribeTimeout bounds the best-effort thread/unsubscribe sent on Close.
const unsubscribeTimeout = 5 * time.Second

// session is one codex thread. Notifications reach it from the client's
// single reader goroutine; Send, Interrupt, Resolve and Close come from
// whoever drives the session, so shared state is behind mu.
type session struct {
	c        *client
	log      *slog.Logger
	threadID string
	// turnParams are repeated on every turn/start (model, effort, approval
	// policy) so the thread's settings hold even after codex reloads it.
	turnParams map[string]any

	out   chan agent.Event
	qmu   sync.Mutex
	qcond *sync.Cond
	queue []agent.Event
	ended bool

	mu sync.Mutex
	// turnID is the running turn, empty when idle.
	turnID string
	// announced is the last turn we emitted TurnStarted for.
	announced string
	// approvals maps our approval id to the JSON-RPC id to answer.
	approvals map[string]json.RawMessage
	// sawText, sawThinking and sawOutput record which items already
	// streamed, so item/completed does not repeat text we have shown.
	sawText     map[string]bool
	sawThinking map[string]bool
	sawOutput   map[string]bool
	// summaryIndex is the reasoning summary section per item.
	summaryIndex map[string]int
	closing      bool
	// usageTotal mirrors the thread's cumulative token counters; usageBase
	// is the snapshot taken when the current turn started.
	usageTotal, usageBase usage
	// ctxTokens and ctxWindow are the last context reading passed on, so
	// the same numbers are not reported twice.
	ctxTokens, ctxWindow int64
	turnStart            time.Time
}

type usage struct{ input, output int64 }

func newSession(c *client, threadID string, log *slog.Logger) *session {
	s := &session{
		c:            c,
		log:          log.With("thread", threadID),
		threadID:     threadID,
		out:          make(chan agent.Event, eventBuffer),
		approvals:    make(map[string]json.RawMessage),
		sawText:      make(map[string]bool),
		sawThinking:  make(map[string]bool),
		sawOutput:    make(map[string]bool),
		summaryIndex: make(map[string]int),
	}
	s.qcond = sync.NewCond(&s.qmu)
	go s.pump()
	return s
}

func (s *session) Events() <-chan agent.Event { return s.out }

// Send starts a turn. It waits for the turn/start response so the turn id is
// known before any of its items arrive.
func (s *session) Send(ctx context.Context, text string) error {
	params := map[string]any{
		"threadId": s.threadID,
		"input":    []any{map[string]any{"type": "text", "text": text}},
	}
	for k, v := range s.turnParams {
		params[k] = v
	}
	raw, err := s.c.call(ctx, "turn/start", params)
	if err != nil {
		return err
	}
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("codex: turn/start: bad result: %w", err)
	}
	if res.Turn.ID == "" {
		return errors.New("codex: turn/start: result has no turn id")
	}
	s.ensureTurn(res.Turn.ID)
	return nil
}

// Interrupt cancels the running turn. The turn ends with a turn/completed
// notification whose status is "interrupted".
// Compact asks the server to fold the thread's history into a summary. The
// server runs it as a turn of its own: turn/started and turn/completed
// arrive as for a prompt, with a contextCompaction item and a fresh token
// reading between them, so nothing here waits for it.
func (s *session) Compact(ctx context.Context) error {
	_, err := s.c.call(ctx, "thread/compact/start", map[string]any{"threadId": s.threadID})
	return err
}

func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	turnID := s.turnID
	s.mu.Unlock()
	if turnID == "" {
		s.log.Debug("codex: interrupt with no active turn")
		return nil
	}
	_, err := s.c.call(ctx, "turn/interrupt", map[string]any{
		"threadId": s.threadID,
		"turnId":   turnID,
	})
	return err
}

// Resolve answers a pending approval request.
func (s *session) Resolve(ctx context.Context, approvalID string, decision agent.Decision) error {
	s.mu.Lock()
	id, ok := s.approvals[approvalID]
	delete(s.approvals, approvalID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("codex: no pending approval %q", approvalID)
	}
	verdict := "decline"
	if decision == agent.Allow {
		verdict = "accept"
	}
	return s.c.respond(id, map[string]any{"decision": verdict})
}

// Close unsubscribes from the thread and ends the event stream. The shared
// app-server keeps running.
func (s *session) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	pending := s.approvals
	s.approvals = make(map[string]json.RawMessage)
	s.mu.Unlock()

	// Leave nothing for the server to wait on.
	for _, id := range pending {
		_ = s.c.respond(id, map[string]any{"decision": "decline"})
	}
	s.c.removeSession(s)
	if !s.c.isDead() {
		ctx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
		defer cancel()
		if _, err := s.c.call(ctx, "thread/unsubscribe", map[string]any{"threadId": s.threadID}); err != nil {
			s.log.Debug("codex: unsubscribe failed", "err", err)
		}
	}
	s.finish(nil)
	return nil
}

// emit queues an event. The queue is unbounded so one slow consumer cannot
// stall the reader goroutine that every session shares.
func (s *session) emit(ev agent.Event) {
	s.qmu.Lock()
	if s.ended {
		s.qmu.Unlock()
		return
	}
	s.queue = append(s.queue, ev)
	s.qmu.Unlock()
	s.qcond.Signal()
}

func (s *session) emitNotice(text string) {
	s.emit(agent.Event{Kind: agent.KindNotice, Notice: &agent.Notice{Text: text}})
}

// finish queues the final Closed event. Later events are dropped.
func (s *session) finish(err error) {
	s.qmu.Lock()
	if s.ended {
		s.qmu.Unlock()
		return
	}
	s.queue = append(s.queue, agent.Event{Kind: agent.KindClosed, Closed: &agent.Closed{Err: err}})
	s.ended = true
	s.qmu.Unlock()
	s.qcond.Signal()
}

func (s *session) pump() {
	for {
		s.qmu.Lock()
		for len(s.queue) == 0 && !s.ended {
			s.qcond.Wait()
		}
		if len(s.queue) == 0 {
			s.qmu.Unlock()
			close(s.out)
			return
		}
		ev := s.queue[0]
		s.queue = s.queue[1:]
		s.qmu.Unlock()
		s.out <- ev
	}
}

// ensureTurn emits TurnStarted the first time a turn id is seen, whether that
// is from the turn/start response or from an event of that turn.
func (s *session) ensureTurn(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	s.turnID = turnID
	if s.announced == turnID {
		s.mu.Unlock()
		return
	}
	s.announced = turnID
	s.turnStart = time.Now()
	s.usageBase = s.usageTotal
	s.mu.Unlock()
	s.emit(agent.Event{Kind: agent.KindTurnStarted, TurnStarted: &agent.TurnStarted{TurnID: turnID}})
}

// item is the subset of ThreadItem fields this adapter uses. Unknown fields
// are ignored; the raw JSON is passed through as tool input.
type item struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Summary          json.RawMessage `json:"summary"`
	Command          json.RawMessage `json:"command"`
	Cwd              string          `json:"cwd"`
	Status           string          `json:"status"`
	AggregatedOutput string          `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	CommandActions   []commandAction `json:"commandActions"`
	Changes          []struct {
		Path string `json:"path"`
		// Kind is {"type":"add"|"delete"|"update", ...}; older servers may
		// send a bare string. Keep it raw so neither shape breaks decoding.
		Kind json.RawMessage `json:"kind"`
		Diff string          `json:"diff"`
	} `json:"changes"`
	Server string          `json:"server"`
	Tool   string          `json:"tool"`
	Query  string          `json:"query"`
	Result json.RawMessage `json:"result"`
	Review string          `json:"review"`
}

type itemNotification struct {
	TurnID string          `json:"turnId"`
	Item   json.RawMessage `json:"item"`
}

type deltaNotification struct {
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	SummaryIndex *int   `json:"summaryIndex"`
}

func (s *session) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "thread/tokenUsage/updated":
		s.tokenUsage(params)

	case "thread/started", "thread/status/changed",
		"thread/settings/updated", "serverRequest/resolved", "turn/diff/updated",
		"turn/plan/updated", "thread/closed":
		s.log.Debug("codex: notification ignored", "method", method)

	case "thread/name/updated":
		var p struct {
			ThreadName *string `json:"threadName"`
		}
		if err := json.Unmarshal(params, &p); err == nil && p.ThreadName != nil {
			if title := strings.TrimSpace(*p.ThreadName); title != "" {
				s.emit(agent.Event{Kind: agent.KindThreadTitle, ThreadTitle: &agent.ThreadTitle{Title: title}})
			}
		}

	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(params, &p); err == nil {
			s.ensureTurn(p.Turn.ID)
		}

	case "item/started":
		n, it, ok := decodeItem(params)
		if !ok {
			return
		}
		s.ensureTurn(n.TurnID)
		s.itemStarted(n, it)

	case "item/completed":
		n, it, ok := decodeItem(params)
		if !ok {
			return
		}
		s.ensureTurn(n.TurnID)
		s.itemCompleted(it)

	case "item/agentMessage/delta":
		var d deltaNotification
		if err := json.Unmarshal(params, &d); err != nil || d.Delta == "" {
			return
		}
		s.markSeen(s.sawText, d.ItemID)
		s.emit(agent.Event{Kind: agent.KindTextDelta, TextDelta: &agent.TextDelta{
			MessageID: d.ItemID, Text: d.Delta,
		}})

	case "item/reasoning/summaryTextDelta":
		var d deltaNotification
		if err := json.Unmarshal(params, &d); err != nil || d.Delta == "" {
			return
		}
		text := d.Delta
		if s.summarySection(d.ItemID, d.SummaryIndex) {
			text = "\n\n" + text
		}
		s.markSeen(s.sawThinking, d.ItemID)
		s.emit(agent.Event{Kind: agent.KindThinkingDelta, ThinkingDelta: &agent.ThinkingDelta{
			MessageID: d.ItemID, Text: text,
		}})

	case "item/reasoning/textDelta":
		var d deltaNotification
		if err := json.Unmarshal(params, &d); err != nil || d.Delta == "" {
			return
		}
		s.markSeen(s.sawThinking, d.ItemID)
		s.emit(agent.Event{Kind: agent.KindThinkingDelta, ThinkingDelta: &agent.ThinkingDelta{
			MessageID: d.ItemID, Text: d.Delta,
		}})

	case "item/commandExecution/outputDelta":
		var d deltaNotification
		if err := json.Unmarshal(params, &d); err != nil || d.Delta == "" {
			return
		}
		s.markSeen(s.sawOutput, d.ItemID)
		s.emit(agent.Event{Kind: agent.KindToolOutput, ToolOutput: &agent.ToolOutput{
			ID: d.ItemID, Text: d.Delta,
		}})

	case "turn/completed":
		s.turnCompleted(params)

	case "error", "thread/error":
		s.threadError(params)

	case "warning", "configWarning":
		var p struct {
			Message string `json:"message"`
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(params, &p)
		if text := firstNonEmpty(p.Message, p.Summary); text != "" {
			s.emitNotice(text)
		}

	default:
		s.log.Debug("codex: unhandled notification", "method", method)
	}
}

func decodeItem(params json.RawMessage) (itemNotification, item, bool) {
	var n itemNotification
	if err := json.Unmarshal(params, &n); err != nil || len(n.Item) == 0 {
		return n, item{}, false
	}
	var it item
	if err := json.Unmarshal(n.Item, &it); err != nil {
		return n, it, false
	}
	return n, it, true
}

func (s *session) itemStarted(n itemNotification, it item) {
	switch it.Type {
	case "agentMessage", "reasoning", "userMessage", "plan", "todoList":
		// Text arrives as deltas; nothing to show yet.

	case "commandExecution":
		cmd := innerCommand(it.Command, it.CommandActions)
		s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{
			ID: it.ID, Name: "Bash", Input: mustJSON(map[string]any{"command": cmd, "cwd": it.Cwd}), Summary: cmd,
		}})

	case "fileChange":
		s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{
			ID: it.ID, Name: "Edit", Input: mustJSON(map[string]any{"changes": fileChanges(it)}), Summary: changedPaths(it),
		}})

	case "mcpToolCall":
		s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{
			ID: it.ID, Name: it.Server + "/" + it.Tool, Input: n.Item, Summary: it.Tool,
		}})

	case "webSearch":
		s.emit(agent.Event{Kind: agent.KindToolStarted, ToolStarted: &agent.ToolStarted{
			ID: it.ID, Name: "WebSearch", Input: n.Item, Summary: it.Query,
		}})

	case "contextCompaction":
		s.emitNotice("codex compacted the conversation history")

	case "enteredReviewMode":
		s.emitNotice("codex started reviewing " + firstNonEmpty(it.Review, "the changes"))

	default:
		s.log.Debug("codex: unhandled item", "type", it.Type, "id", it.ID)
	}
}

func (s *session) itemCompleted(it item) {
	switch it.Type {
	case "agentMessage":
		if it.Text != "" && !s.seen(s.sawText, it.ID) {
			s.emit(agent.Event{Kind: agent.KindTextDelta, TextDelta: &agent.TextDelta{
				MessageID: it.ID, Text: it.Text,
			}})
		}

	case "reasoning":
		if s.seen(s.sawThinking, it.ID) {
			return
		}
		if text := summaryText(it.Summary); text != "" {
			s.emit(agent.Event{Kind: agent.KindThinkingDelta, ThinkingDelta: &agent.ThinkingDelta{
				MessageID: it.ID, Text: text,
			}})
		}

	case "commandExecution":
		output := ""
		if !s.seen(s.sawOutput, it.ID) {
			output = it.AggregatedOutput
		}
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{
			ID:      it.ID,
			Status:  toolStatus(it.Status),
			Output:  output,
			IsError: it.ExitCode != nil && *it.ExitCode != 0,
		}})

	case "fileChange":
		status := toolStatus(it.Status)
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{
			ID: it.ID, Status: status, IsError: status == "failed",
		}})

	case "mcpToolCall", "webSearch":
		status := toolStatus(it.Status)
		output := ""
		if len(it.Result) > 0 && string(it.Result) != "null" {
			output = string(it.Result)
		}
		s.emit(agent.Event{Kind: agent.KindToolCompleted, ToolCompleted: &agent.ToolCompleted{
			ID: it.ID, Status: status, Output: output, IsError: status == "failed",
		}})

	case "plan", "todoList":
		if it.Text != "" {
			s.emitNotice(it.Text)
		}

	case "exitedReviewMode":
		if it.Review != "" {
			s.emitNotice(it.Review)
		}

	case "userMessage", "reasoningSection", "contextCompaction", "enteredReviewMode":
		// Nothing to add.

	default:
		s.log.Debug("codex: unhandled item completion", "type", it.Type, "id", it.ID)
	}
}

func (s *session) turnCompleted(params json.RawMessage) {
	var p struct {
		Turn struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			DurationMS int64  `json:"durationMs"`
			Error      *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		s.log.Warn("codex: bad turn/completed", "err", err)
		return
	}

	s.mu.Lock()
	turnID := p.Turn.ID
	if turnID == "" {
		turnID = s.turnID
	}
	s.turnID = ""
	pending := s.approvals
	s.approvals = make(map[string]json.RawMessage)
	s.mu.Unlock()

	// The server clears its side of any unanswered approval when the turn
	// ends, so just forget ours.
	for id := range pending {
		s.log.Debug("codex: dropping unanswered approval", "id", id)
	}

	status := "done"
	switch p.Turn.Status {
	case "interrupted":
		status = "interrupted"
	case "failed":
		status = "error"
	}
	msg := ""
	if p.Turn.Error != nil {
		msg = p.Turn.Error.Message
	}
	in, out := tokensFrom(p.Usage)
	s.mu.Lock()
	if in == 0 && out == 0 {
		// No usage on the turn itself; use the cumulative counters that
		// thread/tokenUsage/updated has been feeding since the turn began.
		in = s.usageTotal.input - s.usageBase.input
		out = s.usageTotal.output - s.usageBase.output
	}
	dur := p.Turn.DurationMS
	if dur == 0 && !s.turnStart.IsZero() {
		dur = time.Since(s.turnStart).Milliseconds()
	}
	s.mu.Unlock()
	s.emit(agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{
		TurnID:       turnID,
		Status:       status,
		Error:        msg,
		DurationMS:   dur,
		InputTokens:  in,
		OutputTokens: out,
	}})
}

// tokenUsage records the thread's cumulative counters. Per-turn numbers are
// the difference between the value at turn start and at turn end. The same
// notification carries the last request's size and the model's limit, which
// together are how full the context window is.
func (s *session) tokenUsage(params json.RawMessage) {
	var p struct {
		TokenUsage struct {
			Total struct {
				InputTokens  int64 `json:"inputTokens"`
				OutputTokens int64 `json:"outputTokens"`
			} `json:"total"`
			Last struct {
				InputTokens  int64 `json:"inputTokens"`
				OutputTokens int64 `json:"outputTokens"`
				TotalTokens  int64 `json:"totalTokens"`
			} `json:"last"`
			ModelContextWindow int64 `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	inContext := p.TokenUsage.Last.TotalTokens
	if inContext == 0 {
		inContext = p.TokenUsage.Last.InputTokens + p.TokenUsage.Last.OutputTokens
	}
	s.mu.Lock()
	s.usageTotal = usage{p.TokenUsage.Total.InputTokens, p.TokenUsage.Total.OutputTokens}
	window := p.TokenUsage.ModelContextWindow
	repeat := inContext == s.ctxTokens && window == s.ctxWindow
	s.ctxTokens, s.ctxWindow = inContext, window
	s.mu.Unlock()
	if inContext > 0 && !repeat {
		s.emit(agent.Event{Kind: agent.KindContextUsage, ContextUsage: &agent.ContextUsage{Tokens: inContext, Window: window}})
	}
}

func (s *session) threadError(params json.RawMessage) {
	var p struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(params, &p)
	text := firstNonEmpty(p.Error.Message, p.Message, "codex reported an error")

	s.mu.Lock()
	turnID := s.turnID
	s.turnID = ""
	s.mu.Unlock()

	if turnID == "" {
		s.emitNotice(text)
		return
	}
	s.emit(agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{
		TurnID: turnID, Status: "error", Error: text,
	}})
}

// handleServerRequest turns approval requests into events and answers
// everything else so the server is never left waiting.
func (s *session) handleServerRequest(msg wireMessage) {
	var toolName string
	switch msg.Method {
	case "item/commandExecution/requestApproval":
		toolName = "Bash"
	case "item/fileChange/requestApproval":
		toolName = "Edit"
	default:
		s.c.declineRequest(msg, s)
		return
	}

	var p struct {
		ItemID         string          `json:"itemId"`
		TurnID         string          `json:"turnId"`
		Reason         string          `json:"reason"`
		Command        json.RawMessage `json:"command"`
		Cwd            string          `json:"cwd"`
		CommandActions []commandAction `json:"commandActions"`
		GrantRoot      string          `json:"grantRoot"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s.ensureTurn(p.TurnID)

	// Show the user what matters, not the JSON-RPC envelope. The command is
	// the inner one (codex wraps it in `/bin/bash -lc`), which also keeps
	// "allow for session" rules keyed on the real program name.
	input := map[string]any{}
	if toolName == "Bash" {
		input["command"] = innerCommand(p.Command, p.CommandActions)
		if p.Cwd != "" {
			input["cwd"] = p.Cwd
		}
	} else if p.GrantRoot != "" {
		input["grant_root"] = p.GrantRoot
	}
	if p.Reason != "" {
		input["reason"] = p.Reason
	}

	id := requestID(msg.ID)
	s.mu.Lock()
	s.approvals[id] = msg.ID
	s.mu.Unlock()

	s.emit(agent.Event{Kind: agent.KindApproval, Approval: &agent.ApprovalRequested{
		ID:          id,
		ToolID:      p.ItemID,
		ToolName:    toolName,
		Description: firstNonEmpty(p.Reason, innerCommand(p.Command, p.CommandActions)),
		Input:       mustJSON(input),
	}})
}

type commandAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// innerCommand prefers the command codex parsed out of its shell wrapper
// (`/bin/bash -lc '...'`) over the wrapper itself.
func innerCommand(raw json.RawMessage, actions []commandAction) string {
	if len(actions) == 1 && actions[0].Command != "" {
		return actions[0].Command
	}
	if len(actions) > 1 {
		parts := make([]string, 0, len(actions))
		for _, a := range actions {
			if a.Command != "" {
				parts = append(parts, a.Command)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " && ")
		}
	}
	return commandText(raw)
}

func fileChanges(it item) []map[string]any {
	out := make([]map[string]any, 0, len(it.Changes))
	for _, c := range it.Changes {
		kind := ""
		var k struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(c.Kind, &k) == nil {
			kind = k.Type
		} else {
			json.Unmarshal(c.Kind, &kind)
		}
		ch := map[string]any{"path": c.Path, "kind": kind}
		if c.Diff != "" {
			ch["diff"] = c.Diff
		}
		out = append(out, ch)
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

func (s *session) markSeen(m map[string]bool, id string) {
	s.mu.Lock()
	m[id] = true
	s.mu.Unlock()
}

func (s *session) seen(m map[string]bool, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m[id]
}

// summarySection reports whether a new reasoning summary section started.
func (s *session) summarySection(itemID string, index *int) bool {
	if index == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.summaryIndex[itemID]
	s.summaryIndex[itemID] = *index
	return ok && prev != *index
}

// requestID renders a JSON-RPC id as the string we hand to the app layer.
func requestID(raw json.RawMessage) string {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	return string(raw)
}

// toolStatus maps a codex item status onto the ToolCompleted vocabulary.
func toolStatus(status string) string {
	switch status {
	case "failed":
		return "failed"
	case "declined":
		return "declined"
	default:
		return "done"
	}
}

// commandText renders a command that may be a string or an argv array.
func commandText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err == nil {
		return strings.Join(parts, " ")
	}
	return string(raw)
}

func changedPaths(it item) string {
	paths := make([]string, 0, len(it.Changes))
	for _, c := range it.Changes {
		paths = append(paths, c.Path)
	}
	return strings.Join(paths, ", ")
}

// summaryText renders a reasoning summary, which may be a string, a list of
// strings, or a list of objects with a text field.
func summaryText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, "\n\n")
	}
	var objs []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &objs); err == nil {
		parts := make([]string, 0, len(objs))
		for _, o := range objs {
			if o.Text != "" {
				parts = append(parts, o.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// tokensFrom digs input and output token counts out of a usage object. The
// shape has changed across codex versions, so the search is lenient.
func tokensFrom(raw json.RawMessage) (int64, int64) {
	if len(raw) == 0 {
		return 0, 0
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, 0
	}
	return scanTokens(v, 0)
}

func scanTokens(v any, depth int) (int64, int64) {
	m, ok := v.(map[string]any)
	if !ok || depth > 4 {
		return 0, 0
	}
	var in, out int64
	for k, val := range m {
		switch normalizeKey(k) {
		case "inputtokens", "prompttokens":
			in = asInt(val)
		case "outputtokens", "completiontokens":
			out = asInt(val)
		}
	}
	if in != 0 || out != 0 {
		return in, out
	}
	for _, val := range m {
		if i, o := scanTokens(val, depth+1); i != 0 || o != 0 {
			return i, o
		}
	}
	return 0, 0
}

func normalizeKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(k, "_", ""))
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
