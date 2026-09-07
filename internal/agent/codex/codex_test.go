package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"starcode/internal/agent"
)

// pipeTransport stands in for a codex process: the test writes what the
// server would say and reads what the adapter sends.
type pipeTransport struct {
	r *io.PipeReader
	w *io.PipeWriter

	once sync.Once
}

func (p *pipeTransport) Reader() io.Reader { return p.r }
func (p *pipeTransport) Writer() io.Writer { return p.w }

func (p *pipeTransport) Close() error {
	p.once.Do(func() {
		p.r.Close()
		p.w.Close()
	})
	return nil
}

func (p *pipeTransport) Wait() error { return nil }

// message is one JSON-RPC message read off the wire in a test.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type fakeServer struct {
	t     *testing.T
	out   *io.PipeWriter // server -> adapter
	lines chan []byte    // adapter -> server, one message per line
}

func newFake(t *testing.T) (*Agent, *fakeServer) {
	t.Helper()
	sr, sw := io.Pipe() // server writes, adapter reads
	cr, cw := io.Pipe() // adapter writes, server reads
	tr := &pipeTransport{r: sr, w: cw}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(WithLogger(log), withTransport(func() (transport, error) { return tr, nil }))
	f := &fakeServer{t: t, out: sw, lines: make(chan []byte, 64)}
	// Drain what the adapter writes: io.Pipe has no buffer, so a reader has
	// to be running or every write blocks.
	go func() {
		br := bufio.NewReader(cr)
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				f.lines <- line
			}
			if err != nil {
				close(f.lines)
				return
			}
		}
	}()
	t.Cleanup(func() {
		sw.Close()
		cr.Close()
	})
	return a, f
}

// next reads the adapter's next message, failing the test on timeout.
func (f *fakeServer) next() message {
	f.t.Helper()
	select {
	case line, ok := <-f.lines:
		if !ok {
			f.t.Fatal("adapter closed the connection")
		}
		if strings.Contains(string(line), `"jsonrpc"`) {
			f.t.Fatalf("adapter sent a jsonrpc field: %s", line)
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			f.t.Fatalf("bad message from adapter: %v (%s)", err, line)
		}
		return m
	case <-time.After(2 * time.Second):
		f.t.Fatal("timed out waiting for a message from the adapter")
		return message{}
	}
}

func (f *fakeServer) expect(method string) message {
	f.t.Helper()
	m := f.next()
	if m.Method != method {
		f.t.Fatalf("got method %q, want %q", m.Method, method)
	}
	return m
}

func (f *fakeServer) send(raw string) {
	f.t.Helper()
	if _, err := io.WriteString(f.out, raw+"\n"); err != nil {
		f.t.Fatalf("write to adapter: %v", err)
	}
}

func (f *fakeServer) reply(id json.RawMessage, result string) {
	f.t.Helper()
	f.send(`{"id":` + string(id) + `,"result":` + result + `}`)
}

