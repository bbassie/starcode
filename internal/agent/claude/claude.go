// Package claude adapts the Claude Code CLI to the agent.Agent interface.
//
// A session is one long-lived `claude -p --input-format stream-json
// --output-format stream-json` process. Turns are written to its stdin, which
// stays open for the life of the session, and its JSONL stdout is translated
// into agent events.
//
// Line parsing lives in parseLine, a pure function over a parser state, so it
// can be exercised with recorded transcripts without starting a process.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"starcode/internal/agent"
)

// eventBuffer is the capacity of the channel returned by Events. A consumer
// that falls behind slows the reader down; nothing is dropped.
const eventBuffer = 256

// stderrTailBytes is how much of the child's stderr is kept for the Closed
// event.
const stderrTailBytes = 2048

// killGrace is how long the process gets to exit after stdin closes before it
// is signalled, and again between SIGTERM and SIGKILL.
const killGrace = 3 * time.Second

// summaryLimit caps ToolStarted.Summary.
const summaryLimit = 200

// Agent starts Claude Code sessions.
type Agent struct {
	binary string
	env    []string // extra KEY=VALUE pairs for every child
	log    *slog.Logger
}

type Option func(*Agent)

// WithBinary overrides the executable to run (default "claude", resolved on
// PATH).
func WithBinary(path string) Option {
	return func(a *Agent) {
		if path != "" {
			a.binary = path
		}
	}
}

// WithEnv adds KEY=VALUE pairs to every process this agent launches, after
// the inherited environment. CLAUDE_CONFIG_DIR here also moves where the
// adapter looks for the CLI's transcripts.
func WithEnv(kv []string) Option {
	return func(a *Agent) {
		a.env = append(a.env, kv...)
	}
}

// WithLogger sets the logger used for protocol diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(a *Agent) {
		if l != nil {
			a.log = l
		}
	}
}

