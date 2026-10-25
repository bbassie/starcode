package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"starcode/internal/agent"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// RetitleThread asks the thread's agent, on its small model, for a title
// that says what the thread is about now, and renames the thread to it.
// The first prompt makes a poor title once the work has moved on, and the
// agent's own title is written after the first turn and never again.
func (a *App) RetitleThread(ctx context.Context, id string) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	ag, ok := a.Agent(t.Agent)
	if !ok {
		return fmt.Errorf("agent %q is not available", t.Agent)
	}
	asker, ok := ag.(agent.Asker)
	if !ok {
		return fmt.Errorf("%s cannot write a title", t.Agent)
	}
	items, err := a.Store.Items(ctx, id)
	if err != nil {
		return err
	}
	conv := titleExcerpt(items)
	if conv == "" {
		return errors.New("the thread has no conversation to name yet")
	}
	answer, err := asker.Ask(ctx, titlePrompt(t.Title, conv))
	if err != nil {
		return err
	}
	title := cleanTitle(answer)
	if title == "" {
		return errors.New("the agent did not write a title")
	}
	if title == t.Title {
		return nil
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadRenamed{Title: title})
	return err
}

// titlePrompt is the request for a title. The rules follow what makes a
// title useful in the sidebar weeks later: the subject and the outcome,
// not the workflow.
func titlePrompt(previous, conv string) string {
	return `Write a new title for this coding conversation, so I can recognize it in a list weeks from now.
The current title is ` + fmt.Sprintf("%q", previous) + `.

Rules:
- Name the subject and the goal of the work as it stands at the end, not the first request if the work moved on.
- 3 to 8 words, sentence case, no quotes, no trailing period, no emoji.
- Leave out tools, models, agents and process words ("help with", "review", "continue") unless they are the topic.
- Keep the current title if it is still accurate.

Reply with the title only.

The conversation, oldest first:

` + conv
}

// titleExcerptCap bounds what goes to the small model.
const titleExcerptCap = 12000

// titleExcerpt is the prompts and replies of a thread, each clipped, with
// the middle dropped when it is long: the start says what was asked, the
// end where it went.
func titleExcerpt(items []store.Item) string {
	var parts []string
	for _, it := range items {
		var m struct {
			Parent string `json:"parent"`
		}
		json.Unmarshal(it.Meta, &m)
		body := strings.TrimSpace(it.Body)
		if m.Parent != "" || body == "" {
			continue
		}
		switch it.Kind {
		case domain.KindUser:
			parts = append(parts, "Me: "+clipRunes(body, 1500))
		case domain.KindAssistant:
			parts = append(parts, "Agent: "+clipRunes(body, 1500))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	n := 0
	for _, p := range parts {
		n += len(p) + 2
	}
	if n <= titleExcerptCap || len(parts) < 3 {
		return strings.Join(parts, "\n\n")
	}
	// The first part, then as many of the last as fit.
	size := len(parts[0]) + 2
	cut := len(parts)
	for cut > 1 && size+len(parts[cut-1])+2 <= titleExcerptCap {
		cut--
		size += len(parts[cut]) + 2
	}
	return parts[0] + "\n\n[…]\n\n" + strings.Join(parts[cut:], "\n\n")
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// cleanTitle takes the first line of an answer and strips what a model
// adds around a title despite being asked not to.
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	s, _, _ = strings.Cut(s, "\n")
	for range 2 {
		s = strings.Trim(strings.TrimSpace(s), "\"'`*#“”")
		s = strings.TrimPrefix(s, "Title:")
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return strings.TrimSpace(s)
}
