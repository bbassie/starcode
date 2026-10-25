package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
)

// Branches and handoffs: a new thread that carries on from another one,
// which is left as it is. A branch keeps the agent and forks its
// conversation; a handoff gives the work to another agent, which cannot
// read the first one's session and gets the conversation as a file.

// errBusy is why a thread that is working cannot be branched or handed
// off whole: its last turn has no end to fork at yet.
var errBusy = errors.New("wait for the turn to end, or stop it, first")

// Branch opens a new thread that goes on from thread from. before names
// a prompt to branch off in front of, the cut a rewind to it would make;
// empty branches after the last turn. The new thread gets a copy of the
// transcript up to there and a fork of the agent's conversation at the
// same point, so the two go their own ways. Both work in the project's
// checkout, so the files are not split. It returns the new thread and,
// for a branch in front of a prompt, that prompt's text, which becomes
// the new thread's draft.
func (a *App) Branch(ctx context.Context, from, before string) (string, string, error) {
	t, err := a.Store.Thread(ctx, from)
	if err != nil {
		return "", "", err
	}
	items, err := a.Store.Items(ctx, from)
	if err != nil {
		return "", "", err
	}
	ev := domain.ThreadBranched{From: from}
	var keep []store.Item
	var draft string
	if before == "" {
		if busyThread(t) {
			return "", "", errBusy
		}
		keep = items
		if t.ExternalSessionID != "" {
			at := t.ForkAt
			if at == "" {
				at = t.Anchor
			}
			if at == "" {
				return "", "", errors.New("starcode has not recorded where this conversation can be forked yet; it does once a turn ends")
			}
			ev.SessionID, ev.ForkAt = t.ExternalSessionID, at
		}
	} else {
		i, first := -1, true
		for j, x := range items {
			if x.ID == before {
				i = j
				break
			}
			if x.Kind == domain.KindUser {
				first = false
			}
		}
		if i < 0 || items[i].Kind != domain.KindUser {
			return "", "", errors.New("that is not a prompt of this thread")
		}
		it := items[i]
		m := promptMeta(it)
		if !CanRewind(t, it, first) {
			if m.Agent != "" && m.Agent != t.Agent {
				return "", "", errOtherAgent
			}
			return "", "", errors.New("this prompt went out before starcode recorded where to cut the conversation, so it cannot be branched from")
		}
		keep = items[:i]
		if !first && !m.Fresh {
			ev.SessionID, ev.ForkAt = m.Session, m.Anchor
		}
		draft = it.Body
	}

	id := newID()
	title := "new thread"
	if len(keep) > 0 {
		title = t.Title + " (branch)"
	}
	evs := []any{
		domain.ThreadCreated{ID: id, ProjectID: t.ProjectID, Title: title, Agent: t.Agent, Model: t.Model},
		domain.ThreadSettingsChanged{Agent: t.Agent, Model: t.Model, Effort: t.Effort, PermissionMode: t.PermissionMode},
		ev,
	}
	evs = append(evs, copyItems(keep)...)
	if len(keep) > 0 {
		evs = append(evs, domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Quiet: true,
			Body: "Branched from \"" + t.Title + "\" here. The conversation above is a copy; the agent picks it up from this point, and the original thread goes on without this one."})
	}
	if _, err := a.Store.Append(ctx, id, evs...); err != nil {
		return "", "", err
	}

	if draft != "" {
		if err := a.Store.SaveDraft(ctx, id, draft); err != nil {
			a.Log.Warn("save draft", "thread", id, "err", err)
		}
	}
	return id, draft, nil
}

// Handoff is where a handoff goes: the agent and its settings.
type Handoff struct {
	Agent, Model, Effort, PermissionMode string
}

// handoffPrompt opens a thread that took work over from another agent;
// the conversation file is attached below it.
const handoffPrompt = "You are taking over this work from another coding agent (%s), which stopped here. Its whole conversation with me is in the attached file: read it first, then look at the working tree (git status and the diff) to see what it changed. Then tell me in a few lines where the work stands and what you would do next. Do not change anything yet."

