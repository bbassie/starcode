package views

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"starcode/internal/store"
)

// promptMeta is what a prompt's item recorded as it went out (see
// app.PromptMeta): the session and fork point a rewind needs, and Auto
// for a prompt starcode sent itself.
type promptMeta struct {
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Anchor  string `json:"anchor"`
	Auto    string `json:"auto"`
	Fresh   bool   `json:"fresh"`
	// PR is the pull request a "prwatch" prompt reports on.
	PR int `json:"pr"`
	// Reports are the child threads whose replies went ahead of the
	// prompt.
	Reports []string `json:"reports"`
	// Steered is a prompt sent into a running turn, which the agent took
	// where it stands in the transcript.
	Steered bool `json:"steered"`
}

func promptMetaOf(it store.Item) promptMeta {
	var m promptMeta
	json.Unmarshal(it.Meta, &m)
	return m
}

// rewindable is whether the transcript offers a rewind to the prompt: it
// began the conversation, or it has the fork point. Prompts from before
// starcode recorded either get no button.
func (m promptMeta) rewindable() bool {
	return m.Fresh || m.Anchor != ""
}

// rewindAction asks, then posts the rewind to before prompt it.
func rewindAction(it store.Item) string {
	msg := "Rewind to before this message?\n\nIt and everything after it leave the conversation, and the agent forgets them. The message comes back into the composer to edit.\n\nFiles stay as they are."
	url := "/api/threads/" + it.ThreadID + "/rewind/" + it.ID
	return "confirm(" + jsq(msg) + " + ($prompt.trim() ? '\\n\\nWhat is in the composer now is replaced.' : '')) && @post('" + url + "')"
}

// threadRef matches how starcode names a thread in a message it passes
// on: thread "title" (id), as send_to_thread and a finished child's
// report write it.
var threadRef = regexp.MustCompile(`[Tt]hread "((?:[^"\\]|\\.)*)" \(([0-9a-f]{16,})\)`)

type threadLink struct{ ID, Title string }

// fromThreads are the threads a prompt that another thread's agent sent,
// or that carried finished children's replies, names, each once.
func fromThreads(m promptMeta, text string) []threadLink {
	if m.Auto != "agent" && m.Auto != "child" {
		return nil
	}
	var out []threadLink
	seen := map[string]bool{}
	for _, g := range threadRef.FindAllStringSubmatch(text, -1) {
		if seen[g[2]] {
			continue
		}
		seen[g[2]] = true
		title, err := strconv.Unquote(`"` + g[1] + `"`)
		if err != nil {
			title = g[1]
		}
		out = append(out, threadLink{ID: g[2], Title: title})
	}
	return out
}

// autoSummary is the one line a prompt starcode or another agent sent
// folds to: an icon, what it was and the start of its text, past the
// lines that name a thread. A prompt from the reader, a scheduled one
// (often the reader's own words, sent later) and a steered one give ""
// and show whole.
func autoSummary(m promptMeta, text string) (icon, label, preview string) {
	switch m.Auto {
	case "resume":
		icon, label = "rotate-cw", "Resumed after a restart"
	case "limit":
		icon, label = "rotate-cw", "Resumed when the usage limit reset"
	case "prwatch":
		icon, label = "eye", fmt.Sprintf("Pull request #%d changed", m.PR)
	case "handoff":
		icon, label = "arrow-right-left", "Handed over from another thread"
	case "snooze":
		icon, label = "alarm-clock", "Back from snooze"
	case "agent", "child":
		icon = "message-square"
		from := fromThreads(m, text)
		switch {
		case m.Auto == "agent" && len(from) > 0:
			label = "Message from " + from[0].Title
		case m.Auto == "agent":
			label = "Message from another thread"
		case len(from) == 1:
			label = "Reply from " + from[0].Title
		default:
			label = fmt.Sprintf("Replies from %d threads", len(from))
		}
	default:
		return "", "", ""
	}
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !threadRef.MatchString(line) {
			return icon, label, firstLine(line)
		}
	}
	return icon, label, ""
}
