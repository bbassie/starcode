// Package domain defines the event types that make up the append-only log.
// Everything the UI shows is a projection of these events.
package domain

import (
	"encoding/json"
	"fmt"
	"strings"
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
	// Parent is the thread whose agent spawned this one through the
	// starcode tools, empty for a thread the reader opened. WakeParent
	// sends the parent a prompt with this thread's reply whenever it
	// finishes. ReportParent instead holds the reply until the next
	// prompt the parent gets, and sends it along with that one.
	Parent       string `json:"parent,omitempty"`
	WakeParent   bool   `json:"wake_parent,omitempty"`
	ReportParent bool   `json:"report_parent,omitempty"`
}

// ThreadReportPending marks a child with ReportParent that finished a turn
// since its parent last heard from it; ThreadReported clears the mark when
// its reply went to the parent.
type ThreadReportPending struct{}

type ThreadReported struct{}

type ThreadRenamed struct {
	Title string `json:"title"`
}

type ThreadDeleted struct{}

// ThreadArchived moves a thread out of the sidebar and the project cards
// without deleting it (the Settled section). Sending a prompt to an
// archived thread unarchives it first. Neither is activity: the thread
// keeps its place and its read state, so an undo puts it back as it was.
type ThreadArchived struct{}

// ThreadUnarchived brings a settled thread back. It starts the settle
// clock again, so the idle sweep leaves the thread alone for another
// settle period even when its last activity is older than that.
type ThreadUnarchived struct{}

// ThreadSnoozed parks a thread until a time: it leaves the list for the
// Snoozed shelf and comes back on its own when Until passes.
type ThreadSnoozed struct {
	Until time.Time `json:"until"`
	// Resume is a snooze starcode made because the turn hit the plan's
	// usage limit: when it runs out on time, the turn is resumed.
	Resume bool `json:"resume,omitempty"`
	// Prompt is sent to the thread when the snooze runs out on time: the
	// agent snoozed itself to look at something again later.
	Prompt string `json:"prompt,omitempty"`
}

// ThreadWoken ends a snooze. Auto is the sweep waking it on time, which
// counts as activity so the thread comes back on top and unread; a wake
// by hand does not.
type ThreadWoken struct {
	Auto bool `json:"auto,omitempty"`
}

// ThreadRewound cuts the conversation back to just before item ItemID, a
// prompt: that item and everything after it leave the transcript. The
// next agent session forks SessionID at ForkAt (an agent.Config ForkAt),
// or starts fresh when SessionID is empty, which is how the agent forgets
// the dropped turns too.
type ThreadRewound struct {
	ItemID    string `json:"item_id"`
	SessionID string `json:"session_id,omitempty"`
	ForkAt    string `json:"fork_at,omitempty"`
}

// ThreadBranched starts a thread as a branch of thread From, the first
// event after its ThreadCreated. Its transcript arrives as copies of
// From's items; the agent's first session forks SessionID at ForkAt, the
// same way a rewind does, or starts fresh when SessionID is empty (a
// handoff to another agent, or a branch before the first prompt).
type ThreadBranched struct {
	From      string `json:"from"`
	SessionID string `json:"session_id,omitempty"`
	ForkAt    string `json:"fork_at,omitempty"`
}

// ThreadWorktreeSet gave a thread a checkout of its own, back when
// starcode made worktrees. Nothing appends it any more and nothing
// applies it; it is only decoded so that old event logs still replay.
type ThreadWorktreeSet struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
}

// ProjectSettingsChanged is the per-project default for new threads:
// the agent, model, effort and permission mode they start with (Agent
// empty: no default, a new thread takes what the composer holds). Every
// event carries the whole set.
type ProjectSettingsChanged struct {
	ID     string `json:"id"`
	Agent  string `json:"agent,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

// ThreadPinned keeps a thread at the top of its project's list; pinning
// is not activity, so it leaves updated_at and the unread mark alone.
type ThreadPinned struct{}

type ThreadUnpinned struct{}

// ThreadPRLinked ties a thread to the pull request it produced or works
// on. The agent running `gh pr create` sets it, as does opening a thread
// from a PR; the reader can also set or clear it by hand. Only the newest
// link counts.
type ThreadPRLinked struct {
	Repo   string `json:"repo"` // "owner/name"
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type ThreadPRUnlinked struct{}

// ThreadPRWatchSet starts the watch on the thread's linked pull request,
// or records where it stands: State is the app's bookkeeping (what the
// agent was last told) as JSON. A link to another PR, or an unlink, ends
// the watch.
type ThreadPRWatchSet struct {
	State json.RawMessage `json:"state"`
}

// ScheduleSet adds a scheduled prompt or replaces one with the same ID.
// With ThreadID the prompt goes to that thread; without, every run opens
// a new thread in ProjectID with Agent, Model, Effort and Mode (empty:
// the project's defaults). At is the next run. Cron, in the zone TZ,
// makes it repeat; without it the schedule is one prompt sent later. By
// is the thread whose agent made it, empty for the reader.
type ScheduleSet struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"thread_id,omitempty"`
	ProjectID string    `json:"project_id"`
	Prompt    string    `json:"prompt"`
	Title     string    `json:"title,omitempty"`
	At        time.Time `json:"at"`
	Cron      string    `json:"cron,omitempty"`
	TZ        string    `json:"tz,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Model     string    `json:"model,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Paused    bool      `json:"paused,omitempty"`
	By        string    `json:"by,omitempty"`
}