func New(opts ...Option) *Agent {
	a := &Agent{
		binary: "claude",
		log:    slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

var _ agent.Agent = (*Agent)(nil)

func (a *Agent) Name() string { return "claude" }

// Capabilities asks the CLI what this account can use. The stream-json
// control channel answers an "initialize" request with the model list
// (values, display names, effort levels) without starting a conversation,
// so this costs a process launch but no tokens.
func (a *Agent) Capabilities(ctx context.Context) (agent.Capabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.binary, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	cmd.Env = childEnv(a.env)
	cmd.Stdin = strings.NewReader(`{"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}` + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.Capabilities{}, err
	}
	if err := cmd.Start(); err != nil {
		return agent.Capabilities{}, err
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	var caps agent.Capabilities
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string `json:"request_id"`
				Subtype   string `json:"subtype"`
				Error     string `json:"error"`
				Response  struct {
					Models []struct {
						Value         string   `json:"value"`
						ResolvedModel string   `json:"resolvedModel"`
						DisplayName   string   `json:"displayName"`
						Description   string   `json:"description"`
						Efforts       []string `json:"supportedEffortLevels"`
					} `json:"models"`
					CurrentPermissionMode string `json:"current_permission_mode"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != "init" {
			continue
		}
		if msg.Response.Subtype != "success" {
			return caps, fmt.Errorf("claude: initialize: %s", msg.Response.Error)
		}
		listed := map[string]bool{}
		for _, m := range msg.Response.Response.Models {
			listed[strings.TrimSuffix(m.ResolvedModel, "[1m]")] = true
			desc := m.Description
			if m.ResolvedModel != "" && !strings.Contains(desc, m.ResolvedModel) {
				desc = strings.TrimSpace(desc + " (" + m.ResolvedModel + ")")
			}
			caps.Models = append(caps.Models, agent.Model{
				ID: m.Value, DisplayName: versionedName(m.DisplayName, m.Description), Description: desc,
				Default: m.Value == "default", Efforts: m.Efforts,
				ContextWindow: contextWindowFor(m.Value, m.ResolvedModel, m.Description),
			})
		}
		for _, m := range olderModels {
			if !listed[m.ID] {
				m.Group = "Older versions"
				m.ContextWindow = contextWindowFor(m.ID, m.Description)
				caps.Models = append(caps.Models, m)
			}
		}
		break
	}
	if len(caps.Models) == 0 {
		if err := ctx.Err(); err != nil {
			return caps, fmt.Errorf("claude: initialize: %w", err)
		}
		return caps, errors.New("claude: initialize returned no models (is the CLI signed in?)")
	}
	caps.Efforts = []agent.Choice{
		{ID: "", Label: "default"}, {ID: "low", Label: "low"}, {ID: "medium", Label: "medium"},
		{ID: "high", Label: "high"}, {ID: "xhigh", Label: "xhigh"}, {ID: "max", Label: "max"},
	}
	caps.PermissionModes = []agent.Choice{
		{ID: "", Label: "default (ask for everything not allowed by settings)"},
		{ID: "acceptEdits", Label: "acceptEdits (file edits without asking)"},
		{ID: "plan", Label: "plan (read-only, no changes)"},
		{ID: "dontAsk", Label: "dontAsk (deny anything that would prompt)"},
		{ID: "bypassPermissions", Label: "bypassPermissions (never ask)"},
	}
	return caps, nil
}

// olderModels are still-served models the CLI leaves out of its initialize
// list, which only names the current generation by alias. Any of them (and
// anything else the API accepts) can also be typed into the picker; this
// list just saves looking the ids up. Entries whose id an alias already
// resolves to are skipped.
var olderModels = []agent.Model{
	{ID: "claude-fable-5", DisplayName: "Fable 5", Description: "Fable 5 · Previous Fable release (claude-fable-5)"},
	{ID: "claude-opus-4-8", DisplayName: "Opus 4.8", Description: "Opus 4.8 (claude-opus-4-8)"},
	{ID: "claude-opus-4-7", DisplayName: "Opus 4.7", Description: "Opus 4.7 (claude-opus-4-7)"},
	{ID: "claude-opus-4-6", DisplayName: "Opus 4.6", Description: "Opus 4.6 (claude-opus-4-6)"},
	{ID: "claude-sonnet-4-6", DisplayName: "Sonnet 4.6", Description: "Sonnet 4.6 (claude-sonnet-4-6)"},
}

// standardWindow and longWindow are the two context sizes Claude models
// come in today.
const (
	standardWindow = 200_000
	longWindow     = 1_000_000
)

// contextWindowFor is the token limit of a Claude model, guessed from any
// of the names the CLI gives it. The model list carries no number, so the
// long-context variants are recognized by the "[1m]" suffix on their id or
// by their description, and everything else gets the standard window. A
// turn's result states the real number for the model it ran on and
// replaces this.
func contextWindowFor(names ...string) int64 {
	for _, n := range names {
		s := strings.ToLower(n)
		if strings.Contains(s, "[1m]") || strings.Contains(s, "1m context") {
			return longWindow
		}
	}
	return standardWindow
}

// versionedName puts the version on an alias label. The CLI names aliases
// without one ("Fable", "Opus (1M context)") and keeps it in the
// description ("Fable 5.1 · Most capable…", "Opus 5 with 1M context · …"),
// so the label is taken from there when it starts with the same family
// name. Labels that do not ("Default (recommended)") are kept.
func versionedName(name, desc string) string {
	head, _, _ := strings.Cut(desc, "·")
	head = strings.TrimSpace(strings.Replace(head, " with 1M context", " (1M)", 1))
	family, _, _ := strings.Cut(name, " ")
	if family == "" || head == "" || !strings.HasPrefix(strings.ToLower(head), strings.ToLower(family)) {
		return name
	}
	return head
}

// Provider reports the installed CLI: path, version and sign-in state.
// `claude auth status --json` answers without touching the API, so an
// expired OAuth session shows up here before a turn fails on it.
func (a *Agent) Provider(ctx context.Context) (agent.ProviderInfo, error) {
	info := agent.ProviderInfo{Package: "@anthropic-ai/claude-code", UpdateCommand: "claude update"}
	path, err := exec.LookPath(a.binary)
	if err != nil {
		return info, err
	}
	info.Binary = path
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := cliOutput(ctx, a.binary, a.env, "--version")
	if err != nil {
		return info, fmt.Errorf("claude --version: %w", err)
	}
	// "2.1.261 (Claude Code)"
	if f := strings.Fields(out); len(f) > 0 {
		info.Version = f[0]
	}
	out, err = cliOutput(ctx, a.binary, a.env, "auth", "status", "--json")
	var st struct {
		LoggedIn         *bool  `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		Email            string `json:"email"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &st); jsonErr == nil && st.LoggedIn != nil {
		info.LoggedIn = st.LoggedIn
		var parts []string
		if st.Email != "" {
			parts = append(parts, st.Email)
		}
		if st.SubscriptionType != "" {
			parts = append(parts, st.SubscriptionType)
		}
		if st.AuthMethod != "" {
			parts = append(parts, "via "+st.AuthMethod)
		}
		info.Account = strings.Join(parts, " · ")
	} else if err != nil {
		// Older CLIs have no `auth status`; report the version and leave
		// the sign-in state unknown rather than failing the whole probe.
		info.Account = "sign-in state unknown: " + firstLineOf(err.Error())
	}
	return info, nil
}

// cliOutput runs the CLI for a one-shot answer and returns its trimmed
// stdout. A non-zero exit carries the stderr tail in the error.
func cliOutput(ctx context.Context, binary string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = childEnv(env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return strings.TrimSpace(string(out)), fmt.Errorf("%s", msg)
		}
		return strings.TrimSpace(string(out)), err
	}
	return strings.TrimSpace(string(out)), nil
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Start spawns the CLI. The session lives until Close is called or ctx is
// cancelled.
//
// The process produces no output until the first Send, so SessionInfo (and
// with it the id needed for ResumeID) arrives during the first turn, not at
// startup.
func (a *Agent) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-prompt-tool", "stdio",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	if cfg.ResumeID != "" {
		args = append(args, "--resume", cfg.ResumeID)
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, a.binary, args...)
	cmd.Dir = cfg.Cwd
	cmd.Env = childEnv(a.env)
	// Cancel is what Close and a cancelled ctx trigger: ask nicely, then let
	// os/exec send SIGKILL once WaitDelay expires.
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = killGrace

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude: stdout pipe: %w", err)
	}
	errTail := &tailBuffer{max: stderrTailBytes}
	cmd.Stderr = errTail

	s := &session{
		log:    a.log.With("agent", "claude"),
		cmd:    cmd,
		config: claudeConfigDir(a.env),
		cancel: cancel,
		stdin:  stdin,
		stdout: stdout,
		stderr: errTail,
		events: make(chan agent.Event, eventBuffer),
		dying:  make(chan struct{}),
		exited: make(chan struct{}),
		st:     newState(a.log),

		bypassOK: cfg.PermissionMode == "bypassPermissions",
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("claude: start %s: %w", a.binary, err)
	}
	s.log.Debug("session started", "pid", cmd.Process.Pid, "cwd", cfg.Cwd, "model", cfg.Model)
	go s.run()
	return s, nil
}

// childEnv is os.Environ minus the variables Claude Code sets for its own
// children, plus extra. Leaving the former in place makes a nested launch
// think it is running inside another Claude Code and refuse to start.
func childEnv(extra []string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+len(extra))
	for _, kv := range src {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// claudeConfigDir is where this agent's CLI keeps its state: an extra
// CLAUDE_CONFIG_DIR wins over the inherited one, then ~/.claude.
func claudeConfigDir(extra []string) string {
	for i := len(extra) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(extra[i], "CLAUDE_CONFIG_DIR="); ok && strings.TrimSpace(v) != "" {
			return filepath.Clean(strings.TrimSpace(v))
		}
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Clean(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// persistedTitle reads Claude Code's locally generated session name. The
// public stream-json schema does not expose this record, but the CLI persists
// it as {"type":"ai-title","aiTitle":"...","sessionId":"..."} in the
// project transcript used by its own /resume picker. This is deliberately
// best-effort: a missing or changed internal format leaves Starcode's prompt
// title in place.
func persistedTitle(configDir, sessionID string) string {
	return (&titleReader{}).read(configDir, sessionID)
}

// titleReader remembers how far into the transcript it has looked, so a
// session that completes many turns reads each byte once instead of the
// whole file (tens of megabytes on a long thread) after every turn. The
// CLI only appends, and only whole lines count: the offset stops at the
// last newline so a line still being written is read next time.
type titleReader struct {
	sessionID string
	path      string
	offset    int64
	title     string
}

func (r *titleReader) read(configDir, sessionID string) string {
	if configDir == "" || sessionID == "" || filepath.Base(sessionID) != sessionID || strings.ContainsAny(sessionID, "*?[") {
		return ""
	}
	if sessionID != r.sessionID {
		*r = titleReader{sessionID: sessionID}
	}
	if r.path == "" {
		paths, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
		if err != nil || len(paths) == 0 {
			return ""
		}
		r.path = paths[0]
	}
	f, err := os.Open(r.path)
	if err != nil {
		r.path = ""
		return r.title
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() < r.offset {
		// Truncated or replaced: start over.
		r.offset = 0
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return r.title
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return r.title
	}
	lines := bytes.Split(data, []byte{'\n'})
	consumed := 0
	for i, raw := range lines {
		if i == len(lines)-1 {
			// No newline after this one yet. A record that already parses
			// is complete (the CLI writes whole objects); anything else is
			// still being written and is read next time.
			if len(bytes.TrimSpace(raw)) == 0 || !json.Valid(raw) {
				break
			}
			consumed += len(raw)
		} else {
			consumed += len(raw) + 1
		}
		if !bytes.Contains(raw, []byte(`"ai-title"`)) {
			continue
		}
		var line struct {
			Type      string `json:"type"`
			AITitle   string `json:"aiTitle"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &line) == nil && line.Type == "ai-title" && line.SessionID == sessionID {
			if name := strings.TrimSpace(line.AITitle); name != "" {
				r.title = name
			}
		}
	}
	r.offset += int64(consumed)
	return r.title
}

// session is one live CLI process.
type session struct {
	log    *slog.Logger
	cmd    *exec.Cmd
	config string
	cancel context.CancelFunc
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer

	// bypassOK records that the CLI was launched in bypassPermissions. Only
	// then does it accept a later switch back to that mode; other sessions
	// silently keep their mode.
	bypassOK bool

	// wmu serializes stdin writes so two JSON lines never interleave.
	wmu sync.Mutex

	// mu guards st, which both the reader and the caller mutate.
	mu sync.Mutex
	st *state
	// title is the last native ai-title surfaced to the app. Claude's public
	// stream does not currently include it, so readLoop supplements the
	// stream from the CLI's session transcript after a turn completes.
	title  string
	titles titleReader

	// emu orders sends on events against closing it.
	emu      sync.Mutex
	evClosed bool
	events   chan agent.Event

	closeOnce sync.Once
	userClose atomic.Bool
	dying     chan struct{}
	exited    chan struct{}
}

var errSessionClosed = errors.New("claude: session closed")

var _ agent.Session = (*session)(nil)
var _ agent.ModeSetter = (*session)(nil)

func (s *session) Events() <-chan agent.Event { return s.events }

// Send starts a turn. It emits TurnStarted before the text reaches the CLI so
// the turn id is known to the consumer first.
func (s *session) Send(ctx context.Context, text string) error {
	if err := s.alive(); err != nil {
		return err
	}
	turnID := newID()
	s.mu.Lock()
	s.st.turnID = turnID
	s.st.interrupted = false
	s.mu.Unlock()

	if err := s.emit(ctx, agent.Event{Kind: agent.KindTurnStarted, TurnStarted: &agent.TurnStarted{TurnID: turnID}}); err != nil {
		return err
	}
	return s.write(userTurn(text))
}

// Interrupt cancels the running turn. The CLI still reports a result for it;
// that result is reported as "interrupted".
func (s *session) Interrupt(ctx context.Context) error {
	if err := s.alive(); err != nil {
		return err
	}
	s.mu.Lock()
	s.st.interrupted = true
	s.mu.Unlock()
	return s.write(interruptRequest(newUUID()))
}

// SetPermissionMode switches the running session to mode. The CLI applies
// it to the next tool call; an approval it already asked for stays pending.
func (s *session) SetPermissionMode(ctx context.Context, mode string) error {
	if err := s.alive(); err != nil {
		return err
	}
	if mode == "bypassPermissions" && !s.bypassOK {
		return agent.ErrModeNextTurn
	}
	if mode == "" {
		mode = "default"
	}
	return s.write(setModeRequest(newUUID(), mode))
}

// Resolve answers a pending ApprovalRequested.
func (s *session) Resolve(ctx context.Context, approvalID string, decision agent.Decision) error {
	if err := s.alive(); err != nil {
		return err
	}
	s.mu.Lock()
	pending, ok := s.st.approvals[approvalID]
	delete(s.st.approvals, approvalID)
	if ok && decision == agent.Deny {
		s.st.denied[pending.toolID] = true
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("claude: unknown approval %q", approvalID)
	}
	return s.write(approvalResponse(approvalID, decision, pending.input))
}

// Close terminates the process and waits for it. It is safe to call twice.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		s.userClose.Store(true)
		close(s.dying)
		s.wmu.Lock()
		_ = s.stdin.Close()
		s.wmu.Unlock()
		// Closing stdin is usually enough; signal only if it is not.
		go func() {
			select {
			case <-s.exited:
			case <-time.After(killGrace):
				s.cancel()
			}
		}()
	})
	<-s.exited
	s.cancel()
	return nil
}

