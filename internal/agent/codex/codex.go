// Package codex adapts `codex app-server` to the starcode agent interface.
//
// One app-server process is shared by every session: it is started lazily on
// the first Start, its stdio carries newline-delimited JSON-RPC (with the
// "jsonrpc" field omitted, as the protocol specifies), and notifications are
// routed to sessions by the threadId they carry. If the process dies, every
// session bound to it is closed with the error and the next Start launches a
// new one.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"starcode/internal/agent"
)

const (
	clientName    = "starcode"
	clientVersion = "0.1.0"

	// The v2 protocol serializes SandboxMode and AskForApproval in
	// kebab-case; see permissionModes for the combinations offered.
)

var (
	_ agent.Agent   = (*Agent)(nil)
	_ agent.Session = (*session)(nil)
)

// Agent creates codex sessions. The zero value is not usable; call New.
type Agent struct {
	binary string
	args   []string
	env    []string // extra KEY=VALUE pairs for the app-server and probes
	log    *slog.Logger
	dial   func() (transport, error)

	mu     sync.Mutex
	client *client
}

type Option func(*Agent)

// WithBinary overrides the `codex` executable to run.
func WithBinary(path string) Option {
	return func(a *Agent) { a.binary = path }
}

// WithArgs appends extra arguments after "app-server".
func WithArgs(extra ...string) Option {
	return func(a *Agent) { a.args = append(a.args, extra...) }
}

// WithEnv adds KEY=VALUE pairs (CODEX_HOME, say) to the app-server's
// environment and to the version and login probes.
func WithEnv(kv []string) Option {
	return func(a *Agent) { a.env = append(a.env, kv...) }
}

// WithLogger sets the logger; the default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(a *Agent) {
		if l != nil {
			a.log = l
		}
	}
}

// withTransport replaces the process launcher. Tests use it to drive the
// adapter over in-memory pipes.
func withTransport(dial func() (transport, error)) Option {
	return func(a *Agent) { a.dial = dial }
}

func New(opts ...Option) *Agent {
	a := &Agent{binary: "codex", log: slog.Default()}
	for _, opt := range opts {
		opt(a)
	}
	if a.dial == nil {
		a.dial = func() (transport, error) {
			args := append([]string{"app-server"}, a.args...)
			return startProcess(a.binary, args, a.env, a.log)
		}
	}
	return a
}

func (a *Agent) Name() string { return "codex" }

