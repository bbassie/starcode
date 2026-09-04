// Package usage reads usage from the local agent CLIs' append-only transcripts.
// This covers sessions started outside Starcode as well as sessions it launched.
package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const transcriptMTimeSlack = 36 * time.Hour

type Tokens struct {
	UncachedInput int64
	CachedInput   int64
	CacheCreation int64
	Output        int64
	Reasoning     int64
}

func (t Tokens) Total() int64 {
	return t.UncachedInput + t.CachedInput + t.CacheCreation + t.Output
}

type Entry struct {
	Agent        string
	Model        string
	SessionID    string
	CreatedAt    time.Time
	Tokens       Tokens
	CostUSD      float64
	CacheSavings float64
	Priced       bool
	dedupeKey    string
}

type Result struct {
	Entries       []Entry
	PricingStatus string
	Unpriced      int
}

type cachedFile struct {
	size    int64
	modTime time.Time
	entries []Entry
}

type Scanner struct {
	mu           sync.Mutex
	files        map[string]cachedFile
	cacheDir     string
	client       *http.Client
	rates        rateTable
	ratesAt      time.Time
	ratesChecked time.Time
	status       string
	// Tests can override discovery without mutating process-wide environment.
	claudeDir string
	codexDir  string
}

func New(cacheDir string) *Scanner {
	return &Scanner{files: make(map[string]cachedFile), cacheDir: cacheDir, client: &http.Client{Timeout: 5 * time.Second}}
}