func (s *session) alive() error {
	select {
	case <-s.exited:
		return errSessionClosed
	case <-s.dying:
		return errSessionClosed
	default:
		return nil
	}
}

func (s *session) write(line []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.stdin.Write(line); err != nil {
		return fmt.Errorf("claude: write stdin: %w", err)
	}
	return nil
}

// emit delivers one event, blocking while the consumer catches up. Once the
// session is closing, a consumer that has stopped reading no longer holds
// shutdown up: further events are dropped instead.
func (s *session) emit(ctx context.Context, ev agent.Event) error {
	s.emu.Lock()
	defer s.emu.Unlock()
	if s.evClosed {
		return errSessionClosed
	}
	select {
	case s.events <- ev:
		return nil
	default:
	}
	select {
	case s.events <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.dying:
		s.log.Warn("dropped event, consumer stopped reading", "kind", ev.Kind)
		return errSessionClosed
	}
}

func (s *session) closeEvents() {
	s.emu.Lock()
	defer s.emu.Unlock()
	s.evClosed = true
	close(s.events)
}

// run reads stdout to EOF, reaps the process, and emits the final Closed
// event.
func (s *session) run() {
	s.readLoop()
	waitErr := s.cmd.Wait()
	close(s.exited)

	var closeErr error
	if !s.userClose.Load() && waitErr != nil {
		if tail := s.stderr.String(); tail != "" {
			closeErr = fmt.Errorf("claude: %w: %s", waitErr, tail)
		} else {
			closeErr = fmt.Errorf("claude: %w", waitErr)
		}
	}
	s.log.Debug("session ended", "err", closeErr)
	_ = s.emit(context.Background(), agent.Event{Kind: agent.KindClosed, Closed: &agent.Closed{Err: closeErr}})
	s.closeEvents()
}