// handshake answers initialize and consumes the initialized notification.
func (f *fakeServer) handshake() {
	f.t.Helper()
	m := f.expect("initialize")
	var p struct {
		ClientInfo struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"clientInfo"`
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		f.t.Fatalf("initialize params: %v", err)
	}
	if p.ClientInfo.Name != "starcode" || p.ClientInfo.Title != "starcode" || p.ClientInfo.Version == "" {
		f.t.Fatalf("unexpected clientInfo: %+v", p.ClientInfo)
	}
	if p.Capabilities == nil {
		f.t.Fatal("initialize sent no capabilities object")
	}
	f.reply(m.ID, `{"userAgent":"codex/test","codexHome":"/tmp"}`)
	f.expect("initialized")
}

type startResult struct {
	sess agent.Session
	err  error
}

func startAsync(a *Agent, cfg agent.Config) <-chan startResult {
	ch := make(chan startResult, 1)
	go func() {
		s, err := a.Start(context.Background(), cfg)
		ch <- startResult{s, err}
	}()
	return ch
}

// harness is a live session with the handshake and thread/start done.
func harness(t *testing.T) (*Agent, *fakeServer, agent.Session) {
	t.Helper()
	a, f := newFake(t)
	res := startAsync(a, agent.Config{Cwd: "/work", Model: "gpt-5.1-codex"})
	f.handshake()
	m := f.expect("thread/start")
	f.reply(m.ID, `{"thread":{"id":"thr_1","model":"gpt-5.1-codex"}}`)
	r := <-res
	if r.err != nil {
		t.Fatalf("Start: %v", r.err)
	}
	ev := nextEvent(t, r.sess.Events())
	if ev.Kind != agent.KindSessionInfo || ev.SessionInfo.ExternalID != "thr_1" {
		t.Fatalf("first event = %+v, want session info for thr_1", ev)
	}
	return a, f, r.sess
}

func nextEvent(t *testing.T, ch <-chan agent.Event) agent.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed early")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return agent.Event{}
	}
}

func expectKind(t *testing.T, ch <-chan agent.Event, kind agent.EventKind) agent.Event {
	t.Helper()
	ev := nextEvent(t, ch)
	if ev.Kind != kind {
		t.Fatalf("got event %s, want %s (%+v)", ev.Kind, kind, ev)
	}
	return ev
}

// sendAsync starts a turn and answers turn/start with the given turn id.
func sendAsync(t *testing.T, f *fakeServer, s agent.Session, text, turnID string) {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- s.Send(context.Background(), text) }()
	m := f.expect("turn/start")
	var p struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("turn/start params: %v", err)
	}
	if p.ThreadID != "thr_1" {
		t.Fatalf("turn/start threadId = %q", p.ThreadID)
	}
	if len(p.Input) != 1 || p.Input[0].Type != "text" || p.Input[0].Text != text {
		t.Fatalf("turn/start input = %+v", p.Input)
	}
	f.reply(m.ID, `{"turn":{"id":"`+turnID+`","status":"inProgress","items":[],"error":null}}`)
	if err := <-errc; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestStartHandshakeAndThreadStart(t *testing.T) {
	a, f := newFake(t)
	res := startAsync(a, agent.Config{Cwd: "/work", Model: "gpt-5.1-codex", PermissionMode: "full-access"})
	f.handshake()

	m := f.expect("thread/start")
	var p map[string]any
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("thread/start params: %v", err)
	}
	if p["cwd"] != "/work" || p["model"] != "gpt-5.1-codex" {
		t.Fatalf("thread/start params = %v", p)
	}
	if p["approvalPolicy"] != "never" || p["sandbox"] != "danger-full-access" {
		t.Fatalf("thread/start params = %v", p)
	}
	if _, ok := p["threadId"]; ok {
		t.Fatalf("thread/start should not carry a threadId: %v", p)
	}
	f.reply(m.ID, `{"thread":{"id":"thr_9","model":"gpt-5.1-codex","name":"Generated thread name"}}`)

	r := <-res
	if r.err != nil {
		t.Fatalf("Start: %v", r.err)
	}
	ev := nextEvent(t, r.sess.Events())
	if ev.Kind != agent.KindSessionInfo {
		t.Fatalf("got %s, want session info", ev.Kind)
	}
	if ev.SessionInfo.ExternalID != "thr_9" || ev.SessionInfo.Model != "gpt-5.1-codex" {
		t.Fatalf("session info = %+v", ev.SessionInfo)
	}
	ev = expectKind(t, r.sess.Events(), agent.KindThreadTitle)
	if ev.ThreadTitle.Title != "Generated thread name" {
		t.Fatalf("thread title = %+v", ev.ThreadTitle)
	}
	if a.Name() != "codex" {
		t.Fatalf("Name = %q", a.Name())
	}
}

func TestStartDefaultApprovalPolicyAndResume(t *testing.T) {
	a, f := newFake(t)
	res := startAsync(a, agent.Config{Cwd: "/work", ResumeID: "thr_old"})
	f.handshake()

	m := f.expect("thread/resume")
	var p map[string]any
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("thread/resume params: %v", err)
	}
	if p["threadId"] != "thr_old" || p["cwd"] != "/work" {
		t.Fatalf("thread/resume params = %v", p)
	}
	if p["approvalPolicy"] != "on-request" {
		t.Fatalf("approvalPolicy = %v, want on-request", p["approvalPolicy"])
	}
	if _, ok := p["model"]; ok {
		t.Fatalf("empty model should be omitted: %v", p)
	}
	f.reply(m.ID, `{"thread":{"id":"thr_old","turns":[{"id":"turn_1"}]}}`)

	r := <-res
	if r.err != nil {
		t.Fatalf("Start: %v", r.err)
	}
	ev := nextEvent(t, r.sess.Events())
	if ev.SessionInfo.ExternalID != "thr_old" {
		t.Fatalf("session info = %+v", ev.SessionInfo)
	}
}

func TestTurnWithAgentMessageDeltas(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()

	sendAsync(t, f, s, "hello", "turn_1")
	ev := expectKind(t, events, agent.KindTurnStarted)
	if ev.TurnStarted.TurnID != "turn_1" {
		t.Fatalf("turn id = %q", ev.TurnStarted.TurnID)
	}

	f.send(`{"method":"turn/started","params":{"threadId":"thr_1","turn":{"id":"turn_1","status":"inProgress"}}}`)
	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"item_1","type":"agentMessage","text":""}}}`)
	f.send(`{"method":"item/agentMessage/delta","params":{"threadId":"thr_1","itemId":"item_1","delta":"Hel"}}`)
	f.send(`{"method":"item/agentMessage/delta","params":{"threadId":"thr_1","itemId":"item_1","delta":"lo."}}`)
	f.send(`{"method":"item/completed","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"item_1","type":"agentMessage","text":"Hello."}}}`)
	f.send(`{"method":"turn/completed","params":{"threadId":"thr_1","turn":{"id":"turn_1","status":"completed"},"usage":{"inputTokens":120,"outputTokens":34}}}`)

	var text strings.Builder
	for range 2 {
		ev := expectKind(t, events, agent.KindTextDelta)
		if ev.TextDelta.MessageID != "item_1" {
			t.Fatalf("message id = %q", ev.TextDelta.MessageID)
		}
		text.WriteString(ev.TextDelta.Text)
	}
	if text.String() != "Hello." {
		t.Fatalf("streamed text = %q", text.String())
	}

	// item/completed must not repeat text that already streamed.
	ev = expectKind(t, events, agent.KindTurnCompleted)
	if ev.TurnCompleted.TurnID != "turn_1" || ev.TurnCompleted.Status != "done" {
		t.Fatalf("turn completed = %+v", ev.TurnCompleted)
	}
	if ev.TurnCompleted.InputTokens != 120 || ev.TurnCompleted.OutputTokens != 34 {
		t.Fatalf("usage = %+v", ev.TurnCompleted)
	}
}

