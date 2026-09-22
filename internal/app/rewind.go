package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
)

// RewindInfo is what the transcript needs to offer a rewind on a prompt:
// whether the conversation can be cut there, and whether the files can
// be put back too.
type RewindInfo struct {
	OK    bool
	Files bool
}

// CanRewind says what a rewind to prompt it could do. A prompt that
// opened the conversation starts a fresh one; a later one needs the fork
// point its item recorded (prompts sent before starcode recorded them
// have none). Files need the checkpoint and a worktree no other thread
// uses. first is whether it is the thread's first prompt.
func CanRewind(t store.Thread, it store.Item, first bool, shared bool) RewindInfo {
	if it.Kind != domain.KindUser {
		return RewindInfo{}
	}
	m := promptMeta(it)
	ok := first || (m.Anchor != "" && (m.Agent == "" || m.Agent == t.Agent))
	return RewindInfo{OK: ok, Files: ok && m.Checkpoint != "" && t.Worktree != "" && !shared}
}

// errOtherAgent is why a rewind across an agent switch is refused.
var errOtherAgent = errors.New("this prompt went to another agent than the thread runs now, so its conversation cannot be picked up from here")

// Rewind cuts the thread's conversation back to just before prompt
// itemID: the prompt and everything after it leave the transcript, and
// the agent's next session is a fork of its conversation that ends where
// the prompt began (or a fresh one, for the first prompt), so the agent
// forgets those turns too. With files the worktree goes back to the
// checkpoint taken as the prompt went out. It returns the prompt's text
// for the composer, which also becomes the thread's draft.
func (a *App) Rewind(ctx context.Context, threadID, itemID string, files bool) (string, error) {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	t, err := a.Store.Thread(ctx, threadID)
	if err != nil {
		return "", err
	}
	if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
		return "", errors.New("stop the running turn first")
	}
	items, err := a.Store.Items(ctx, threadID)
	if err != nil {
		return "", err
	}
	var it store.Item
	first, found := true, false
	for _, x := range items {
		if x.ID == itemID {
			it, found = x, true
			break
		}
		if x.Kind == domain.KindUser {
			first = false
		}
	}
	if !found || it.Kind != domain.KindUser {
		return "", errors.New("that is not a prompt of this thread")
	}
	info := CanRewind(t, it, first, t.Worktree != "" && a.sharedWorktree(ctx, t))
	if !info.OK {
		if m := promptMeta(it); m.Agent != "" && m.Agent != t.Agent {
			return "", errOtherAgent
		}
		return "", errors.New("this prompt went out before starcode recorded where to cut the conversation, so it cannot be rewound to")
	}
	if files && !info.Files {
		return "", errors.New("the files cannot be put back for this prompt: that needs a worktree of the thread's own and a checkpoint from when it was sent")
	}
	m := promptMeta(it)
	a.closeSession(threadID)
	if files {
		if _, err := os.Stat(t.Worktree); err != nil {
			return "", fmt.Errorf("the worktree is gone: %w", err)
		}
		if err := gitx.RestoreCheckpoint(ctx, t.Worktree, m.Checkpoint); err != nil {
			return "", fmt.Errorf("put the files back: %w", err)
		}
	}
	ev := domain.ThreadRewound{ItemID: itemID}
	if !first {
		ev.SessionID, ev.ForkAt = m.Session, m.Anchor
	}
	note := "Rewound to before this prompt; the files were left as they are."
	if files {
		note = "Rewound to before this prompt, and put the worktree's files back as they were then."
	}
	if _, err := a.Store.Append(ctx, threadID, ev,
		domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Body: note}); err != nil {
		return "", err
	}
	a.Bus.Publish(domain.GitChanged{ProjectID: t.ProjectID})
	if err := a.Store.SaveDraft(ctx, threadID, it.Body); err != nil {
		a.Log.Warn("save draft", "thread", threadID, "err", err)
	}
	return it.Body, nil
}