// ScheduleRan records a run: the thread it went to (Thread), what failed
// (Error), and the next run, zero when there is none, which ends the
// schedule.
type ScheduleRan struct {
	ID     string    `json:"id"`
	Thread string    `json:"thread,omitempty"`
	Error  string    `json:"error,omitempty"`
	Next   time.Time `json:"next"`
}

// ScheduleRemoved deletes a schedule.
type ScheduleRemoved struct {
	ID string `json:"id"`
}

// ThreadPRWatchStopped ends the watch; Reason says why when starcode
// stopped it, and is empty when the reader did.
type ThreadPRWatchStopped struct {
	Reason string `json:"reason,omitempty"`
}

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

// PromptQueued holds a follow-up while another turn is still running. It is
// kept outside the transcript until dispatched so the visible conversation
// remains in chronological turn order.
type PromptQueued struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

type PromptDequeued struct {
	ID string `json:"id"`
}

// PromptEdited changes the text of a queued prompt.
type PromptEdited struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// PromptsReordered puts the thread's queued prompts in the order of IDs.
type PromptsReordered struct {
	IDs []string `json:"ids"`
}

// ItemStarted opens a new item in the transcript. Body may be empty and be
// filled by ItemDelta events (assistant text, thinking, tool output).
type ItemStarted struct {
	ID string `json:"id"`
	// Quiet is a line starcode writes for the record (a branch was made,
	// copied items) that is not activity: the
	// thread keeps its place and does not go unread over it.
	Quiet    bool            `json:"quiet,omitempty"`
	Kind     string          `json:"kind"`
	ToolName string          `json:"tool_name,omitempty"`
	Status   string          `json:"status,omitempty"`
	Body     string          `json:"body,omitempty"`
	Meta     json.RawMessage `json:"meta,omitempty"`
	// At is when a copied item (a branch's transcript) first happened;
	// zero for a new one.
	At time.Time `json:"at,omitempty"`
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
	Reason      string          `json:"reason,omitempty"` // why the agent asked, see agent.ApprovalRequested
	Input       json.RawMessage `json:"input,omitempty"`
}

type ApprovalResolved struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Auto     bool   `json:"auto,omitempty"` // resolved by a session rule
	// Answers is set when the approval was a question to the human (see
	// Questions): each question's text to what they picked or wrote.
	Answers map[string]string `json:"answers,omitempty"`
}