func TestTokenUsageReportsContext(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()

	sendAsync(t, f, s, "hello", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	f.send(`{"method":"thread/tokenUsage/updated","params":{"threadId":"thr_1","turnId":"turn_1","tokenUsage":{"total":{"inputTokens":120,"outputTokens":34,"totalTokens":154},"last":{"inputTokens":120,"outputTokens":34,"totalTokens":154},"modelContextWindow":272000}}}`)
	ev := expectKind(t, events, agent.KindContextUsage)
	if ev.ContextUsage.Tokens != 154 || ev.ContextUsage.Window != 272000 {
		t.Fatalf("context usage = %+v", ev.ContextUsage)
	}

	// The same reading again says nothing; a bigger one does.
	f.send(`{"method":"thread/tokenUsage/updated","params":{"threadId":"thr_1","turnId":"turn_1","tokenUsage":{"total":{"inputTokens":120,"outputTokens":34,"totalTokens":154},"last":{"inputTokens":120,"outputTokens":34,"totalTokens":154},"modelContextWindow":272000}}}`)
	f.send(`{"method":"thread/tokenUsage/updated","params":{"threadId":"thr_1","turnId":"turn_1","tokenUsage":{"total":{"inputTokens":900,"outputTokens":60,"totalTokens":960},"last":{"inputTokens":900,"outputTokens":60,"totalTokens":960},"modelContextWindow":272000}}}`)
	ev = expectKind(t, events, agent.KindContextUsage)
	if ev.ContextUsage.Tokens != 960 {
		t.Fatalf("context usage = %+v", ev.ContextUsage)
	}
}

func TestAgentMessageWithoutDeltas(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	sendAsync(t, f, s, "hi", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	f.send(`{"method":"item/completed","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"item_7","type":"agentMessage","text":"whole reply"}}}`)
	ev := expectKind(t, events, agent.KindTextDelta)
	if ev.TextDelta.Text != "whole reply" || ev.TextDelta.MessageID != "item_7" {
		t.Fatalf("text delta = %+v", ev.TextDelta)
	}
}

func TestReasoningSummarySections(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	sendAsync(t, f, s, "think", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"r1","type":"reasoning"}}}`)
	f.send(`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"thr_1","itemId":"r1","delta":"first","summaryIndex":0}}`)
	f.send(`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"thr_1","itemId":"r1","delta":"second","summaryIndex":1}}`)

	if got := expectKind(t, events, agent.KindThinkingDelta).ThinkingDelta.Text; got != "first" {
		t.Fatalf("first summary delta = %q", got)
	}
	if got := expectKind(t, events, agent.KindThinkingDelta).ThinkingDelta.Text; got != "\n\nsecond" {
		t.Fatalf("second summary delta = %q", got)
	}
}

// runCommandWithApproval drives a commandExecution up to the approval event.
func runCommandWithApproval(t *testing.T, f *fakeServer, s agent.Session) agent.Event {
	t.Helper()
	events := s.Events()
	sendAsync(t, f, s, "run tests", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"cmd_1","type":"commandExecution","command":["go","test","./..."],"cwd":"/work","status":"inProgress"}}}`)
	ev := expectKind(t, events, agent.KindToolStarted)
	if ev.ToolStarted.ID != "cmd_1" || ev.ToolStarted.Name != "Bash" {
		t.Fatalf("tool started = %+v", ev.ToolStarted)
	}
	if ev.ToolStarted.Summary != "go test ./..." {
		t.Fatalf("summary = %q", ev.ToolStarted.Summary)
	}
	if !json.Valid(ev.ToolStarted.Input) {
		t.Fatalf("input is not json: %s", ev.ToolStarted.Input)
	}

	f.send(`{"id":41,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"cmd_1","command":["go","test","./..."],"cwd":"/work","reason":"command needs network","availableDecisions":["accept","decline"]}}`)
	ev = expectKind(t, events, agent.KindApproval)
	if ev.Approval.ID != "41" || ev.Approval.ToolID != "cmd_1" || ev.Approval.ToolName != "Bash" {
		t.Fatalf("approval = %+v", ev.Approval)
	}
	if ev.Approval.Description != "command needs network" {
		t.Fatalf("description = %q", ev.Approval.Description)
	}
	return ev
}

