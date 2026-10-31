package app

import (
	"context"
	"fmt"
	"strings"

	"starcode/internal/agent"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// Background work is what an agent runs beside its turn: a command it
// started in the background, a subagent. It can outlive the turn, and it
// dies with the agent process, which a restart of starcode ends, and so
// do an idle close, a settle and "restart agent session". The agent is
// not told: the work just never reports back. So starcode keeps a row
// per task that has not ended (store.Background), marks them lost when
// the process goes, and tells the agent what it lost: ahead of the next
// prompt, or, after a restart that cut off work the agent was waiting on
// (a subagent rather than a dev server), in a prompt of its own.

// background records the start or the end of background work, and
// says in the transcript what ended while no turn ran, since the agent
// may take that up in a turn of its own next.
func (a *App) background(ctx context.Context, tid string, b *agent.Background, append_ func(...any)) {
	if !b.Done {
		append_(domain.ThreadBackgroundStarted{TaskID: b.ID, Kind: b.Kind, Label: b.Label})
		return
	}
	var task store.BackgroundTask
	tasks, _ := a.Store.Background(ctx, tid)
	for _, t := range tasks {
		if t.TaskID == b.ID && !t.Lost {
			task = t
		}
	}
	if task.TaskID == "" {
		return
	}
	append_(domain.ThreadBackgroundEnded{TaskID: b.ID, Status: b.Status})
	if t, err := a.Store.Thread(ctx, tid); err == nil && t.Status != domain.StatusRunning && t.Status != domain.StatusAwaitingApproval {
		how := "finished"
		switch b.Status {
		case "failed":
			how = "failed"
		case "stopped":
			how = "was stopped"
		}
		append_(domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Quiet: true,
			Body: fmt.Sprintf("Background %s %s: %s", task.Kind, how, taskLabel(task))})
	}
}

// loseBackground marks the thread's running background work lost, when
// its agent process is gone. It returns that work.
func (a *App) loseBackground(ctx context.Context, tid string) []store.BackgroundTask {
	tasks, err := a.Store.Background(ctx, tid)
	if err != nil {
		return nil
	}
	var live []store.BackgroundTask
	for _, t := range tasks {
		if !t.Lost {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return nil
	}
	if _, err := a.Store.Append(ctx, tid, domain.ThreadBackgroundLost{}); err != nil {
		a.Log.Warn("background lost", "thread", tid, "err", err)
		return nil
	}
	return live
}

// awaited is whether any of tasks is work the agent waits on: a
// subagent or another task, not a background command, which is mostly a
// dev server or a watcher nobody waits for.
func awaited(tasks []store.BackgroundTask) bool {
	for _, t := range tasks {
		if t.Kind != agent.BackgroundCommand {
			return true
		}
	}
	return false
}

// maxBackgroundLines bounds the note, which goes into the agent's context.
const maxBackgroundLines = 10

// backgroundNote tells the agent what background work it lost.
func backgroundNote(tasks []store.BackgroundTask) string {
	var b strings.Builder
	b.WriteString("[starcode] Your agent process was closed (starcode restarted, or the session ended) while this background work you started was still running. It was stopped and will not report back, so start again what you still need and wait for its result:\n")
	for i, t := range tasks {
		if i == maxBackgroundLines {
			fmt.Fprintf(&b, "- and %d more\n", len(tasks)-i)
			break
		}
		fmt.Fprintf(&b, "- %s: %s\n", t.Kind, taskLabel(t))
	}
	return b.String()
}

func taskLabel(t store.BackgroundTask) string {
	l := strings.Join(strings.Fields(t.Label), " ")
	if r := []rune(l); len(r) > 160 {
		l = string(r[:159]) + "…"
	}
	if l == "" {
		l = "(no description, task " + t.TaskID + ")"
	}
	return l
}

// pendingBackground takes the thread's lost background work as a note
// to send ahead of its next prompt, and marks it told.
func (a *App) pendingBackground(ctx context.Context, tid string) string {
	tasks, err := a.Store.Background(ctx, tid)
	if err != nil {
		return ""
	}
	var lost []store.BackgroundTask
	for _, t := range tasks {
		if t.Lost {
			lost = append(lost, t)
		}
	}
	if len(lost) == 0 {
		return ""
	}
	if _, err := a.Store.Append(ctx, tid, domain.ThreadBackgroundReported{}); err != nil {
		return ""
	}
	return backgroundNote(lost) + "\n[starcode] The prompt follows.\n\n"
}

// hasBackground is whether the thread has background work running.
func (a *App) hasBackground(ctx context.Context, tid string) bool {
	tasks, _ := a.Store.Background(ctx, tid)
	for _, t := range tasks {
		if !t.Lost {
			return true
		}
	}
	return false
}

// wakeForBackground prompts a thread whose awaited background work a
// restart cut off, with the note as the prompt, so it picks the work up
// without waiting for the reader.
func (a *App) wakeForBackground(ctx context.Context, id string) {
	tasks, err := a.Store.Background(ctx, id)
	if err != nil {
		return
	}
	var lost []store.BackgroundTask
	for _, t := range tasks {
		if t.Lost {
			lost = append(lost, t)
		}
	}
	if len(lost) == 0 {
		return
	}
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	t, err := a.Store.Thread(ctx, id)
	if err == nil && (t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval) {
		// Something else started a turn first; it carries the note.
		return
	}
	if err == nil {
		_, err = a.Store.Append(ctx, id, domain.ThreadBackgroundReported{})
	}
	var l *live
	if err == nil {
		l, err = a.session(ctx, t)
	}
	if err == nil {
		err = a.startPromptMeta(ctx, l, t, "", strings.TrimPrefix(backgroundNote(lost), "[starcode] ")+"\nCarry on with the task.", map[string]any{"auto": "resume"})
	}
	if err != nil {
		a.Log.Warn("background wake", "thread", id, "err", err)
		a.warn(ctx, id, "Could not resume the background work: "+err.Error())
	}
}