// Question is one of the questions an AskUserQuestion call puts to the
// human. Each has 2 to 4 options; the human may also write their own
// answer.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Questions returns the questions of an approval that asks the human
// something instead of asking for permission, or nil for any other.
func Questions(tool string, input json.RawMessage) []Question {
	if tool != "AskUserQuestion" {
		return nil
	}
	var in struct {
		Questions []Question `json:"questions"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	return in.Questions
}

// RuleRevoked drops an "allow for session" rule (see RuleKey) from a
// thread, so the next matching request asks again.
type RuleRevoked struct {
	Key string `json:"key"`
}

// RuleKey reduces an approval to what a session rule remembers: Bash
// commands by their first word, everything else by tool name.
func RuleKey(tool string, input json.RawMessage) string {
	if tool == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		json.Unmarshal(input, &in)
		if f := strings.Fields(in.Command); len(f) > 0 {
			return "Bash:" + f[0]
		}
	}
	return tool
}

// ContextUsed records how full the model's context window is. Only the
// newest one counts, so the projection keeps it on the thread rather than
// in the transcript. Window is 0 when the agent did not name a limit and
// the reader falls back to the model catalog.
type ContextUsed struct {
	Tokens int64 `json:"tokens"`
	Window int64 `json:"window,omitempty"`
}

type TurnCompleted struct {
	TurnID string `json:"turn_id"`
	// Anchor is where the agent's conversation can be forked to keep this
	// turn and everything before it (see agent.TurnCompleted); the next
	// prompt records it so a rewind to that prompt knows where to cut.
	Anchor     string  `json:"anchor,omitempty"`
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

// LimitsChanged is bus-only: an instance's subscription windows moved
// (a probe, or a window an agent streamed mid-turn).
type LimitsChanged struct{}

// ProvidersChanged is bus-only: an agent CLI's version, sign-in state or
// available update changed, so sidebars and the providers page refresh.
type ProvidersChanged struct{}

// AgentNotified is bus-only: a thread's agent asked for the reader's
// attention (the notify tool), which every open page shows.
type AgentNotified struct {
	ThreadID string `json:"thread_id"`
	Title    string `json:"title"`
	Text     string `json:"text"`
}

// NotifyLevelChanged is bus-only: the reader chose what thread ThreadID
// pushes, which its header shows.
type NotifyLevelChanged struct {
	ThreadID string `json:"thread_id"`
}

// SeenChanged is bus-only: a thread was marked seen and its row should
// lose the unread mark on every other page.
type SeenChanged struct {
	ThreadID string `json:"thread_id"`
}

// PRStateChanged is bus-only: the poll of linked pull requests found a
// state that differs from the last one, so sidebars redraw their chips.
type PRStateChanged struct{}

// BinaryUpdated is bus-only: the starcode executable on disk is newer than
// the running one, so every open page shows the restart banner.
type BinaryUpdated struct{}

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
	case ThreadArchived, *ThreadArchived:
		return "thread.archived"
	case ThreadUnarchived, *ThreadUnarchived:
		return "thread.unarchived"
	case ThreadSnoozed, *ThreadSnoozed:
		return "thread.snoozed"
	case ThreadWoken, *ThreadWoken:
		return "thread.woken"
	case ThreadRewound, *ThreadRewound:
		return "thread.rewound"
	case ThreadBranched, *ThreadBranched:
		return "thread.branched"
	case ThreadWorktreeSet, *ThreadWorktreeSet:
		return "thread.worktree_set"
	case ProjectSettingsChanged, *ProjectSettingsChanged:
		return "project.settings_changed"
	case ThreadPinned, *ThreadPinned:
		return "thread.pinned"
	case ThreadUnpinned, *ThreadUnpinned:
		return "thread.unpinned"
	case ThreadPRLinked, *ThreadPRLinked:
		return "thread.pr_linked"
	case ThreadPRUnlinked, *ThreadPRUnlinked:
		return "thread.pr_unlinked"
	case ThreadPRWatchSet, *ThreadPRWatchSet:
		return "thread.pr_watch_set"
	case ThreadPRWatchStopped, *ThreadPRWatchStopped:
		return "thread.pr_watch_stopped"
	case ThreadReportPending, *ThreadReportPending:
		return "thread.report_pending"
	case ThreadReported, *ThreadReported:
		return "thread.reported"
	case ScheduleSet, *ScheduleSet:
		return "schedule.set"
	case ScheduleRan, *ScheduleRan:
		return "schedule.ran"
	case ScheduleRemoved, *ScheduleRemoved:
		return "schedule.removed"
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
	case ContextUsed, *ContextUsed:
		return "thread.context"
	case PromptQueued, *PromptQueued:
		return "prompt.queued"
	case PromptDequeued, *PromptDequeued:
		return "prompt.dequeued"
	case PromptEdited, *PromptEdited:
		return "prompt.edited"
	case PromptsReordered, *PromptsReordered:
		return "prompt.reordered"
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
	case RuleRevoked, *RuleRevoked:
		return "rule.revoked"
	case GitChanged, *GitChanged:
		return "git.changed"
	case ProvidersChanged, *ProvidersChanged:
		return "providers.changed"
	case BinaryUpdated, *BinaryUpdated:
		return "binary.updated"
	case PRStateChanged, *PRStateChanged:
		return "pr.state_changed"
	case SeenChanged, *SeenChanged:
		return "seen.changed"
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
	case "thread.archived":
		p = &ThreadArchived{}
	case "thread.unarchived":
		p = &ThreadUnarchived{}
	case "thread.snoozed":
		p = &ThreadSnoozed{}
	case "thread.woken":
		p = &ThreadWoken{}
	case "thread.rewound":
		p = &ThreadRewound{}
	case "thread.branched":
		p = &ThreadBranched{}
	case "thread.worktree_set":
		p = &ThreadWorktreeSet{}
	case "project.settings_changed":
		p = &ProjectSettingsChanged{}
	case "thread.pinned":
		p = &ThreadPinned{}
	case "thread.unpinned":
		p = &ThreadUnpinned{}
	case "thread.pr_linked":
		p = &ThreadPRLinked{}
	case "thread.pr_unlinked":
		p = &ThreadPRUnlinked{}
	case "thread.pr_watch_set":
		p = &ThreadPRWatchSet{}
	case "thread.pr_watch_stopped":
		p = &ThreadPRWatchStopped{}
	case "thread.report_pending":
		p = &ThreadReportPending{}
	case "thread.reported":
		p = &ThreadReported{}
	case "schedule.set":
		p = &ScheduleSet{}
	case "schedule.ran":
		p = &ScheduleRan{}
	case "schedule.removed":
		p = &ScheduleRemoved{}
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
	case "thread.context":
		p = &ContextUsed{}
	case "prompt.queued":
		p = &PromptQueued{}
	case "prompt.dequeued":
		p = &PromptDequeued{}
	case "prompt.edited":
		p = &PromptEdited{}
	case "prompt.reordered":
		p = &PromptsReordered{}
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
	case "rule.revoked":
		p = &RuleRevoked{}
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