func TestCommandApprovalAccepted(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	ap := runCommandWithApproval(t, f, s)

	if err := s.Resolve(context.Background(), ap.Approval.ID, agent.Allow); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	m := f.next()
	if string(m.ID) != "41" || m.Method != "" {
		t.Fatalf("approval answer = %+v", m)
	}
	var res struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(m.Result, &res); err != nil || res.Decision != "accept" {
		t.Fatalf("decision = %s (%v)", m.Result, err)
	}

	f.send(`{"method":"item/commandExecution/outputDelta","params":{"threadId":"thr_1","itemId":"cmd_1","delta":"ok\n"}}`)
	ev := expectKind(t, events, agent.KindToolOutput)
	if ev.ToolOutput.ID != "cmd_1" || ev.ToolOutput.Text != "ok\n" {
		t.Fatalf("tool output = %+v", ev.ToolOutput)
	}

	f.send(`{"method":"item/completed","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"cmd_1","type":"commandExecution","status":"completed","exitCode":0,"aggregatedOutput":"ok\n"}}}`)
	ev = expectKind(t, events, agent.KindToolCompleted)
	if ev.ToolCompleted.Status != "done" || ev.ToolCompleted.IsError {
		t.Fatalf("tool completed = %+v", ev.ToolCompleted)
	}
	// The output already streamed, so it is not repeated.
	if ev.ToolCompleted.Output != "" {
		t.Fatalf("output repeated: %q", ev.ToolCompleted.Output)
	}

	// A second Resolve for the same id fails; the approval is gone.
	if err := s.Resolve(context.Background(), ap.Approval.ID, agent.Allow); err == nil {
		t.Fatal("second Resolve should fail")
	}
}

