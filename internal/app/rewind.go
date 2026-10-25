package app

import (
	"context"
	"errors"

	"starcode/internal/domain"
	"starcode/internal/store"
)

// CanRewind says whether the conversation can be cut back to before
// prompt it. A prompt that opened the conversation starts a fresh one; a
// later one needs the fork point its item recorded (prompts sent before
// starcode recorded them have none). first is whether it is the
// thread's first prompt.
func CanRewind(t store.Thread, it store.Item, first bool) bool {
	if it.Kind != domain.KindUser {
		return false
	}
	m := promptMeta(it)
	return first || m.Fresh || (m.Anchor != "" && (m.Agent == "" || m.Agent == t.Agent))
}

// errOtherAgent is why a rewind across an agent switch is refused.
var errOtherAgent = errors.New("this prompt went to another agent than the thread runs now, so its conversation cannot be picked up from here")

// Rewind cuts the thread's conversation back to just before prompt
// itemID: the prompt and everything after it leave the transcript, and
// the agent's next session is a fork of its conversation that ends where
// the prompt began (or a fresh one, for the first prompt), so the agent
// forgets those turns too. The files are left as they are. It returns
// the prompt's text for the composer, which also becomes the thread's
// draft.
func (a *App) Rewind(ctx context.Context, threadID, itemID string) (string, error) {
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
	if !CanRewind(t, it, first) {
		if m := promptMeta(it); m.Agent != "" && m.Agent != t.Agent {
			return "", errOtherAgent
		}
		return "", errors.New("this prompt went out before starcode recorded where to cut the conversation, so it cannot be rewound to")
	}
	m := promptMeta(it)
	a.closeSession(threadID)
	ev := domain.ThreadRewound{ItemID: itemID}
	if !first && !m.Fresh {
		ev.SessionID, ev.ForkAt = m.Session, m.Anchor
	}
	if _, err := a.Store.Append(ctx, threadID, ev,
		domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Body: "Rewound to before this prompt; the files were left as they are."}); err != nil {
		return "", err
	}
	if err := a.Store.SaveDraft(ctx, threadID, it.Body); err != nil {
		a.Log.Warn("save draft", "thread", threadID, "err", err)
	}
	return it.Body, nil
}
