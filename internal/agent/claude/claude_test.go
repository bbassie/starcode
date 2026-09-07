package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
)

// Recorded lines from claude 2.1.252. Long fields (signatures, tool lists)
// are trimmed; the shapes are verbatim.
const (
	lineInit = `{"type":"system","subtype":"init","cwd":"/tmp/work","session_id":"35450ae3-148d-4cc0-9493-76ae6036b29d","tools":["Bash","Write"],"mcp_servers":[],"model":"claude-haiku-4-5-20251001","permissionMode":"default","apiKeySource":"none","claude_code_version":"2.1.252","uuid":"32c47e91-3cf9-4ac0-af62-55654551ff02"}`

	lineStatus        = `{"type":"system","subtype":"status","status":"requesting","session_id":"s1","uuid":"cd5766f6-903f-418b-9661-6b50bb34288d"}`
	lineThinkingToks  = `{"type":"system","subtype":"thinking_tokens","estimated_tokens":50,"estimated_tokens_delta":50,"session_id":"s1","uuid":"4d1a3d25-ae14-46d3-991c-d7a9fe1ddca5"}`
	lineCompact       = `{"type":"system","subtype":"compact_boundary","session_id":"s1","compact_metadata":{"trigger":"auto"}}`
	// A manual /compact, recorded from claude 2.1.263: the status pair, the
	// boundary with sizes, then a result with no usage.
	lineCompacting    = `{"type":"system","subtype":"status","status":"compacting","session_id":"s1"}`
	lineCompactedOK   = `{"type":"system","subtype":"status","status":null,"compact_result":"success","session_id":"s1"}`
	lineCompactSizes  = `{"type":"system","subtype":"compact_boundary","session_id":"s1","compact_metadata":{"trigger":"manual","pre_tokens":23016,"post_tokens":1046,"cumulative_dropped_tokens":21970,"duration_ms":8252}}`
	lineCompactResult = `{"session_id":"s1","total_cost_usd":0.02,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0},"is_error":false,"num_turns":0,"subtype":"success","result":"","type":"result","duration_ms":8300}`
	lineRateAllowed   = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1788525000,"rateLimitType":"five_hour"},"session_id":"s1"}`
	lineRateThrottled = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1788525000,"rateLimitType":"five_hour"},"session_id":"s1"}`
	lineCtlAck        = `{"type":"control_response","response":{"subtype":"success","request_id":"int-1","response":{"still_queued":[]}}}`
	lineUnknown       = `{"type":"brand_new_thing","payload":{"a":1}}`
	lineAITitle       = `{"type":"ai-title","aiTitle":" Terminal UI addition ","sessionId":"s1"}`

	lineMsgStart  = `{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-haiku-4-5-20251001","id":"msg_011CeiAxjHc8XT31ZgLc3G27","type":"message","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":4}}},"session_id":"s1","parent_tool_use_id":null}`
	lineMsgDelta  = `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":46}},"session_id":"s1"}`
	lineMsgStop   = `{"type":"stream_event","event":{"type":"message_stop"},"session_id":"s1"}`
	lineTextStart = `{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}},"session_id":"s1"}`
	// \u0060 is a backtick, escaped so these stay raw Go strings.
	lineTextA    = `{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Done. Created \u0060hello."}},"session_id":"s1"}`
	lineTextB    = `{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"txt\u0060."}},"session_id":"s1"}`
	lineTextStop = `{"type":"stream_event","event":{"type":"content_block_stop","index":1},"session_id":"s1"}`
	lineTextMsg  = `{"type":"assistant","message":{"id":"msg_011CeiAxjHc8XT31ZgLc3G27","type":"message","role":"assistant","content":[{"type":"text","text":"Done. Created \u0060hello.txt\u0060."}]},"session_id":"s1"}`

	lineThinkStart = `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}},"session_id":"s1"}`
	lineThinkDelta = `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"The user wants a file."}},"session_id":"s1"}`
	lineThinkEmpty = `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"","estimated_tokens":50}},"session_id":"s1"}`
	lineSigDelta   = `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqcEC"}},"session_id":"s1"}`

	lineToolStart = `{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01LgDc215WriEAa54mPYebjY","name":"Write","input":{},"caller":{"type":"direct"}}},"session_id":"s1"}`
	lineToolIn1   = `{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\": \"/"}},"session_id":"s1"}`
	lineToolIn2   = `{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"tmp/work/hello.txt"}},"session_id":"s1"}`
	lineToolIn3   = `{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\", \"content\": \"hi\"}"}},"session_id":"s1"}`
	lineToolStop  = `{"type":"stream_event","event":{"type":"content_block_stop","index":1},"session_id":"s1"}`
	lineToolMsg   = `{"type":"assistant","message":{"id":"msg_011CeiAxjHc8XT31ZgLc3G27","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01LgDc215WriEAa54mPYebjY","name":"Write","input":{"file_path":"/tmp/work/hello.txt","content":"hi"},"caller":{"type":"direct"}}]},"session_id":"s1"}`

	lineApproval = `{"type":"control_request","request_id":"590e89a1-ab17-4fa4-bcb7-4a44035eb43d","request":{"subtype":"can_use_tool","tool_name":"Write","display_name":"Write","input":{"file_path":"/tmp/work/hello.txt","content":"hi"},"description":"hello.txt","permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}],"tool_use_id":"toolu_01LgDc215WriEAa54mPYebjY"}}`
	lineHookReq  = `{"type":"control_request","request_id":"hook-7","request":{"subtype":"hook_callback","callback_id":"h1"}}`

	lineToolOK      = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01LgDc215WriEAa54mPYebjY","type":"tool_result","content":"File created successfully at: /tmp/work/hello.txt"}]},"session_id":"s1","tool_use_result":{"type":"create","filePath":"/tmp/work/hello.txt"}}`
	lineToolDenied  = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"User denied this action","is_error":true,"tool_use_id":"toolu_01LgDc215WriEAa54mPYebjY"}]},"session_id":"s1","tool_use_result":"Error: User denied this action"}`
	lineToolBlocks  = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":[{"type":"text","text":"line one"},{"type":"image","source":{}},{"type":"text","text":"line two"}]}]},"session_id":"s1"}`
	lineUserText    = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"session_id":"s1"}`
	lineUserStrCont = `{"type":"user","message":{"role":"user","content":"plain string turn"},"session_id":"s1"}`

	lineResultOK  = `{"duration_api_ms":3987,"stop_reason":"end_turn","session_id":"s1","total_cost_usd":0.0244249,"usage":{"input_tokens":18,"cache_creation_input_tokens":9312,"cache_read_input_tokens":38209,"output_tokens":197},"is_error":false,"num_turns":2,"subtype":"success","result":"Done.","type":"result","duration_ms":3567}`
	lineResultErr = `{"session_id":"s1","total_cost_usd":0.01,"usage":{"input_tokens":5,"output_tokens":1},"is_error":true,"num_turns":1,"subtype":"error_during_execution","result":"API error: overloaded","type":"result","duration_ms":900}`

	// Assistant messages carry the usage of the request that produced them,
	// which is where the size of the conversation shows up. The result
	// states the window through modelUsage.
	lineTextMsgUsage  = `{"type":"assistant","message":{"id":"msg_a","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":9099,"cache_read_input_tokens":13615,"output_tokens":4}},"session_id":"s1"}`
	lineTextMsgUsage2 = `{"type":"assistant","message":{"id":"msg_b","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"two"}],"usage":{"input_tokens":12,"cache_creation_input_tokens":9099,"cache_read_input_tokens":20000,"output_tokens":40}},"session_id":"s1"}`
	lineResultWindow  = `{"session_id":"s1","total_cost_usd":0.02,"usage":{"input_tokens":22,"output_tokens":44},"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":22,"outputTokens":44,"contextWindow":200000,"maxOutputTokens":32000}},"is_error":false,"subtype":"success","result":"Done.","type":"result","duration_ms":900}`
	lineResultSubs    = `{"session_id":"s1","total_cost_usd":0.02,"usage":{"input_tokens":22,"output_tokens":44},"modelUsage":{"claude-haiku-4-5-20251001":{"contextWindow":200000},"claude-sonnet-5":{"contextWindow":999}},"is_error":false,"subtype":"success","result":"Done.","type":"result","duration_ms":900}`
)

// feed runs lines through the parser and returns everything they emitted.
func feed(st *state, lines ...string) []agent.Event {
	var out []agent.Event
	for _, l := range lines {
		out = append(out, parseLine([]byte(l), st)...)
	}
	return out
}

func kinds(events []agent.Event) []agent.EventKind {
	out := make([]agent.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func only(t *testing.T, events []agent.Event, kind agent.EventKind) []agent.Event {
	t.Helper()
	var out []agent.Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func one(t *testing.T, events []agent.Event, kind agent.EventKind) agent.Event {
	t.Helper()
	got := only(t, events, kind)
	if len(got) != 1 {
		t.Fatalf("want exactly one %s, got %d in %v", kind, len(got), kinds(events))
	}
	return got[0]
}

func TestParseInitEmitsSessionInfo(t *testing.T) {
	st := newState(nil)
	ev := one(t, feed(st, lineInit), agent.KindSessionInfo)
	if got, want := ev.SessionInfo.ExternalID, "35450ae3-148d-4cc0-9493-76ae6036b29d"; got != want {
		t.Errorf("ExternalID = %q, want %q", got, want)
	}
	if got, want := ev.SessionInfo.Model, "claude-haiku-4-5-20251001"; got != want {
		t.Errorf("Model = %q, want %q", got, want)
	}
}

func TestParseAITitle(t *testing.T) {
	ev := one(t, feed(newState(nil), lineAITitle), agent.KindThreadTitle)
	if ev.ThreadTitle == nil || ev.ThreadTitle.Title != "Terminal UI addition" {
		t.Fatalf("thread title = %+v", ev.ThreadTitle)
	}
}

func TestParseIgnoresNoise(t *testing.T) {
	st := newState(nil)
	events := feed(st, lineStatus, lineThinkingToks, lineRateAllowed, lineCtlAck, lineUnknown, lineMsgDelta, lineMsgStop, lineSigDelta)
	if len(events) != 0 {
		t.Fatalf("want no events, got %v", kinds(events))
	}
	if len(st.outbox) != 0 {
		t.Fatalf("want empty outbox, got %d lines", len(st.outbox))
	}
}

func TestParseNotices(t *testing.T) {
	st := newState(nil)
	if got := one(t, feed(st, lineCompact), agent.KindNotice).Notice.Text; !strings.Contains(got, "compacted") {
		t.Errorf("compact notice = %q", got)
	}
	if got := one(t, feed(st, lineRateThrottled), agent.KindNotice).Notice.Text; !strings.Contains(got, "rejected") {
		t.Errorf("rate limit notice = %q", got)
	}
}

func TestParseStreamedTextIsNotDuplicated(t *testing.T) {
	st := newState(nil)
	st.turnID = "turn-1"
	events := feed(st,
		lineMsgStart,
		lineThinkStart, lineThinkEmpty, lineThinkDelta,
		lineTextStart, lineTextA, lineTextB,
		lineTextMsg, // the complete message arrives after the deltas
		lineTextStop, lineMsgStop,
	)

	texts := only(t, events, agent.KindTextDelta)
	if len(texts) != 2 {
		t.Fatalf("want 2 text deltas, got %d (%v)", len(texts), kinds(events))
	}
	var joined string
	for _, e := range texts {
		if e.TextDelta.MessageID != "msg_011CeiAxjHc8XT31ZgLc3G27" {
			t.Errorf("MessageID = %q", e.TextDelta.MessageID)
		}
		joined += e.TextDelta.Text
	}
	if want := "Done. Created `hello.txt`."; joined != want {
		t.Errorf("joined text = %q, want %q", joined, want)
	}

	think := one(t, events, agent.KindThinkingDelta)
	if want := "The user wants a file."; think.ThinkingDelta.Text != want {
		t.Errorf("thinking = %q, want %q", think.ThinkingDelta.Text, want)
	}
}

func TestParseFullTextWhenPartialsAreMissing(t *testing.T) {
	// Same message, but --include-partial-messages produced nothing.
	st := newState(nil)
	ev := one(t, feed(st, lineTextMsg), agent.KindTextDelta)
	if want := "Done. Created `hello.txt`."; ev.TextDelta.Text != want {
		t.Errorf("text = %q, want %q", ev.TextDelta.Text, want)
	}
}

func TestParseToolCallStreaming(t *testing.T) {
	st := newState(nil)
	events := feed(st,
		lineMsgStart,
		lineToolStart, lineToolIn1, lineToolIn2, lineToolIn3,
		lineToolMsg, // authoritative input arrives before content_block_stop
		lineToolStop,
	)

	started := only(t, events, agent.KindToolStarted)
	if len(started) != 2 {
		t.Fatalf("want 2 tool_started events, got %d (%v)", len(started), kinds(events))
	}
	first, second := started[0].ToolStarted, started[1].ToolStarted
	if first.ID != "toolu_01LgDc215WriEAa54mPYebjY" || first.Name != "Write" {
		t.Errorf("announcement = %+v", first)
	}
	if first.Input != nil {
		t.Errorf("announcement Input = %s, want nil", first.Input)
	}
	if second.ID != first.ID {
		t.Errorf("ids differ: %q vs %q", first.ID, second.ID)
	}
	var input map[string]string
	if err := json.Unmarshal(second.Input, &input); err != nil {
		t.Fatalf("final input is not JSON: %v (%s)", err, second.Input)
	}
	if input["file_path"] != "/tmp/work/hello.txt" || input["content"] != "hi" {
		t.Errorf("final input = %v", input)
	}
	if want := "/tmp/work/hello.txt"; second.Summary != want {
		t.Errorf("Summary = %q, want %q", second.Summary, want)
	}
}

func TestParseToolCallStreamingWithoutCompleteMessage(t *testing.T) {
	// content_block_stop must finalize the input on its own.
	st := newState(nil)
	events := feed(st, lineMsgStart, lineToolStart, lineToolIn1, lineToolIn2, lineToolIn3, lineToolStop)
	started := only(t, events, agent.KindToolStarted)
	if len(started) != 2 {
		t.Fatalf("want 2 tool_started events, got %d", len(started))
	}
	if got := started[1].ToolStarted.Summary; got != "/tmp/work/hello.txt" {
		t.Errorf("Summary = %q", got)
	}
}

func TestParseToolCallWithTruncatedInput(t *testing.T) {
	st := newState(nil)
	events := feed(st, lineMsgStart, lineToolStart, lineToolIn1, lineToolStop)
	started := only(t, events, agent.KindToolStarted)
	if len(started) != 2 {
		t.Fatalf("want 2 tool_started events, got %d", len(started))
	}
	if started[1].ToolStarted.Input != nil {
		t.Errorf("Input = %s, want nil for unparsable JSON", started[1].ToolStarted.Input)
	}
}

func TestParseToolResults(t *testing.T) {
	st := newState(nil)
	ok := one(t, feed(st, lineToolOK), agent.KindToolCompleted).ToolCompleted
	if ok.ID != "toolu_01LgDc215WriEAa54mPYebjY" || ok.Status != "done" || ok.IsError {
		t.Errorf("success result = %+v", ok)
	}
	if !strings.HasPrefix(ok.Output, "File created successfully") {
		t.Errorf("Output = %q", ok.Output)
	}

	// An error with no recorded denial is a failure, not a decline.
	st = newState(nil)
	failed := one(t, feed(st, lineToolDenied), agent.KindToolCompleted).ToolCompleted
	if failed.Status != "failed" || !failed.IsError {
		t.Errorf("error result = %+v", failed)
	}

	// Once the user has denied the tool, the same line reads as declined.
	st = newState(nil)
	st.denied["toolu_01LgDc215WriEAa54mPYebjY"] = true
	declined := one(t, feed(st, lineToolDenied), agent.KindToolCompleted).ToolCompleted
	if declined.Status != "declined" {
		t.Errorf("Status = %q, want declined", declined.Status)
	}
}

func TestParseToolResultContentBlocks(t *testing.T) {
	st := newState(nil)
	ev := one(t, feed(st, lineToolBlocks), agent.KindToolCompleted).ToolCompleted
	if want := "line one\nline two"; ev.Output != want {
		t.Errorf("Output = %q, want %q", ev.Output, want)
	}
}

func TestParseUserMessagesWithoutToolResults(t *testing.T) {
	st := newState(nil)
	if events := feed(st, lineUserText, lineUserStrCont); len(events) != 0 {
		t.Fatalf("want no events, got %v", kinds(events))
	}
}

func TestParseApprovalRequest(t *testing.T) {
	st := newState(nil)
	ev := one(t, feed(st, lineApproval), agent.KindApproval).Approval
	if ev.ID != "590e89a1-ab17-4fa4-bcb7-4a44035eb43d" {
		t.Errorf("ID = %q", ev.ID)
	}
	if ev.ToolID != "toolu_01LgDc215WriEAa54mPYebjY" || ev.ToolName != "Write" {
		t.Errorf("tool = %q/%q", ev.ToolID, ev.ToolName)
	}
	if ev.Description != "hello.txt" {
		t.Errorf("Description = %q", ev.Description)
	}
	pending, ok := st.approvals[ev.ID]
	if !ok {
		t.Fatal("approval was not recorded for Resolve")
	}
	if !json.Valid(pending.input) || !strings.Contains(string(pending.input), "hello.txt") {
		t.Errorf("stored input = %s", pending.input)
	}
	if len(st.outbox) != 0 {
		t.Errorf("approval must not be answered by the parser, outbox has %d lines", len(st.outbox))
	}
}

func TestParseUnsupportedControlRequestIsRefused(t *testing.T) {
	st := newState(nil)
	if events := feed(st, lineHookReq); len(events) != 0 {
		t.Fatalf("want no events, got %v", kinds(events))
	}
	sent := st.takeOutbox()
	if len(sent) != 1 {
		t.Fatalf("want 1 outgoing line, got %d", len(sent))
	}
	var got map[string]any
	if err := json.Unmarshal(sent[0], &got); err != nil {
		t.Fatalf("outgoing line is not JSON: %v", err)
	}
	resp := got["response"].(map[string]any)
	if got["type"] != "control_response" || resp["subtype"] != "error" || resp["request_id"] != "hook-7" {
		t.Errorf("refusal = %s", sent[0])
	}
	if st.takeOutbox() != nil {
		t.Error("outbox was not drained")
	}
}

func TestParseResult(t *testing.T) {
	st := newState(nil)
	st.turnID = "turn-1"
	ev := one(t, feed(st, lineResultOK), agent.KindTurnCompleted).TurnCompleted
	want := agent.TurnCompleted{
		TurnID:       "turn-1",
		Status:       "done",
		DurationMS:   3567,
		CostUSD:      0.0244249,
		InputTokens:  18 + 9312 + 38209,
		OutputTokens: 197,
	}
	if *ev != want {
		t.Errorf("TurnCompleted = %+v, want %+v", *ev, want)
	}
	if st.turnID != "" {
		t.Error("turn state was not reset")
	}
}

func TestParseAssistantReportsContextUsage(t *testing.T) {
	st := newState(nil)
	// init names the model, which is enough for a window before any turn
	// has finished.
	events := feed(st, lineInit, lineTextMsgUsage)
	ev := one(t, events, agent.KindContextUsage).ContextUsage
	if got, want := ev.Tokens, int64(10+9099+13615+4); got != want {
		t.Errorf("Tokens = %d, want %d", got, want)
	}
	if got, want := ev.Window, int64(standardWindow); got != want {
		t.Errorf("Window = %d, want %d", got, want)
	}
	// The same message again is not news; a bigger one is.
	if got := only(t, feed(st, lineTextMsgUsage), agent.KindContextUsage); len(got) != 0 {
		t.Errorf("repeated reading emitted %d events", len(got))
	}
	next := one(t, feed(st, lineTextMsgUsage2), agent.KindContextUsage).ContextUsage
	if got, want := next.Tokens, int64(12+9099+20000+40); got != want {
		t.Errorf("Tokens = %d, want %d", got, want)
	}
}

func TestParseCompactionMovesTheMeter(t *testing.T) {
	st := newState(nil)
	st.turnID = "turn-c"
	feed(st, lineInit, lineTextMsgUsage)
	events := feed(st, lineCompacting, lineCompactedOK, lineCompactSizes, lineCompactResult)
	want := []agent.EventKind{agent.KindNotice, agent.KindContextUsage, agent.KindTurnCompleted}
	if got := kinds(events); !slices.Equal(got, want) {
		t.Fatalf("compaction events = %v, want %v", got, want)
	}
	if got := one(t, events, agent.KindContextUsage).ContextUsage; got.Tokens != 1046 || got.Window != standardWindow {
		t.Errorf("reading after compaction = %+v", got)
	}
	if got := one(t, events, agent.KindTurnCompleted).TurnCompleted; got.Status != "done" || got.InputTokens != 0 {
		t.Errorf("compaction turn = %+v", got)
	}
	// A boundary without sizes (older CLIs, auto compaction) is only a notice.
	if got := kinds(feed(st, lineCompact)); !slices.Equal(got, []agent.EventKind{agent.KindNotice}) {
		t.Errorf("bare boundary = %v", got)
	}
}

func TestParseResultCorrectsContextWindow(t *testing.T) {
	st := newState(nil)
	st.turnID = "turn-1"
	// A 1M model guessed from the id, corrected to what the turn ran with.
	feed(st, `{"type":"system","subtype":"init","session_id":"s1","model":"claude-haiku-4-5-20251001"}`)
	st.window = longWindow
	feed(st, lineTextMsgUsage)
	ev := one(t, feed(st, lineResultWindow), agent.KindContextUsage).ContextUsage
	if got, want := ev.Window, int64(200000); got != want {
		t.Errorf("Window = %d, want %d", got, want)
	}
	if got, want := ev.Tokens, int64(10+9099+13615+4); got != want {
		t.Errorf("Tokens = %d, want the last message's %d", got, want)
	}
	// A second identical result says nothing new.
	if got := only(t, feed(st, lineResultWindow), agent.KindContextUsage); len(got) != 0 {
		t.Errorf("unchanged window emitted %d events", len(got))
	}
}

func TestParseResultPicksTheThreadsModelWindow(t *testing.T) {
	st := newState(nil)
	// A subagent ran on another model; the window of the one the
	// conversation is on is the one that counts.
	feed(st, lineTextMsgUsage, lineResultSubs)
	if got, want := st.window, int64(200000); got != want {
		t.Errorf("window = %d, want %d", got, want)
	}
}

func TestContextWindowFor(t *testing.T) {
	cases := []struct {
		names []string
		want  int64
	}{
		{[]string{"claude-opus-5[1m]"}, longWindow},
		{[]string{"opus[1m]", "claude-opus-5[1m]"}, longWindow},
		{[]string{"claude-fable-5-1[1m]", "claude-fable-5-1"}, longWindow},
		{[]string{"default", "claude-opus-5", "Opus 5 with 1M context · Best for everyday tasks"}, longWindow},
		{[]string{"sonnet", "claude-sonnet-5", "Sonnet 5 · Efficient for routine tasks"}, standardWindow},
		{[]string{"haiku"}, standardWindow},
		{nil, standardWindow},
	}
	for _, c := range cases {
		if got := contextWindowFor(c.names...); got != c.want {
			t.Errorf("contextWindowFor(%q) = %d, want %d", c.names, got, c.want)
		}
	}
}

func TestParseResultError(t *testing.T) {
	st := newState(nil)
	st.turnID = "turn-2"
	ev := one(t, feed(st, lineResultErr), agent.KindTurnCompleted).TurnCompleted
	if ev.Status != "error" {
		t.Errorf("Status = %q, want error", ev.Status)
	}
	if ev.Error != "API error: overloaded" {
		t.Errorf("Error = %q", ev.Error)
	}
}

func TestParseResultAfterInterrupt(t *testing.T) {
	st := newState(nil)
	st.turnID = "turn-3"
	st.interrupted = true
	ev := one(t, feed(st, lineResultOK), agent.KindTurnCompleted).TurnCompleted
	if ev.Status != "interrupted" {
		t.Errorf("Status = %q, want interrupted", ev.Status)
	}
	if st.interrupted {
		t.Error("interrupt flag survived the turn")
	}
}

func TestParseFullTurnOrder(t *testing.T) {
	st := newState(nil)
	st.turnID = "turn-1"
	events := feed(st,
		lineInit, lineStatus, lineMsgStart, lineRateAllowed,
		lineThinkStart, lineThinkEmpty, lineThinkDelta, lineSigDelta, lineThinkEmpty,
		lineToolStart, lineToolIn1, lineToolIn2, lineToolIn3, lineToolMsg, lineToolStop,
		lineApproval, lineToolOK,
		lineMsgStart, lineTextStart, lineTextA, lineTextB, lineTextMsg, lineTextStop,
		lineMsgDelta, lineMsgStop, lineResultOK,
	)
	want := []agent.EventKind{
		agent.KindSessionInfo,
		agent.KindThinkingDelta,
		agent.KindToolStarted, // announcement, no input yet
		agent.KindToolStarted, // full input
		agent.KindApproval,
		agent.KindToolCompleted,
		agent.KindTextDelta,
		agent.KindTextDelta,
		agent.KindTurnCompleted,
	}
	if got := kinds(events); !slices.Equal(got, want) {
		t.Errorf("event order =\n  %v\nwant\n  %v", got, want)
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"bash command", "Bash", `{"command":"go test ./...","description":"Run tests"}`, "go test ./..."},
		{"bash without command", "Bash", `{"description":"Run tests"}`, "Run tests"},
		{"bash multiline", "Bash", `{"command":"set -e\n  go build ./...\n"}`, "set -e go build ./..."},
		{"write", "Write", `{"file_path":"/tmp/a.txt","content":"hi"}`, "/tmp/a.txt"},
		{"read", "Read", `{"file_path":"/tmp/a.txt"}`, "/tmp/a.txt"},
		{"edit", "Edit", `{"file_path":"/tmp/a.txt","old_string":"a","new_string":"b"}`, "/tmp/a.txt"},
		{"notebook", "NotebookEdit", `{"file_path":"/tmp/a.ipynb","new_source":"x"}`, "/tmp/a.ipynb"},
		{"glob", "Glob", `{"pattern":"**/*.go","path":"/tmp"}`, "**/*.go"},
		{"grep", "Grep", `{"pattern":"func main","output_mode":"content"}`, "func main"},
		{"webfetch", "WebFetch", `{"url":"https://example.com","prompt":"summarize"}`, "https://example.com"},
		{"task", "Task", `{"description":"Find the bug","prompt":"long..."}`, "Find the bug"},
		{"unknown tool", "Mystery", `{"zeta":"last","alpha":"first"}`, "first"},
		{"no strings", "Mystery", `{"count":3,"deep":{"a":"b"}}`, ""},
		{"empty input", "Bash", ``, ""},
		{"bad json", "Bash", `{"command":`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := summarize(c.tool, json.RawMessage(c.input)); got != c.want {
				t.Errorf("summarize(%q, %s) = %q, want %q", c.tool, c.input, got, c.want)
			}
		})
	}
}

func TestSummarizeIsCapped(t *testing.T) {
	long, err := json.Marshal(map[string]string{"command": strings.Repeat("x", 500)})
	if err != nil {
		t.Fatal(err)
	}
	got := summarize("Bash", long)
	if len([]rune(got)) != summaryLimit {
		t.Errorf("length = %d, want %d", len([]rune(got)), summaryLimit)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("want an elided summary, got %q", got)
	}
}

func TestOutgoingMessageShapes(t *testing.T) {
	// These are the exact shapes claude 2.1.252 accepts.
	cases := []struct {
		name string
		line []byte
		want string
	}{
		{
			"user turn",
			userTurn("hello"),
			`{"message":{"content":"hello","role":"user"},"type":"user"}`,
		},
		{
			"allow",
			approvalResponse("req-1", agent.Allow, json.RawMessage(`{"file_path":"/tmp/a.txt"}`)),
			`{"response":{"request_id":"req-1","response":{"behavior":"allow","updatedInput":{"file_path":"/tmp/a.txt"}},"subtype":"success"},"type":"control_response"}`,
		},
		{
			"allow without input",
			approvalResponse("req-1", agent.Allow, nil),
			`{"response":{"request_id":"req-1","response":{"behavior":"allow","updatedInput":{}},"subtype":"success"},"type":"control_response"}`,
		},
		{
			"deny",
			approvalResponse("req-1", agent.Deny, json.RawMessage(`{"file_path":"/tmp/a.txt"}`)),
			`{"response":{"request_id":"req-1","response":{"behavior":"deny","message":"User denied this action"},"subtype":"success"},"type":"control_response"}`,
		},
		{
			"interrupt",
			interruptRequest("int-1"),
			`{"request":{"subtype":"interrupt"},"request_id":"int-1","type":"control_request"}`,
		},
		{
			"set permission mode",
			setModeRequest("mode-1", "acceptEdits"),
			`{"request":{"mode":"acceptEdits","subtype":"set_permission_mode"},"request_id":"mode-1","type":"control_request"}`,
		},
		{
			"control error",
			controlError("hook-7", "unsupported"),
			`{"response":{"error":"unsupported","request_id":"hook-7","subtype":"error"},"type":"control_response"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.HasSuffix(string(c.line), "\n") {
				t.Fatalf("line is not newline terminated: %q", c.line)
			}
			if got := strings.TrimSuffix(string(c.line), "\n"); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

func TestSetPermissionModeOnlyReturnsToBypass(t *testing.T) {
	var out bytes.Buffer
	s := &session{stdin: nopWriteCloser{&out}, dying: make(chan struct{}), exited: make(chan struct{})}

	// The CLI ignores a switch into bypassPermissions unless it started
	// there, so the adapter says so instead of pretending it happened.
	if err := s.SetPermissionMode(context.Background(), "bypassPermissions"); !errors.Is(err, agent.ErrModeNextTurn) {
		t.Fatalf("SetPermissionMode(bypass) err = %v, want ErrModeNextTurn", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %q for a refused switch", out.String())
	}
	if err := s.SetPermissionMode(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"mode":"default","subtype":"set_permission_mode"`) {
		t.Errorf("empty mode wrote %q, want default", out.String())
	}

	out.Reset()
	s.bypassOK = true
	if err := s.SetPermissionMode(context.Background(), "bypassPermissions"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"mode":"bypassPermissions"`) {
		t.Errorf("wrote %q", out.String())
	}

	close(s.exited)
	if err := s.SetPermissionMode(context.Background(), "plan"); err == nil {
		t.Error("SetPermissionMode on an exited session did not fail")
	}
}

func TestNewUUIDShape(t *testing.T) {
	id := newUUID()
	if len(id) != 36 {
		t.Fatalf("len = %d, want 36 (%q)", len(id), id)
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || id[14] != '4' {
		t.Errorf("not a v4 uuid: %q", id)
	}
	if newUUID() == id {
		t.Error("ids repeat")
	}
}

func TestChildEnvDropsNestedLaunchGuard(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_SSE_PORT", "1234")
	t.Setenv("CLAUDE_CONFIG_DIR", "/keep/me")
	t.Setenv("PATH", os.Getenv("PATH"))

	var kept []string
	for _, kv := range childEnv(nil) {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "CLAUDECODE", strings.HasPrefix(name, "CLAUDE_CODE_"):
			t.Errorf("%s survived", name)
		case name == "CLAUDE_CONFIG_DIR", name == "PATH":
			kept = append(kept, name)
		}
	}
	if len(kept) != 2 {
		t.Errorf("unrelated variables were dropped, kept %v", kept)
	}
}

func TestPersistedTitle(t *testing.T) {
	config := t.TempDir()
	sessionID := "35450ae3-148d-4cc0-9493-76ae6036b29d"
	dir := filepath.Join(config, "projects", "-tmp-work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"user","sessionId":"` + sessionID + `"}`,
		`{"type":"ai-title","aiTitle":" First title ","sessionId":"` + sessionID + `"}`,
		`{"type":"ai-title","aiTitle":"Updated title","sessionId":"` + sessionID + `"}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := persistedTitle(config, sessionID); got != "Updated title" {
		t.Fatalf("persistedTitle = %q", got)
	}
	for _, invalid := range []string{"", "../escape", "session*"} {
		if got := persistedTitle(config, invalid); got != "" {
			t.Fatalf("persistedTitle(%q) = %q", invalid, got)
		}
	}
}

func TestTitleReaderReadsOnlyNewBytes(t *testing.T) {
	config := t.TempDir()
	sessionID := "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d"
	dir := filepath.Join(config, "projects", "-tmp-work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	write := func(s string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.WriteString(s)
	}
	write(`{"type":"user","sessionId":"` + sessionID + `"}` + "\n")
	var r titleReader
	if got := r.read(config, sessionID); got != "" {
		t.Fatalf("title before any record = %q", got)
	}
	// A half-written line stays unread until its newline arrives.
	write(`{"type":"ai-title","aiTitle":"Half`)
	if got := r.read(config, sessionID); got != "" {
		t.Fatalf("title from a partial line = %q", got)
	}
	write(` done","sessionId":"` + sessionID + `"}` + "\n")
	if got := r.read(config, sessionID); got != "Half done" {
		t.Fatalf("title after completing the line = %q", got)
	}
	size, _ := os.Stat(path)
	if r.offset != size.Size() {
		t.Fatalf("offset %d, want %d (everything consumed)", r.offset, size.Size())
	}
	// Nothing new: same answer, no re-read of earlier bytes.
	if got := r.read(config, sessionID); got != "Half done" || r.offset != size.Size() {
		t.Fatalf("idle read = %q at %d", got, r.offset)
	}
	write(`{"type":"ai-title","aiTitle":"Renamed","sessionId":"` + sessionID + `"}`)
	if got := r.read(config, sessionID); got != "Renamed" {
		t.Fatalf("title after an unterminated but complete record = %q", got)
	}
}

func TestTailBufferKeepsTail(t *testing.T) {
	b := &tailBuffer{max: 8}
	b.Write([]byte("abcdef"))
	b.Write([]byte("ghijkl"))
	if got := b.String(); got != "efghijkl" {
		t.Errorf("String() = %q, want %q", got, "efghijkl")
	}
}

func TestAgentName(t *testing.T) {
	if got := New().Name(); got != "claude" {
		t.Errorf("Name() = %q", got)
	}
}

func TestStartRejectsMissingBinary(t *testing.T) {
	a := New(WithBinary(filepath.Join(t.TempDir(), "not-a-real-binary")))
	if _, err := a.Start(t.Context(), agent.Config{Cwd: t.TempDir()}); err == nil {
		t.Fatal("want an error for a missing binary")
	}
}

// Integration tests. They run the real CLI, so they need it on PATH and are
// skipped under -short.

const integrationTimeout = 90 * time.Second

func startIntegration(t *testing.T, dir string) *session {
	return startIntegrationCfg(t, agent.Config{Cwd: dir, Model: "haiku"})
}

func startIntegrationCfg(t *testing.T, cfg agent.Config) *session {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test needs the claude CLI")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	sess, err := New(WithLogger(log)).Start(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() {
		s.Close()
		if t.Failed() {
			if tail := s.stderrTail(); tail != "" {
				t.Logf("claude stderr:\n%s", tail)
			}
		}
	})
	return s
}

// waitFor drains events until one matches, recording everything it saw.
func waitFor(t *testing.T, s *session, seen *[]agent.Event, what string, match func(agent.Event) bool) agent.Event {
	t.Helper()
	deadline := time.After(integrationTimeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events closed while waiting for %s; saw %v", what, kinds(*seen))
			}
			*seen = append(*seen, ev)
			if ev.Kind == agent.KindClosed {
				t.Fatalf("session closed while waiting for %s: %v", what, ev.Closed.Err)
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; saw %v", what, kinds(*seen))
		}
	}
}

func isKind(k agent.EventKind) func(agent.Event) bool {
	return func(ev agent.Event) bool { return ev.Kind == k }
}

func TestIntegrationSimpleTurn(t *testing.T) {
	s := startIntegration(t, t.TempDir())
	var seen []agent.Event

	// The CLI stays silent until it has a turn to work on, so the init line
	// (and with it SessionInfo) only arrives after the first Send.
	if err := s.Send(t.Context(), "Reply with exactly the word pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	turn := waitFor(t, s, &seen, "turn start", isKind(agent.KindTurnStarted)).TurnStarted

	info := waitFor(t, s, &seen, "session info", isKind(agent.KindSessionInfo))
	if info.SessionInfo.ExternalID == "" {
		t.Error("SessionInfo.ExternalID is empty")
	}
	if info.SessionInfo.Model == "" {
		t.Error("SessionInfo.Model is empty")
	}

	done := waitFor(t, s, &seen, "turn completion", isKind(agent.KindTurnCompleted)).TurnCompleted

	if done.TurnID != turn.TurnID {
		t.Errorf("TurnID = %q, want %q", done.TurnID, turn.TurnID)
	}
	if done.Status != "done" {
		t.Errorf("Status = %q (%s)", done.Status, done.Error)
	}
	if done.DurationMS <= 0 || done.InputTokens <= 0 || done.OutputTokens <= 0 {
		t.Errorf("usage looks wrong: %+v", *done)
	}

	var text string
	for _, ev := range seen {
		if ev.Kind == agent.KindTextDelta {
			text += ev.TextDelta.Text
		}
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Errorf("assistant text = %q, want it to contain pong", text)
	}
}

func TestIntegrationApprovalAllow(t *testing.T) {
	dir := t.TempDir()
	s := startIntegration(t, dir)
	var seen []agent.Event

	if err := s.Send(t.Context(), "Create a file named hello.txt containing hi, using the Write tool. Do not ask questions."); err != nil {
		t.Fatalf("Send: %v", err)
	}
	approval := waitFor(t, s, &seen, "approval", func(ev agent.Event) bool {
		return ev.Kind == agent.KindApproval && ev.Approval.ToolName == "Write"
	}).Approval
	if approval.ToolID == "" {
		t.Error("approval has no ToolID")
	}
	if !strings.Contains(string(approval.Input), "hello.txt") {
		t.Errorf("approval Input = %s", approval.Input)
	}

	if err := s.Resolve(t.Context(), approval.ID, agent.Allow); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	completed := waitFor(t, s, &seen, "tool completion", func(ev agent.Event) bool {
		return ev.Kind == agent.KindToolCompleted && ev.ToolCompleted.ID == approval.ToolID
	}).ToolCompleted
	if completed.Status != "done" || completed.IsError {
		t.Errorf("tool completed = %+v", *completed)
	}

	done := waitFor(t, s, &seen, "turn completion", isKind(agent.KindTurnCompleted)).TurnCompleted
	if done.Status != "done" {
		t.Errorf("Status = %q (%s)", done.Status, done.Error)
	}

	body, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("hello.txt was not written: %v", err)
	}
	if !strings.Contains(string(body), "hi") {
		t.Errorf("hello.txt = %q", body)
	}
}

func TestIntegrationApprovalDeny(t *testing.T) {
	dir := t.TempDir()
	s := startIntegration(t, dir)
	var seen []agent.Event

	if err := s.Send(t.Context(), "Create a file named hello.txt containing hi, using the Write tool. Do not ask questions."); err != nil {
		t.Fatalf("Send: %v", err)
	}
	approval := waitFor(t, s, &seen, "approval", func(ev agent.Event) bool {
		return ev.Kind == agent.KindApproval && ev.Approval.ToolName == "Write"
	}).Approval

	if err := s.Resolve(t.Context(), approval.ID, agent.Deny); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	completed := waitFor(t, s, &seen, "tool completion", func(ev agent.Event) bool {
		return ev.Kind == agent.KindToolCompleted && ev.ToolCompleted.ID == approval.ToolID
	}).ToolCompleted
	if completed.Status != "declined" {
		t.Errorf("Status = %q, want declined", completed.Status)
	}

	waitFor(t, s, &seen, "turn completion", isKind(agent.KindTurnCompleted))

	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); !os.IsNotExist(err) {
		t.Errorf("hello.txt exists after a denial (stat err = %v)", err)
	}
}

func TestIntegrationInterrupt(t *testing.T) {
	s := startIntegrationCfg(t, agent.Config{
		Cwd:            t.TempDir(),
		Model:          "haiku",
		PermissionMode: "bypassPermissions", // the point here is the interrupt, not approvals
	})
	var seen []agent.Event

	if err := s.Send(t.Context(), "Run `sleep 120` with the Bash tool. Do not ask questions."); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, s, &seen, "bash tool call", func(ev agent.Event) bool {
		return ev.Kind == agent.KindToolStarted && ev.ToolStarted.Name == "Bash"
	})
	if err := s.Interrupt(t.Context()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	done := waitFor(t, s, &seen, "turn completion", isKind(agent.KindTurnCompleted)).TurnCompleted
	if done.Status != "interrupted" {
		t.Errorf("Status = %q, want interrupted (%s)", done.Status, done.Error)
	}
}

func TestIntegrationCloseEndsSession(t *testing.T) {
	s := startIntegration(t, t.TempDir())

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	deadline := time.After(integrationTimeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return // channel closed after Closed, as promised
			}
			if ev.Kind == agent.KindClosed {
				if ev.Closed.Err != nil {
					t.Errorf("Closed.Err = %v, want nil after Close", ev.Closed.Err)
				}
			}
		case <-deadline:
			t.Fatal("events channel was not closed")
		}
	}
}

func TestIntegrationSendAfterClose(t *testing.T) {
	s := startIntegration(t, t.TempDir())

	// Drain so Close is not blocked behind an unread event.
	go func() {
		for range s.Events() {
		}
	}()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Send(context.Background(), "hello"); err == nil {
		t.Error("Send after Close returned no error")
	}
}