func TestCommandApprovalDeclined(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	ap := runCommandWithApproval(t, f, s)

	if err := s.Resolve(context.Background(), ap.Approval.ID, agent.Deny); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	m := f.next()
	var res struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(m.Result, &res); err != nil || res.Decision != "decline" {
		t.Fatalf("decision = %s (%v)", m.Result, err)
	}

	f.send(`{"method":"item/completed","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"cmd_1","type":"commandExecution","status":"declined","aggregatedOutput":"skipped"}}}`)
	ev := expectKind(t, events, agent.KindToolCompleted)
	if ev.ToolCompleted.Status != "declined" || ev.ToolCompleted.Output != "skipped" {
		t.Fatalf("tool completed = %+v", ev.ToolCompleted)
	}
}

func TestFileChangeApproval(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	sendAsync(t, f, s, "edit", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"fc_1","type":"fileChange","status":"inProgress","changes":[{"path":"/work/a.go","kind":"update"},{"path":"/work/b.go","kind":"add"}]}}}`)
	ev := expectKind(t, events, agent.KindToolStarted)
	if ev.ToolStarted.Name != "Edit" || ev.ToolStarted.Summary != "/work/a.go, /work/b.go" {
		t.Fatalf("tool started = %+v", ev.ToolStarted)
	}

	f.send(`{"id":52,"method":"item/fileChange/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"fc_1","reason":"write outside workspace"}}`)
	ev = expectKind(t, events, agent.KindApproval)
	if ev.Approval.ToolName != "Edit" || ev.Approval.ID != "52" {
		t.Fatalf("approval = %+v", ev.Approval)
	}
	if err := s.Resolve(context.Background(), "52", agent.Allow); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m := f.next(); string(m.ID) != "52" {
		t.Fatalf("answered the wrong request: %+v", m)
	}

	f.send(`{"method":"item/completed","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"fc_1","type":"fileChange","status":"failed","changes":[]}}}`)
	ev = expectKind(t, events, agent.KindToolCompleted)
	if ev.ToolCompleted.Status != "failed" || !ev.ToolCompleted.IsError {
		t.Fatalf("tool completed = %+v", ev.ToolCompleted)
	}
}