// HandOff opens a thread on another agent that takes over thread from's
// work: in the same checkout, with a copy of the transcript, and a first
// prompt that points the new agent at the conversation written out as a
// file (it cannot read the other agent's session). The prompt goes out
// straight away, so the new agent is read in by the time anyone looks.
// The original thread is left as it is.
func (a *App) HandOff(ctx context.Context, from string, to Handoff) (string, error) {
	if _, ok := a.Agent(to.Agent); !ok {
		return "", fmt.Errorf("unknown agent %q", to.Agent)
	}
	t, err := a.Store.Thread(ctx, from)
	if err != nil {
		return "", err
	}
	if to.Agent == t.Agent {
		return "", errors.New("this thread already runs on " + to.Agent + "; branch it instead")
	}
	if busyThread(t) {
		return "", errBusy
	}
	if a.AttachDir == "" {
		return "", errors.New("attachments are not configured")
	}
	items, err := a.Store.Items(ctx, from)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", errors.New("there is nothing to hand over yet")
	}
	p, err := a.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return "", err
	}

	id := newID()
	evs := []any{
		domain.ThreadCreated{ID: id, ProjectID: t.ProjectID, Title: t.Title, Agent: to.Agent, Model: strings.TrimSpace(to.Model)},
		domain.ThreadSettingsChanged{Agent: to.Agent, Model: strings.TrimSpace(to.Model), Effort: strings.TrimSpace(to.Effort), PermissionMode: strings.TrimSpace(to.PermissionMode)},
		domain.ThreadBranched{From: from},
	}
	evs = append(evs, copyItems(items)...)
	evs = append(evs, domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Quiet: true,
		Body: "Handed over from \"" + t.Title + "\" (" + t.Agent + ") here. The conversation above is a copy; " + to.Agent + " gets it as the file attached below."})
	if _, err := a.Store.Append(ctx, id, evs...); err != nil {
		return "", err
	}

	if t.SnoozeResume {
		// The original was waiting out the usage limit to resume; the new
		// agent has the work now, and two in one checkout would clash.
		a.Store.Append(ctx, from, domain.ThreadWoken{})
		a.note(ctx, from, "Handed over to "+to.Agent+", so this thread no longer resumes when the usage limit resets.")
	}

	dir := filepath.Join(a.AttachDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return id, err
	}
	name := "conversation-" + gitx.Slug(t.Title) + ".md"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(Transcript(t, p, items)), 0o644); err != nil {
		return id, err
	}
	text := fmt.Sprintf(handoffPrompt, t.Agent) + "\n\n[Attached file \"" + name + "\" is saved at: " + path + "]"

	nt, err := a.Store.Thread(ctx, id)
	if err != nil {
		return id, err
	}
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	l, err := a.session(ctx, nt)
	if err == nil {
		err = a.startPromptMeta(ctx, l, nt, "", text, map[string]any{"auto": "handoff"})
	}
	if err != nil {
		// The thread stands with its history; the prompt can be sent by
		// hand once whatever failed is fixed.
		a.warn(ctx, id, "Could not start "+to.Agent+": "+err.Error())
		a.Store.SaveDraft(ctx, id, text)
	}
	return id, nil
}

// busyThread is whether a turn is under way on t.
func busyThread(t store.Thread) bool {
	return t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval
}

// copyItems is the events that give a new thread copies of items: new
// ids (an item id is unique across threads), the parent links of
// subagent activity moved to the copies, the original times kept.
func copyItems(items []store.Item) []any {
	ids := make(map[string]string, len(items))
	for _, it := range items {
		ids[it.ID] = newID()
	}
	evs := make([]any, 0, len(items))
	for _, it := range items {
		meta := it.Meta
		var m map[string]any
		if json.Unmarshal(meta, &m) == nil {
			if parent, ok := m["parent"].(string); ok && ids[parent] != "" {
				m["parent"] = ids[parent]
				meta, _ = json.Marshal(m)
			}
		}
		evs = append(evs, domain.ItemStarted{ID: ids[it.ID], Kind: it.Kind, ToolName: it.ToolName, Status: it.Status, Body: it.Body, Meta: meta, Quiet: true, At: it.CreatedAt})
		if it.Output != "" {
			evs = append(evs, domain.ItemDelta{ID: ids[it.ID], Field: "output", Text: it.Output})
		}
	}
	return evs
}

// transcriptCap bounds the conversation file a handoff writes. Past it
// the middle goes: the first prompt says what the work is, the end where
// it stands.
const transcriptCap = 200 << 10

// Transcript writes a thread's conversation as markdown for another agent
// to read: the prompts and replies in full, one line per tool call, and
// starcode's notes. Thinking and the activity of subagents are left out.
func Transcript(t store.Thread, p store.Project, items []store.Item) string {
	var head strings.Builder
	fmt.Fprintf(&head, "# %s\n\n", t.Title)
	fmt.Fprintf(&head, "A conversation between me and a coding agent (%s", t.Agent)
	if t.ResolvedModel != "" {
		fmt.Fprintf(&head, ", model %s", t.ResolvedModel)
	}
	fmt.Fprintf(&head, ") in %s", p.Path)
	head.WriteString(". Oldest first.\n")

	var parts []string
	for _, it := range items {
		var m struct {
			Parent  string `json:"parent"`
			Summary string `json:"summary"`
		}
		json.Unmarshal(it.Meta, &m)
		if m.Parent != "" {
			continue
		}
		body := strings.TrimSpace(it.Body)
		switch it.Kind {
		case domain.KindUser:
			parts = append(parts, "\n## Me ("+it.CreatedAt.UTC().Format("2006-01-02 15:04")+" UTC)\n\n"+body+"\n")
		case domain.KindAssistant:
			if body != "" {
				parts = append(parts, "\n## Agent\n\n"+body+"\n")
			}
		case domain.KindTool:
			line := it.ToolName
			if s := strings.TrimSpace(m.Summary); s != "" {
				line += ": " + firstLineOf(s)
			}
			if it.Status != "" && it.Status != domain.ItemDone {
				line += " (" + it.Status + ")"
			}
			parts = append(parts, "- tool "+line+"\n")
		case domain.KindError, domain.KindSystem:
			if body != "" {
				parts = append(parts, "\n> "+strings.ReplaceAll(body, "\n", "\n> ")+"\n")
			}
		}
	}
	n := head.Len()
	for _, s := range parts {
		n += len(s)
	}
	if n > transcriptCap && len(parts) > 2 {
		// Keep the first part and as much of the end as fits.
		size := head.Len() + len(parts[0])
		cut := len(parts)
		for cut > 1 && size+len(parts[cut-1]) <= transcriptCap {
			cut--
			size += len(parts[cut])
		}
		kept := append([]string{parts[0], fmt.Sprintf("\n[... %d earlier entries left out ...]\n", cut-1)}, parts[cut:]...)
		parts = kept
	}
	return head.String() + strings.Join(parts, "")
}

// firstLineOf is s up to its first newline.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