func (s *session) readLoop() {
	sc := bufio.NewScanner(s.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		s.mu.Lock()
		events := parseLine(line, s.st)
		outbox := s.st.takeOutbox()
		s.mu.Unlock()

		// Control responses first: the CLI blocks on them.
		for _, msg := range outbox {
			if err := s.write(msg); err != nil {
				s.log.Warn("control response failed", "err", err)
			}
		}
		for _, ev := range events {
			if ev.Kind == agent.KindThreadTitle && ev.ThreadTitle != nil {
				s.title = strings.TrimSpace(ev.ThreadTitle.Title)
			}
			if ev.Kind == agent.KindTurnCompleted {
				s.mu.Lock()
				sessionID := s.st.sessionID
				s.mu.Unlock()
				if err := s.emitPersistedTitle(sessionID); err != nil {
					return
				}
			}
			if err := s.emit(context.Background(), ev); err != nil {
				return
			}
			// Resumed sessions already have a title on disk at init time. New
			// sessions get another lookup just before their first completion.
			if ev.Kind == agent.KindSessionInfo && ev.SessionInfo != nil {
				if err := s.emitPersistedTitle(ev.SessionInfo.ExternalID); err != nil {
					return
				}
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		s.log.Warn("stdout read failed", "err", err)
	}
}

func (s *session) emitPersistedTitle(sessionID string) error {
	title := s.titles.read(s.config, sessionID)
	if title == "" || title == s.title {
		return nil
	}
	s.title = title
	return s.emit(context.Background(), agent.Event{Kind: agent.KindThreadTitle, ThreadTitle: &agent.ThreadTitle{Title: title}})
}

// stderrTail returns the captured tail of the child's stderr. Used by tests.
func (s *session) stderrTail() string { return s.stderr.String() }

// Outgoing messages.

func userTurn(text string) []byte {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	return encodeLine(msg)
}

func interruptRequest(id string) []byte {
	return encodeLine(map[string]any{
		"type":       "control_request",
		"request_id": id,
		"request":    map[string]any{"subtype": "interrupt"},
	})
}

func setModeRequest(id, mode string) []byte {
	return encodeLine(map[string]any{
		"type":       "control_request",
		"request_id": id,
		"request":    map[string]any{"subtype": "set_permission_mode", "mode": mode},
	})
}

func approvalResponse(requestID string, decision agent.Decision, input json.RawMessage) []byte {
	inner := map[string]any{"behavior": "deny", "message": "User denied this action"}
	if decision == agent.Allow {
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		inner = map[string]any{"behavior": "allow", "updatedInput": input}
	}
	return encodeLine(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   inner,
		},
	})
}