func TestInterrupt(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	sendAsync(t, f, s, "long job", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	errc := make(chan error, 1)
	go func() { errc <- s.Interrupt(context.Background()) }()
	m := f.expect("turn/interrupt")
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("turn/interrupt params: %v", err)
	}
	if p.ThreadID != "thr_1" || p.TurnID != "turn_1" {
		t.Fatalf("turn/interrupt params = %+v", p)
	}
	f.reply(m.ID, `{}`)
	if err := <-errc; err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	f.send(`{"method":"turn/completed","params":{"threadId":"thr_1","turn":{"id":"turn_1","status":"interrupted"}}}`)
	ev := expectKind(t, events, agent.KindTurnCompleted)
	if ev.TurnCompleted.Status != "interrupted" {
		t.Fatalf("turn completed = %+v", ev.TurnCompleted)
	}

	// With no turn running, Interrupt is a no-op rather than an error.
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("idle Interrupt: %v", err)
	}
}

func TestFailedTurnCarriesError(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	sendAsync(t, f, s, "boom", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	f.send(`{"method":"turn/completed","params":{"threadId":"thr_1","turn":{"id":"turn_1","status":"failed","error":{"message":"usage limit reached"}},"usage":{"input_tokens":5,"output_tokens":0}}}`)
	ev := expectKind(t, events, agent.KindTurnCompleted)
	if ev.TurnCompleted.Status != "error" || ev.TurnCompleted.Error != "usage limit reached" {
		t.Fatalf("turn completed = %+v", ev.TurnCompleted)
	}
	if ev.TurnCompleted.InputTokens != 5 {
		t.Fatalf("usage = %+v", ev.TurnCompleted)
	}
}

func TestServerRequestsAreAlwaysAnswered(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()

	// Unknown method: a JSON-RPC error keeps the server from hanging.
	f.send(`{"id":70,"method":"something/newInCodex","params":{"threadId":"thr_1"}}`)
	m := f.next()
	if string(m.ID) != "70" || m.Error == nil || m.Error.Code != -32601 {
		t.Fatalf("unknown request answer = %+v", m)
	}
	if ev := expectKind(t, events, agent.KindNotice); !strings.Contains(ev.Notice.Text, "something/newInCodex") {
		t.Fatalf("notice = %q", ev.Notice.Text)
	}

	// Elicitations are auto-declined.
	f.send(`{"id":71,"method":"mcpServer/elicitation/request","params":{"threadId":"thr_1","serverName":"docs","mode":"form","message":"pick one"}}`)
	m = f.next()
	var elicit struct {
		Action  string          `json:"action"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(m.Result, &elicit); err != nil {
		t.Fatalf("elicitation answer: %v (%s)", err, m.Result)
	}
	if string(m.ID) != "71" || elicit.Action != "decline" || string(elicit.Content) != "null" {
		t.Fatalf("elicitation answer = %+v %s", m, m.Result)
	}
	expectKind(t, events, agent.KindNotice)

	// request_user_input is declined the same way.
	f.send(`{"id":72,"method":"item/tool/requestUserInput","params":{"threadId":"thr_1","isBlocking":true}}`)
	m = f.next()
	if string(m.ID) != "72" || !strings.Contains(string(m.Result), `"decline"`) {
		t.Fatalf("requestUserInput answer = %+v", m)
	}
	expectKind(t, events, agent.KindNotice)

	// Permission requests are answered with an empty grant.
	f.send(`{"id":73,"method":"item/permissions/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"call_1","permissions":{"fileSystem":{"write":["/etc"]}}}}`)
	m = f.next()
	var perms struct {
		Permissions map[string]any `json:"permissions"`
	}
	if err := json.Unmarshal(m.Result, &perms); err != nil {
		t.Fatalf("permissions answer: %v", err)
	}
	if string(m.ID) != "73" || len(perms.Permissions) != 0 {
		t.Fatalf("permissions answer = %+v %s", m, m.Result)
	}
	expectKind(t, events, agent.KindNotice)

	// currentTime/read is answered even though it has no session behind it.
	f.send(`{"id":74,"method":"currentTime/read","params":{"threadId":"thr_1"}}`)
	m = f.next()
	var now struct {
		CurrentTimeAt int64 `json:"currentTimeAt"`
	}
	if err := json.Unmarshal(m.Result, &now); err != nil {
		t.Fatalf("currentTime answer: %v", err)
	}
	if string(m.ID) != "74" || now.CurrentTimeAt <= 0 {
		t.Fatalf("currentTime answer = %+v %s", m, m.Result)
	}

	// Attestation is refused outright.
	f.send(`{"id":75,"method":"attestation/generate","params":{}}`)
	m = f.next()
	if string(m.ID) != "75" || m.Error == nil || m.Error.Code != -32601 {
		t.Fatalf("attestation answer = %+v", m)
	}
}

func TestNotificationsForUnknownThreadAreDropped(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	sendAsync(t, f, s, "hi", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	f.send(`{"method":"item/agentMessage/delta","params":{"threadId":"thr_other","itemId":"x","delta":"not ours"}}`)
	f.send(`{"method":"item/agentMessage/delta","params":{"threadId":"thr_1","itemId":"item_1","delta":"ours"}}`)

	ev := expectKind(t, events, agent.KindTextDelta)
	if ev.TextDelta.Text != "ours" {
		t.Fatalf("leaked another thread's delta: %q", ev.TextDelta.Text)
	}

	// A server request for an unknown thread is still answered.
	f.send(`{"id":80,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr_other","itemId":"cmd_x"}}`)
	m := f.next()
	if string(m.ID) != "80" || m.Error == nil {
		t.Fatalf("unknown-thread request answer = %+v", m)
	}
}

func TestProcessDeathClosesEverySession(t *testing.T) {
	a, f := newFake(t)

	res := startAsync(a, agent.Config{Cwd: "/work"})
	f.handshake()
	m := f.expect("thread/start")
	f.reply(m.ID, `{"thread":{"id":"thr_1"}}`)
	first := <-res
	if first.err != nil {
		t.Fatalf("Start: %v", first.err)
	}
	expectKind(t, first.sess.Events(), agent.KindSessionInfo)

	res = startAsync(a, agent.Config{Cwd: "/work2"})
	m = f.expect("thread/start")
	f.reply(m.ID, `{"thread":{"id":"thr_2"}}`)
	second := <-res
	if second.err != nil {
		t.Fatalf("Start: %v", second.err)
	}
	expectKind(t, second.sess.Events(), agent.KindSessionInfo)

	// The process dies.
	f.out.Close()

	for i, s := range []agent.Session{first.sess, second.sess} {
		ev := expectKind(t, s.Events(), agent.KindClosed)
		if ev.Closed.Err == nil {
			t.Fatalf("session %d closed without an error", i)
		}
		if _, ok := <-s.Events(); ok {
			t.Fatalf("session %d kept emitting after Closed", i)
		}
	}

	// Sends after the process died fail instead of blocking.
	if err := first.sess.Send(context.Background(), "hi"); err == nil {
		t.Fatal("Send on a dead process should fail")
	}
}

func TestCloseUnsubscribesAndEndsTheStream(t *testing.T) {
	_, f, s := harness(t)

	errc := make(chan error, 1)
	go func() { errc <- s.Close() }()
	m := f.expect("thread/unsubscribe")
	var p struct {
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil || p.ThreadID != "thr_1" {
		t.Fatalf("unsubscribe params = %s (%v)", m.Params, err)
	}
	f.reply(m.ID, `{}`)
	if err := <-errc; err != nil {
		t.Fatalf("Close: %v", err)
	}

	ev := expectKind(t, s.Events(), agent.KindClosed)
	if ev.Closed.Err != nil {
		t.Fatalf("clean close reported %v", ev.Closed.Err)
	}
	if _, ok := <-s.Events(); ok {
		t.Fatal("events channel stayed open after Closed")
	}
	// Close is safe to call again.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestShutdownStopsTheSharedProcess(t *testing.T) {
	a, _, s := harness(t)
	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	ev := expectKind(t, s.Events(), agent.KindClosed)
	if ev.Closed.Err == nil {
		t.Fatal("shutdown should close sessions with an error")
	}
}

func TestMiscItemMappings(t *testing.T) {
	_, f, s := harness(t)
	events := s.Events()
	sendAsync(t, f, s, "search", "turn_1")
	expectKind(t, events, agent.KindTurnStarted)

	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"ws_1","type":"webSearch","query":"go slog"}}}`)
	ev := expectKind(t, events, agent.KindToolStarted)
	if ev.ToolStarted.Name != "WebSearch" || ev.ToolStarted.Summary != "go slog" {
		t.Fatalf("tool started = %+v", ev.ToolStarted)
	}

	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"mcp_1","type":"mcpToolCall","server":"docs","tool":"search","arguments":{"q":"x"}}}}`)
	ev = expectKind(t, events, agent.KindToolStarted)
	if ev.ToolStarted.Name != "docs/search" || ev.ToolStarted.Summary != "search" {
		t.Fatalf("tool started = %+v", ev.ToolStarted)
	}

	f.send(`{"method":"item/completed","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"mcp_1","type":"mcpToolCall","status":"completed","result":{"hits":2}}}}`)
	ev = expectKind(t, events, agent.KindToolCompleted)
	if ev.ToolCompleted.Status != "done" || !strings.Contains(ev.ToolCompleted.Output, `"hits":2`) {
		t.Fatalf("tool completed = %+v", ev.ToolCompleted)
	}

	// Items we only surface as notices.
	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"cc_1","type":"contextCompaction"}}}`)
	expectKind(t, events, agent.KindNotice)

	// Items we ignore entirely produce nothing, so the next event is the
	// turn completion.
	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_1","item":{"id":"um_1","type":"userMessage","content":[]}}}`)
	f.send(`{"method":"thread/status/changed","params":{"threadId":"thr_1","status":"running"}}`)
	f.send(`{"method":"turn/completed","params":{"threadId":"thr_1","turn":{"id":"turn_1","status":"completed"}}}`)
	expectKind(t, events, agent.KindTurnCompleted)
}

