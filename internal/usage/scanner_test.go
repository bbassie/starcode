package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseClaudeLine(t *testing.T) {
	line := []byte(`{"type":"assistant","timestamp":"2026-09-04T12:00:00Z","sessionId":"s1","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":4}}}`)
	entry, ok := parseClaudeLine(line)
	if !ok {
		t.Fatal("line was not parsed")
	}
	if entry.Agent != "claude" || entry.Model != "claude-sonnet-5" || entry.SessionID != "s1" || entry.dedupeKey != "m1:r1" {
		t.Errorf("identity = %+v", entry)
	}
	if entry.Tokens != (Tokens{UncachedInput: 10, CachedInput: 20, CacheCreation: 30, Output: 4}) {
		t.Errorf("tokens = %+v", entry.Tokens)
	}
}

func TestParseCodexLine(t *testing.T) {
	state := codexState{}
	lines := []string{
		`{"type":"session_meta","timestamp":"2026-09-04T12:00:00Z","payload":{"id":"s1"}}`,
		`{"type":"turn_context","timestamp":"2026-09-04T12:00:01Z","payload":{"model":"gpt-5.6-sol"}}`,
		`{"type":"event_msg","timestamp":"2026-09-04T12:00:02Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":70,"cache_write_input_tokens":10,"output_tokens":20,"reasoning_output_tokens":5}}}}`,
	}
	for _, line := range lines[:2] {
		if _, ok := parseCodexLine([]byte(line), &state); ok {
			t.Fatal("metadata produced usage")
		}
	}
	entry, ok := parseCodexLine([]byte(lines[2]), &state)
	if !ok {
		t.Fatal("usage line was not parsed")
	}
	if entry.Agent != "codex" || entry.Model != "gpt-5.6-sol" || entry.SessionID != "s1" {
		t.Errorf("identity = %+v", entry)
	}
	if entry.Tokens != (Tokens{UncachedInput: 20, CachedInput: 70, CacheCreation: 10, Output: 20, Reasoning: 5}) {
		t.Errorf("tokens = %+v", entry.Tokens)
	}
	if _, ok := parseCodexLine([]byte(lines[2]), &state); ok {
		t.Error("consecutive duplicate was counted")
	}
}

func TestPricing(t *testing.T) {
	document := map[string]json.RawMessage{
		"model-x": json.RawMessage(`{"input_cost_per_token":0.01,"output_cost_per_token":0.02,"cache_read_input_token_cost":0.001,"cache_creation_input_token_cost":0.015}`),
	}
	rate, ok := lookupRate(parseRates(document), "provider/model-x")
	if !ok {
		t.Fatal("rate not found")
	}
	got := price(rate, Tokens{UncachedInput: 10, CachedInput: 20, CacheCreation: 2, Output: 3})
	if math.Abs(got-0.21) > 1e-9 {
		t.Errorf("price = %v", got)
	}
}

func TestScannerDeduplicatesClaudeTranscripts(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, "claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","timestamp":"2026-09-04T12:00:00Z","sessionId":"s1","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":4}}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "one.jsonl"), []byte(line+"\n"+line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New("")
	s.claudeDir = claudeDir
	s.codexDir = filepath.Join(root, "missing")
	s.rates = rateTable{"claude-sonnet-5": {input: 0.01, output: 0.02, cacheRead: 0.001, cacheCreation: 0.015}}
	s.ratesAt = time.Now()
	s.status = "test"
	result, err := s.Read(t.Context(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].CostUSD != 0.18 {
		t.Fatalf("result = %+v", result)
	}
}

func TestScannerAttributesRootsToInstances(t *testing.T) {
	root := t.TempDir()
	line := `{"type":"assistant","timestamp":"2026-09-04T12:00:00Z","sessionId":"%s","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":4}}}`
	dirs := map[string]string{"claude": filepath.Join(root, "home", "projects"), "work-claude": filepath.Join(root, "work", "projects")}
	for name, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "one.jsonl"), []byte(fmt.Sprintf(line, name)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := New("")
	s.rates, s.ratesAt, s.status = rateTable{}, time.Now(), "test"
	s.SetRoots([]Root{
		{Agent: "claude", Driver: "claude", Dir: dirs["claude"]},
		{Agent: "work-claude", Driver: "claude", Dir: dirs["work-claude"]},
		{Agent: "claude-again", Driver: "claude", Dir: dirs["claude"]}, // shares a dir: counted once
	})
	result, err := s.Read(t.Context(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range result.Entries {
		got[e.Agent] = e.SessionID
	}
	if len(result.Entries) != 2 || got["claude"] != "claude" || got["work-claude"] != "work-claude" {
		t.Fatalf("entries = %+v", result.Entries)
	}
}

func TestLocalSystemUsage(t *testing.T) {
	if os.Getenv("STARCODE_TEST_SYSTEM_USAGE") == "" {
		t.Skip("set STARCODE_TEST_SYSTEM_USAGE=1 to scan local provider transcripts")
	}
	result, err := New("").Read(t.Context(), time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	var claude, codex int
	var tokens int64
	var cost float64
	for _, entry := range result.Entries {
		tokens += entry.Tokens.Total()
		cost += entry.CostUSD
		switch entry.Agent {
		case "claude":
			claude++
		case "codex":
			codex++
		}
	}
	t.Logf("records=%d claude=%d codex=%d tokens=%d cost=%.2f unpriced=%d pricing=%s", len(result.Entries), claude, codex, tokens, cost, result.Unpriced, result.PricingStatus)
}