func controlError(requestID, reason string) []byte {
	return encodeLine(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "error",
			"request_id": requestID,
			"error":      reason,
		},
	})
}

func encodeLine(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Every value here is built from strings and json.RawMessage that the
		// CLI already gave us, so this cannot fail in practice.
		panic("claude: encode outgoing message: " + err.Error())
	}
	return append(b, '\n')
}

// Parser.

// state is everything parseLine remembers between lines. It is not safe for
// concurrent use; session guards it.
type state struct {
	log *slog.Logger
	// sessionID is learned from system/init and identifies Claude's local
	// persisted transcript.
	sessionID string

	// model is the model the conversation runs on, so the result's
	// per-model usage can be read for the right one. window is its context
	// limit; ctxTokens and ctxWindow are the last reading passed on, so the
	// same numbers are not reported twice.
	model                        string
	window, ctxTokens, ctxWindow int64

	// turnID is the id Send generated for the turn in flight.
	turnID string
	// interrupted records that we asked the CLI to stop this turn.
	interrupted bool

	// msgID is the assistant message the stream events belong to.
	msgID string
	// blocks tracks open content blocks by index.
	blocks map[int]*blockState
	// streamedText/streamedThinking record which messages arrived as stream
	// events, so the complete message is not emitted a second time.
	streamedText     map[string]bool
	streamedThinking map[string]bool
	// toolInputs records tool calls whose full input was already emitted.
	toolInputs map[string]bool

	// approvals maps a control request id to the tool it concerns.
	approvals map[string]pendingApproval
	// denied records tools the user refused, so their result reads as
	// declined rather than merely failed.
	denied map[string]bool

	// outbox holds lines the session must write back to the CLI.
	outbox [][]byte
}

type blockState struct {
	kind     string
	toolID   string
	toolName string
	input    []byte
}

type pendingApproval struct {
	toolID string
	input  json.RawMessage
}

func newState(log *slog.Logger) *state {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &state{
		log:              log,
		blocks:           map[int]*blockState{},
		streamedText:     map[string]bool{},
		streamedThinking: map[string]bool{},
		toolInputs:       map[string]bool{},
		approvals:        map[string]pendingApproval{},
		denied:           map[string]bool{},
	}
}

func (st *state) takeOutbox() [][]byte {
	out := st.outbox
	st.outbox = nil
	return out
}