func TestThreadNameUpdated(t *testing.T) {
	_, f, s := harness(t)
	f.send(`{"method":"thread/name/updated","params":{"threadId":"thr_1","threadName":" Fix flaky queue tests "}}`)
	ev := expectKind(t, s.Events(), agent.KindThreadTitle)
	if ev.ThreadTitle == nil || ev.ThreadTitle.Title != "Fix flaky queue tests" {
		t.Fatalf("thread title = %+v", ev.ThreadTitle)
	}

	// A cleared server-side name should leave Starcode's fallback intact.
	f.send(`{"method":"thread/name/updated","params":{"threadId":"thr_1","threadName":null}}`)
	f.send(`{"method":"warning","params":{"threadId":"thr_1","message":"next"}}`)
	expectKind(t, s.Events(), agent.KindNotice)
}

func TestItemEventsAnnounceAnUnseenTurn(t *testing.T) {
	// Nothing was sent, so the adapter has no turn id yet; an item event
	// must still announce the turn before its own event.
	_, f, s := harness(t)
	events := s.Events()

	f.send(`{"method":"item/started","params":{"threadId":"thr_1","turnId":"turn_x","item":{"id":"cmd_9","type":"commandExecution","command":"ls","status":"inProgress"}}}`)
	ev := expectKind(t, events, agent.KindTurnStarted)
	if ev.TurnStarted.TurnID != "turn_x" {
		t.Fatalf("turn id = %q", ev.TurnStarted.TurnID)
	}
	ev = expectKind(t, events, agent.KindToolStarted)
	if ev.ToolStarted.Summary != "ls" {
		t.Fatalf("summary = %q", ev.ToolStarted.Summary)
	}
}
