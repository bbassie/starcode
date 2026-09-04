// Package domain defines the event types that make up the append-only log.
// Everything the UI shows is a projection of these events.
package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Event is one persisted log entry. Payload is one of the *Event structs
// below, selected by Type.
type Event struct {
	Seq       int64     `json:"seq"`
	ThreadID  string    `json:"thread_id,omitempty"`
	Type      string    `json:"type"`
	Payload   any       `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// Thread status values.
const (
	StatusIdle             = "idle"
	StatusRunning          = "running"
	StatusAwaitingApproval = "awaiting_approval"
	StatusError            = "error"
)

// Item kinds.
const (
	KindUser      = "user"
	KindAssistant = "assistant"
	KindThinking  = "thinking"
	KindTool      = "tool"
	KindResult    = "result"
	KindError     = "error"
	KindSystem    = "system"
)

// Item status values (tools and approvals).
const (
	ItemRunning   = "running"
	ItemDone      = "done"
	ItemFailed    = "failed"
	ItemDeclined  = "declined"
	ItemPending   = "pending"
	ItemCancelled = "cancelled"
)

// Approval decisions.
const (
	DecisionAllow        = "allow"
	DecisionAllowSession = "allow_session"
	DecisionDeny         = "deny"
)

type ProjectAdded struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
}

type ProjectRemoved struct {
	ID string `json:"id"`
}

type ThreadCreated struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	Agent     string `json:"agent"` // "claude" | "codex" | "fake"
	Model     string `json:"model"`
}

type ThreadRenamed struct {
	Title string `json:"title"`
}

type ThreadDeleted struct{}

// ThreadSettingsChanged updates how the thread's agent is launched. The
// agent itself can only change before the first prompt; the rest applies
// from the next turn on.
type ThreadSettingsChanged struct {
	Agent          string `json:"agent"`
	Model          string `json:"model"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

// AgentSessionBound records the agent-side session id so a thread can be
// resumed after the process (or starcode itself) restarts.
type AgentSessionBound struct {
	ExternalID string `json:"external_id"`
	// Model is the model the agent reports it is actually using (an alias
	// like "sonnet" resolves to a full name here). It is kept apart from
	// the user's choice. Empty means unknown.
	Model string `json:"model,omitempty"`
}

type ThreadStatusChanged struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type TurnStarted struct {
	TurnID string `json:"turn_id"`
}

// ItemStarted opens a new item in the transcript. Body may be empty and be
// filled by ItemDelta events (assistant text, thinking, tool output).
type ItemStarted struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	ToolName string          `json:"tool_name,omitempty"`
	Status   string          `json:"status,omitempty"`
	Body     string          `json:"body,omitempty"`
	Meta     json.RawMessage `json:"meta,omitempty"`
}

// ItemDelta appends text to an item. Field selects which text: "body"
// (default) or "output" (tool output kept in meta).
type ItemDelta struct {
	ID    string `json:"id"`
	Field string `json:"field,omitempty"`
	Text  string `json:"text"`
}

// ItemCompleted finalizes an item. Body, when non-empty, replaces the
// accumulated body (used when an adapter only sees the full text at the end).
type ItemCompleted struct {
	ID     string          `json:"id"`
	Status string          `json:"status,omitempty"`
	Body   string          `json:"body,omitempty"`
	Meta   json.RawMessage `json:"meta,omitempty"`
}

// ApprovalRequested is emitted when the agent needs a human decision.
type ApprovalRequested struct {
	ID          string          `json:"id"`      // adapter-scoped request id
	ItemID      string          `json:"item_id"` // tool item this concerns, may be empty
	ToolName    string          `json:"tool_name"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
}

type ApprovalResolved struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Auto     bool   `json:"auto,omitempty"` // resolved by a session rule
}

type TurnCompleted struct {
	TurnID     string  `json:"turn_id"`
	Status     string  `json:"status"` // "done" | "interrupted" | "error"
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	InputTok   int64   `json:"input_tokens,omitempty"`
	OutputTok  int64   `json:"output_tokens,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// GitChanged is not persisted; it is only published on the bus.
type GitChanged struct {
	ProjectID string `json:"project_id"`
}

// TypeOf returns the wire name for a payload.
func TypeOf(p any) string {
	switch p.(type) {
	case ProjectAdded, *ProjectAdded:
		return "project.added"
	case ProjectRemoved, *ProjectRemoved:
		return "project.removed"
	case ThreadCreated, *ThreadCreated:
		return "thread.created"
	case ThreadRenamed, *ThreadRenamed:
		return "thread.renamed"
	case ThreadDeleted, *ThreadDeleted:
		return "thread.deleted"
	case ThreadSettingsChanged, *ThreadSettingsChanged:
		return "thread.settings"
	case AgentSessionBound, *AgentSessionBound:
		return "thread.session_bound"
	case ThreadStatusChanged, *ThreadStatusChanged:
		return "thread.status"
	case TurnStarted, *TurnStarted:
		return "turn.started"
	case TurnCompleted, *TurnCompleted:
		return "turn.completed"
	case ItemStarted, *ItemStarted:
		return "item.started"
	case ItemDelta, *ItemDelta:
		return "item.delta"
	case ItemCompleted, *ItemCompleted:
		return "item.completed"
	case ApprovalRequested, *ApprovalRequested:
		return "approval.requested"
	case ApprovalResolved, *ApprovalResolved:
		return "approval.resolved"
	case GitChanged, *GitChanged:
		return "git.changed"
	}
	panic(fmt.Sprintf("domain: unknown event payload %T", p))
}

// Decode turns a stored (type, json) pair back into a typed payload.
func Decode(typ string, raw []byte) (any, error) {
	var p any
	switch typ {
	case "project.added":
		p = &ProjectAdded{}
	case "project.removed":
		p = &ProjectRemoved{}
	case "thread.created":
		p = &ThreadCreated{}
	case "thread.renamed":
		p = &ThreadRenamed{}
	case "thread.deleted":
		p = &ThreadDeleted{}
	case "thread.settings", "thread.agent":
		p = &ThreadSettingsChanged{}
	case "thread.session_bound":
		p = &AgentSessionBound{}
	case "thread.status":
		p = &ThreadStatusChanged{}
	case "turn.started":
		p = &TurnStarted{}
	case "turn.completed":
		p = &TurnCompleted{}
	case "item.started":
		p = &ItemStarted{}
	case "item.delta":
		p = &ItemDelta{}
	case "item.completed":
		p = &ItemCompleted{}
	case "approval.requested":
		p = &ApprovalRequested{}
	case "approval.resolved":
		p = &ApprovalResolved{}
	case "git.changed":
		p = &GitChanged{}
	default:
		return nil, fmt.Errorf("domain: unknown event type %q", typ)
	}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	return p, nil
}