// contextEvent reports how full the window is, or nothing when neither the
// count nor the limit moved since the last reading.
func (st *state) contextEvent(tokens int64) []agent.Event {
	if tokens <= 0 || (tokens == st.ctxTokens && st.window == st.ctxWindow) {
		return nil
	}
	st.ctxTokens, st.ctxWindow = tokens, st.window
	return []agent.Event{{
		Kind:         agent.KindContextUsage,
		ContextUsage: &agent.ContextUsage{Tokens: tokens, Window: st.window},
	}}
}

// endTurn drops the per-turn bookkeeping. Message and tool ids are unique per
// turn, so nothing here outlives a result. The model and its context reading
// describe the conversation instead, and stay.
func (st *state) endTurn() {
	st.turnID = ""
	st.interrupted = false
	st.msgID = ""
	clear(st.blocks)
	clear(st.streamedText)
	clear(st.streamedThinking)
	clear(st.toolInputs)
	clear(st.denied)
	// A can_use_tool the turn never answered is dead with the turn; keeping
	// it would let a later Resolve write a response nobody waits for.
	clear(st.approvals)
}

// Wire shapes. Only the fields the adapter uses are declared.

type outLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	AITitle   string `json:"aiTitle"`

	// stream_event
	Event json.RawMessage `json:"event"`

	// assistant, user
	Message json.RawMessage `json:"message"`

	// control_request
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`

	// control_response
	Response json.RawMessage `json:"response"`

	// rate_limit_event
	Status        string          `json:"status"`
	RateLimitInfo json.RawMessage `json:"rate_limit_info"`

	// result
	DurationMS int64   `json:"duration_ms"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	CostUSD    float64 `json:"total_cost_usd"`
	Usage      *usage  `json:"usage"`
	// ModelUsage breaks the turn down per model, and is the one place the
	// CLI states the context window it ran with.
	ModelUsage map[string]struct {
		ContextWindow int64 `json:"contextWindow"`
	} `json:"modelUsage"`
}

// contextWindow reads the window of the model the conversation runs on.
// Subagents add entries of their own, so the main model is looked up by
// name and a map with nothing else in it is taken as that model's.
func (out *outLine) contextWindow(model string) int64 {
	if u, ok := out.ModelUsage[model]; ok {
		return u.ContextWindow
	}
	if len(out.ModelUsage) == 1 {
		for _, u := range out.ModelUsage {
			return u.ContextWindow
		}
	}
	return 0
}

type usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// context is what this request put in the window: the fresh input, the
// cached prefix it was replayed on top of, and the reply that joins them
// for the next one. On a result line the same fields are the turn's total
// instead, so only a message's usage may be read this way.
func (u *usage) context() int64 {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}

type streamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message *struct {
		ID string `json:"id"`
	} `json:"message"`

	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

type apiMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Usage   *usage          `json:"usage"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text"`
	// thinking
	Thinking string `json:"thinking"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type controlRequest struct {
	Subtype     string          `json:"subtype"`
	ToolName    string          `json:"tool_name"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	ToolUseID   string          `json:"tool_use_id"`
}