// Start opens a thread on the shared app-server, resuming cfg.ResumeID when
// it is set.
func (a *Agent) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	c, err := a.ensureClient(ctx)
	if err != nil {
		return nil, err
	}

	mode := permissionMode(cfg.PermissionMode)
	params := map[string]any{
		"cwd":            cfg.Cwd,
		"approvalPolicy": mode.approval,
		"sandbox":        mode.sandbox,
	}
	turnParams := map[string]any{"approvalPolicy": mode.approval}
	if cfg.Model != "" {
		params["model"] = cfg.Model
		turnParams["model"] = cfg.Model
	}
	if cfg.Effort != "" {
		params["config"] = map[string]any{"model_reasoning_effort": cfg.Effort}
		turnParams["effort"] = cfg.Effort
	}
	method := "thread/start"
	if cfg.ResumeID != "" {
		method = "thread/resume"
		params["threadId"] = cfg.ResumeID
		// History already lives in starcode's store, so skip the replay.
		params["excludeTurns"] = true
	}

	raw, err := c.call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	var res struct {
		Thread struct {
			ID    string  `json:"id"`
			Model string  `json:"model"`
			Name  *string `json:"name"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("codex: %s: bad result: %w", method, err)
	}
	if res.Thread.ID == "" {
		return nil, errors.New("codex: " + method + ": result has no thread id")
	}

	model := res.Thread.Model
	if model == "" {
		model = cfg.Model
	}
	s := newSession(c, res.Thread.ID, a.log)
	s.turnParams = turnParams
	c.addSession(s)
	s.emit(agent.Event{
		Kind:        agent.KindSessionInfo,
		SessionInfo: &agent.SessionInfo{ExternalID: res.Thread.ID, Model: model},
	})
	if res.Thread.Name != nil {
		if title := strings.TrimSpace(*res.Thread.Name); title != "" {
			s.emit(agent.Event{Kind: agent.KindThreadTitle, ThreadTitle: &agent.ThreadTitle{Title: title}})
		}
	}
	return s, nil
}

// Provider reports the installed CLI: path, version and sign-in state,
// from `codex --version` and `codex login status`.
func (a *Agent) Provider(ctx context.Context) (agent.ProviderInfo, error) {
	info := agent.ProviderInfo{Package: "@openai/codex", UpdateCommand: "codex update"}
	path, err := exec.LookPath(a.binary)
	if err != nil {
		return info, err
	}
	info.Binary = path
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	version := exec.CommandContext(ctx, a.binary, "--version")
	version.Env = childEnv(a.env)
	out, err := version.Output()
	if err != nil {
		return info, fmt.Errorf("codex --version: %w", err)
	}
	// "codex-cli 0.153.3"
	if f := strings.Fields(string(out)); len(f) > 0 {
		info.Version = f[len(f)-1]
	}
	status := exec.CommandContext(ctx, a.binary, "login", "status")
	status.Env = childEnv(a.env)
	var combined bytes.Buffer
	status.Stdout, status.Stderr = &combined, &combined
	statusErr := status.Run()
	line := strings.TrimSpace(combined.String())
	if first, _, ok := strings.Cut(line, "\n"); ok {
		line = first
	}
	loggedIn := statusErr == nil && !strings.Contains(strings.ToLower(line), "not logged in")
	info.LoggedIn = &loggedIn
	info.Account = line
	return info, nil
}

// Shutdown stops the shared app-server. Sessions still open are closed with an
// error. A later Start launches a fresh process.
func (a *Agent) Shutdown() error {
	a.mu.Lock()
	c := a.client
	a.client = nil
	a.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.shutdown()
}

func (a *Agent) ensureClient(ctx context.Context) (*client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil && !a.client.isDead() {
		return a.client, nil
	}
	tr, err := a.dial()
	if err != nil {
		return nil, fmt.Errorf("codex: start app-server: %w", err)
	}
	c := newClient(tr, a.log)
	c.run()
	if err := c.handshake(ctx); err != nil {
		_ = c.shutdown()
		return nil, fmt.Errorf("codex: handshake: %w", err)
	}
	a.client = c
	return c, nil
}

// permissionModes are starcode-level ids mapping onto codex's two knobs.
type codexMode struct {
	id, label, approval, sandbox string
}

var permissionModes = []codexMode{
	{"", "default (workspace writes allowed, ask to leave the sandbox)", "on-request", "workspace-write"},
	{"untrusted", "untrusted (ask before every command)", "untrusted", "workspace-write"},
	{"read-only", "read-only (no writes, ask to leave the sandbox)", "on-request", "read-only"},
	{"full-access", "full-access (no sandbox, never ask)", "never", "danger-full-access"},
}

func permissionMode(id string) codexMode {
	for _, m := range permissionModes {
		if m.id == id {
			return m
		}
	}
	return permissionModes[0]
}

// Capabilities queries the app-server's model catalog for the signed-in
// account and lists the permission modes above.
func (a *Agent) Capabilities(ctx context.Context) (agent.Capabilities, error) {
	var caps agent.Capabilities
	c, err := a.ensureClient(ctx)
	if err != nil {
		return caps, err
	}
	raw, err := c.call(ctx, "model/list", map[string]any{})
	if err != nil {
		return caps, err
	}
	var res struct {
		Data []struct {
			ID          string `json:"id"`
			Model       string `json:"model"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Hidden      bool   `json:"hidden"`
			IsDefault   bool   `json:"isDefault"`
			Efforts     []struct {
				ReasoningEffort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
			DefaultEffort string `json:"defaultReasoningEffort"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return caps, fmt.Errorf("codex: model/list: bad result: %w", err)
	}
	seen := map[string]bool{}
	for _, m := range res.Data {
		if m.Hidden {
			continue
		}
		id := m.Model
		if id == "" {
			id = m.ID
		}
		var efforts []string
		for _, e := range m.Efforts {
			efforts = append(efforts, e.ReasoningEffort)
			if !seen[e.ReasoningEffort] {
				seen[e.ReasoningEffort] = true
				caps.Efforts = append(caps.Efforts, agent.Choice{ID: e.ReasoningEffort, Label: e.ReasoningEffort})
			}
		}
		desc := m.Description
		if m.DefaultEffort != "" {
			desc = strings.TrimSpace(desc + " · default effort " + m.DefaultEffort)
		}
		caps.Models = append(caps.Models, agent.Model{ID: id, DisplayName: m.DisplayName, Description: desc, Default: m.IsDefault, Efforts: efforts})
	}
	caps.Efforts = append([]agent.Choice{{ID: "", Label: "default"}}, caps.Efforts...)
	for _, m := range permissionModes {
		caps.PermissionModes = append(caps.PermissionModes, agent.Choice{ID: m.id, Label: m.label})
	}
	return caps, nil
}