func (s *Scanner) Read(ctx context.Context, since time.Time) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rates, status := s.loadRates(ctx)
	roots := s.transcriptRoots()
	var entries []Entry
	for agent, root := range roots {
		files, err := transcriptFiles(root, since.Add(-transcriptMTimeSlack))
		if err != nil {
			return Result{}, err
		}
		for _, path := range files {
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			cached, ok := s.files[path]
			if !ok || cached.size != info.Size() || !cached.modTime.Equal(info.ModTime()) {
				parsed, err := parseTranscript(path, agent)
				if err != nil {
					continue
				}
				cached = cachedFile{size: info.Size(), modTime: info.ModTime(), entries: parsed}
				s.files[path] = cached
			}
			entries = append(entries, cached.entries...)
		}
	}

	seen := make(map[string]bool)
	out := Result{PricingStatus: status}
	for _, entry := range entries {
		if entry.CreatedAt.Before(since) || entry.Tokens.Total() == 0 {
			continue
		}
		if entry.dedupeKey != "" {
			key := entry.Agent + "\x00" + entry.dedupeKey
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		if !entry.Priced {
			if rate, ok := lookupRate(rates, entry.Model); ok {
				entry.CostUSD = price(rate, entry.Tokens)
				entry.CacheSavings = max(0, float64(entry.Tokens.CachedInput)*(rate.input-rate.cacheRead))
				entry.Priced = true
			} else {
				out.Unpriced++
			}
		}
		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}

func (s *Scanner) transcriptRoots() map[string]string {
	if s.claudeDir != "" || s.codexDir != "" {
		return map[string]string{"claude": s.claudeDir, "codex": s.codexDir}
	}
	home, _ := os.UserHomeDir()
	claudeHome := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return map[string]string{
		"claude": filepath.Join(claudeHome, "projects"),
		"codex":  filepath.Join(codexHome, "sessions"),
	}
}

func transcriptFiles(root string, modifiedAfter time.Time) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err == nil && !info.ModTime().Before(modifiedAfter) {
			out = append(out, path)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

func parseTranscript(path, agent string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 128*1024)
	var out []Entry
	state := codexState{}
	for {
		line, readErr := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			switch agent {
			case "claude":
				if bytes.Contains(line, []byte(`"usage"`)) {
					if entry, ok := parseClaudeLine(line); ok {
						out = append(out, entry)
					}
				}
			case "codex":
				if bytes.Contains(line, []byte(`"token_count"`)) || bytes.Contains(line, []byte(`"turn_context"`)) || bytes.Contains(line, []byte(`"session_meta"`)) {
					if entry, ok := parseCodexLine(line, &state); ok {
						out = append(out, entry)
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readErr
		}
	}
	return out, nil
}

func positive(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

type claudeLine struct {
	Type      string   `json:"type"`
	Timestamp string   `json:"timestamp"`
	SessionID string   `json:"sessionId"`
	RequestID string   `json:"requestId"`
	CostUSD   *float64 `json:"costUSD"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			Input         int64 `json:"input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			Output        int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func parseClaudeLine(line []byte) (Entry, bool) {
	var raw claudeLine
	if json.Unmarshal(line, &raw) != nil || raw.Type != "assistant" || raw.Message.Model == "" {
		return Entry{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp)
	if err != nil {
		return Entry{}, false
	}
	dedupe := ""
	if raw.Message.ID != "" || raw.RequestID != "" {
		dedupe = raw.Message.ID + ":" + raw.RequestID
	}
	entry := Entry{
		Agent:     "claude",
		Model:     raw.Message.Model,
		SessionID: raw.SessionID,
		CreatedAt: ts,
		Tokens: Tokens{
			UncachedInput: positive(raw.Message.Usage.Input),
			CachedInput:   positive(raw.Message.Usage.CacheRead),
			CacheCreation: positive(raw.Message.Usage.CacheCreation),
			Output:        positive(raw.Message.Usage.Output),
		},
		dedupeKey: dedupe,
	}
	if raw.CostUSD != nil && *raw.CostUSD >= 0 {
		entry.CostUSD = *raw.CostUSD
		entry.Priced = true
	}
	return entry, true
}

type codexState struct {
	model          string
	sessionID      string
	lastUsage      string
	sawMeta        bool
	suppressFork   bool
	forkCopyAnchor time.Time
}

func parseCodexLine(line []byte, state *codexState) (Entry, bool) {
	var outer struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Payload   json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &outer) != nil {
		return Entry{}, false
	}
	switch outer.Type {
	case "session_meta":
		if state.sawMeta {
			return Entry{}, false
		}
		state.sawMeta = true
		var payload struct {
			ID           string `json:"id"`
			SessionID    string `json:"session_id"`
			ForkedFromID string `json:"forked_from_id"`
			Source       struct {
				Subagent struct {
					ThreadSpawn struct {
						ParentThreadID string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subagent"`
			} `json:"source"`
		}
		if json.Unmarshal(outer.Payload, &payload) == nil {
			state.sessionID = payload.ID
			if state.sessionID == "" {
				state.sessionID = payload.SessionID
			}
			if payload.ForkedFromID != "" || payload.Source.Subagent.ThreadSpawn.ParentThreadID != "" {
				state.suppressFork = true
				state.forkCopyAnchor, _ = time.Parse(time.RFC3339Nano, outer.Timestamp)
			}
		}
		return Entry{}, false
	case "turn_context":
		var payload struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(outer.Payload, &payload) == nil && payload.Model != "" {
			state.model = payload.Model
		}
		return Entry{}, false
	}

	var payload struct {
		Type string `json:"type"`
		Info struct {
			Last json.RawMessage `json:"last_token_usage"`
		} `json:"info"`
	}
	if json.Unmarshal(outer.Payload, &payload) != nil || payload.Type != "token_count" || len(payload.Info.Last) == 0 || state.model == "" {
		return Entry{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, outer.Timestamp)
	if err != nil {
		return Entry{}, false
	}
	signature := string(payload.Info.Last)
	if signature == state.lastUsage {
		return Entry{}, false
	}
	state.lastUsage = signature
	if state.suppressFork {
		if ts.Sub(state.forkCopyAnchor) < time.Second {
			state.forkCopyAnchor = ts
			return Entry{}, false
		}
		state.suppressFork = false
	}
	var usage struct {
		Input         int64 `json:"input_tokens"`
		CachedInput   int64 `json:"cached_input_tokens"`
		CacheCreation int64 `json:"cache_write_input_tokens"`
		Output        int64 `json:"output_tokens"`
		Reasoning     int64 `json:"reasoning_output_tokens"`
	}
	if json.Unmarshal(payload.Info.Last, &usage) != nil {
		return Entry{}, false
	}
	cached := positive(usage.CachedInput)
	created := positive(usage.CacheCreation)
	uncached := positive(usage.Input - cached - created)
	output := positive(usage.Output)
	if uncached+cached+created+output == 0 {
		return Entry{}, false
	}
	return Entry{
		Agent: "codex", Model: state.model, SessionID: state.sessionID, CreatedAt: ts,
		Tokens: Tokens{UncachedInput: uncached, CachedInput: cached, CacheCreation: created, Output: output, Reasoning: min(positive(usage.Reasoning), output)},
	}, true
}