// parseLine turns one JSONL line from the CLI into agent events. Lines that
// need an answer put it in st.outbox.
func parseLine(line []byte, st *state) []agent.Event {
	var out outLine
	if err := json.Unmarshal(line, &out); err != nil {
		st.log.Debug("unparsable line", "err", err, "line", clip(string(line), 200))
		return nil
	}

	switch out.Type {
	case "system":
		return parseSystem(&out, st)
	case "stream_event":
		return parseStreamEvent(&out, st)
	case "assistant":
		return parseAssistant(&out, st)
	case "user":
		return parseUser(&out, st)
	case "control_request":
		return parseControlRequest(&out, st)
	case "rate_limit_event":
		return parseRateLimit(&out, st)
	case "result":
		return parseResult(&out, st)
	case "ai-title":
		if title := strings.TrimSpace(out.AITitle); title != "" {
			return []agent.Event{{Kind: agent.KindThreadTitle, ThreadTitle: &agent.ThreadTitle{Title: title}}}
		}
		return nil
	case "control_response":
		// Answers to our own requests (interrupt, set_permission_mode). A
		// refusal is worth seeing in the log; success is noise.
		var resp struct {
			Subtype string `json:"subtype"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(out.Response, &resp) == nil && resp.Subtype == "error" {
			st.log.Warn("control request refused", "err", resp.Error, "line", clip(string(line), 200))
		} else {
			st.log.Debug("control response", "line", clip(string(line), 200))
		}
		return nil
	default:
		st.log.Debug("ignored line", "type", out.Type, "subtype", out.Subtype)
		return nil
	}
}

func parseSystem(out *outLine, st *state) []agent.Event {
	switch out.Subtype {
	case "init":
		st.sessionID = out.SessionID
		if out.Model != "" {
			st.model = out.Model
			st.window = contextWindowFor(out.Model)
		}
		return []agent.Event{{
			Kind:        agent.KindSessionInfo,
			SessionInfo: &agent.SessionInfo{ExternalID: out.SessionID, Model: out.Model},
		}}
	case "compact_boundary":
		return []agent.Event{{
			Kind:   agent.KindNotice,
			Notice: &agent.Notice{Text: "Context was compacted."},
		}}
	default:
		st.log.Debug("ignored system message", "subtype", out.Subtype)
		return nil
	}
}

func parseStreamEvent(out *outLine, st *state) []agent.Event {
	var ev streamEvent
	if err := json.Unmarshal(out.Event, &ev); err != nil {
		st.log.Debug("unparsable stream event", "err", err)
		return nil
	}

	switch ev.Type {
	case "message_start":
		st.msgID = ""
		if ev.Message != nil {
			st.msgID = ev.Message.ID
		}
		if st.msgID == "" {
			st.msgID = newID()
		}
		clear(st.blocks)
		return nil

	case "content_block_start":
		if ev.ContentBlock == nil {
			return nil
		}
		blk := &blockState{kind: ev.ContentBlock.Type, toolID: ev.ContentBlock.ID, toolName: ev.ContentBlock.Name}
		st.blocks[ev.Index] = blk
		if blk.kind == "tool_use" && blk.toolID != "" {
			return []agent.Event{{
				Kind:        agent.KindToolStarted,
				ToolStarted: &agent.ToolStarted{ID: blk.toolID, Name: blk.toolName},
			}}
		}
		return nil

	case "content_block_delta":
		if ev.Delta == nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text == "" {
				return nil
			}
			st.streamedText[st.msgID] = true
			return []agent.Event{{
				Kind:      agent.KindTextDelta,
				TextDelta: &agent.TextDelta{MessageID: st.msgID, Text: ev.Delta.Text},
			}}
		case "thinking_delta":
			if ev.Delta.Thinking == "" {
				return nil
			}
			st.streamedThinking[st.msgID] = true
			return []agent.Event{{
				Kind:          agent.KindThinkingDelta,
				ThinkingDelta: &agent.ThinkingDelta{MessageID: st.msgID, Text: ev.Delta.Thinking},
			}}
		case "input_json_delta":
			if blk := st.blocks[ev.Index]; blk != nil {
				blk.input = append(blk.input, ev.Delta.PartialJSON...)
			}
		}
		return nil

	case "content_block_stop":
		blk := st.blocks[ev.Index]
		delete(st.blocks, ev.Index)
		if blk == nil || blk.kind != "tool_use" || blk.toolID == "" {
			return nil
		}
		if st.toolInputs[blk.toolID] {
			// The complete assistant message already carried the input.
			return nil
		}
		input := json.RawMessage(blk.input)
		if !json.Valid(input) {
			st.log.Debug("incomplete tool input", "tool", blk.toolName, "id", blk.toolID)
			input = nil
		}
		st.toolInputs[blk.toolID] = true
		return []agent.Event{{
			Kind: agent.KindToolStarted,
			ToolStarted: &agent.ToolStarted{
				ID:      blk.toolID,
				Name:    blk.toolName,
				Input:   input,
				Summary: summarize(blk.toolName, input),
			},
		}}

	default:
		// message_delta, message_stop and anything new.
		return nil
	}
}

func parseAssistant(out *outLine, st *state) []agent.Event {
	msg, blocks, ok := decodeMessage(out.Message, st)
	if !ok {
		return nil
	}
	// Every assistant message states what its request cost, which is the
	// only place the size of the conversation shows up while a turn runs.
	if msg.Model != "" {
		st.model = msg.Model
	}
	events := st.contextEvent(msg.Usage.context())
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			// Only a fallback: with --include-partial-messages the text
			// already arrived as deltas.
			if blk.Text == "" || st.streamedText[msg.ID] {
				continue
			}
			events = append(events, agent.Event{
				Kind:      agent.KindTextDelta,
				TextDelta: &agent.TextDelta{MessageID: msg.ID, Text: blk.Text},
			})
		case "thinking":
			if blk.Thinking == "" || st.streamedThinking[msg.ID] {
				continue
			}
			events = append(events, agent.Event{
				Kind:          agent.KindThinkingDelta,
				ThinkingDelta: &agent.ThinkingDelta{MessageID: msg.ID, Text: blk.Thinking},
			})
		case "tool_use":
			if blk.ID == "" || st.toolInputs[blk.ID] {
				continue
			}
			st.toolInputs[blk.ID] = true
			events = append(events, agent.Event{
				Kind: agent.KindToolStarted,
				ToolStarted: &agent.ToolStarted{
					ID:      blk.ID,
					Name:    blk.Name,
					Input:   blk.Input,
					Summary: summarize(blk.Name, blk.Input),
				},
			})
		}
	}
	return events
}

func parseUser(out *outLine, st *state) []agent.Event {
	_, blocks, ok := decodeMessage(out.Message, st)
	if !ok {
		return nil
	}
	var events []agent.Event
	for _, blk := range blocks {
		if blk.Type != "tool_result" || blk.ToolUseID == "" {
			continue
		}
		status := "done"
		switch {
		case st.denied[blk.ToolUseID]:
			status = "declined"
		case blk.IsError:
			status = "failed"
		}
		events = append(events, agent.Event{
			Kind: agent.KindToolCompleted,
			ToolCompleted: &agent.ToolCompleted{
				ID:      blk.ToolUseID,
				Status:  status,
				Output:  flattenContent(blk.Content),
				IsError: blk.IsError,
			},
		})
	}
	return events
}

func parseControlRequest(out *outLine, st *state) []agent.Event {
	var req controlRequest
	if err := json.Unmarshal(out.Request, &req); err != nil {
		st.log.Debug("unparsable control request", "err", err)
		st.outbox = append(st.outbox, controlError(out.RequestID, "unsupported"))
		return nil
	}
	if req.Subtype != "can_use_tool" {
		// Anything we cannot answer must still be answered or the CLI waits
		// forever.
		st.log.Debug("declining control request", "subtype", req.Subtype)
		st.outbox = append(st.outbox, controlError(out.RequestID, "unsupported"))
		return nil
	}

	name := req.ToolName
	if name == "" {
		name = req.DisplayName
	}
	st.approvals[out.RequestID] = pendingApproval{toolID: req.ToolUseID, input: req.Input}
	return []agent.Event{{
		Kind: agent.KindApproval,
		Approval: &agent.ApprovalRequested{
			ID:          out.RequestID,
			ToolID:      req.ToolUseID,
			ToolName:    name,
			Description: req.Description,
			Input:       req.Input,
		},
	}}
}

func parseRateLimit(out *outLine, st *state) []agent.Event {
	status := out.Status
	if len(out.RateLimitInfo) > 0 {
		var info struct {
			Status        string `json:"status"`
			RateLimitType string `json:"rateLimitType"`
		}
		if err := json.Unmarshal(out.RateLimitInfo, &info); err == nil && info.Status != "" {
			status = info.Status
		}
	}
	if status == "" || status == "allowed" {
		return nil
	}
	return []agent.Event{{
		Kind:   agent.KindNotice,
		Notice: &agent.Notice{Text: "Rate limit " + status + "."},
	}}
}

func parseResult(out *outLine, st *state) []agent.Event {
	done := &agent.TurnCompleted{
		TurnID:     st.turnID,
		Status:     "done",
		DurationMS: out.DurationMS,
		CostUSD:    out.CostUSD,
	}
	if out.Usage != nil {
		done.InputTokens = out.Usage.InputTokens + out.Usage.CacheReadTokens + out.Usage.CacheCreationTokens
		done.OutputTokens = out.Usage.OutputTokens
	}
	if out.IsError || (out.Subtype != "" && out.Subtype != "success") {
		done.Status = "error"
		done.Error = out.Result
		if done.Error == "" {
			done.Error = out.Subtype
		}
	}
	if st.interrupted {
		done.Status = "interrupted"
		done.Error = ""
	}
	// The result is where the CLI names the real window; the count stays
	// the one the last message reported, since the usage here is the sum
	// over the whole turn rather than what sits in the window.
	var events []agent.Event
	if w := out.contextWindow(st.model); w > 0 && w != st.window {
		st.window = w
		events = st.contextEvent(st.ctxTokens)
	}
	st.endTurn()
	return append(events, agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: done})
}

// decodeMessage unpacks message.content, which is either a string or a list
// of content blocks.
func decodeMessage(raw json.RawMessage, st *state) (apiMessage, []contentBlock, bool) {
	var msg apiMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		st.log.Debug("unparsable message", "err", err)
		return msg, nil, false
	}
	if len(msg.Content) == 0 {
		return msg, nil, false
	}
	var blocks []contentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		// A plain string turn (our own echo, or an interrupt marker).
		return msg, nil, false
	}
	return msg, blocks, true
}

// flattenContent renders tool_result content, which is either a string or a
// list of blocks, as plain text.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// summarize picks the one field that best describes a tool call.
func summarize(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(input, &fields); err != nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := fields[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}

	var text string
	switch name {
	case "Bash":
		text = pick("command", "description")
	case "Read", "Write", "Edit", "NotebookEdit":
		text = pick("file_path")
	case "Glob", "Grep":
		text = pick("pattern")
	case "WebFetch":
		text = pick("url")
	case "Task", "Agent":
		text = pick("description")
	}
	if text == "" {
		// Sorted so the choice does not change between runs.
		for _, k := range slices.Sorted(maps.Keys(fields)) {
			if v, ok := fields[k].(string); ok && v != "" {
				text = v
				break
			}
		}
	}
	return clip(strings.Join(strings.Fields(text), " "), summaryLimit)
}

func clip(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit-3]) + "..."
}

// Identifiers.

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
