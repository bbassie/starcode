package views

import (
	"encoding/json"

	"starcode/internal/store"
)

// promptMeta is what a prompt's item recorded as it went out (see
// app.PromptMeta): the session and fork point a rewind needs, the
// worktree checkpoint, and Auto for a prompt starcode sent itself.
type promptMeta struct {
	Agent      string `json:"agent"`
	Session    string `json:"session"`
	Anchor     string `json:"anchor"`
	Checkpoint string `json:"checkpoint"`
	Auto       string `json:"auto"`
	Fresh      bool   `json:"fresh"`
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
func rewindAction(it store.Item, files bool) string {
	msg := "Rewind to before this message?\n\nIt and everything after it leave the conversation, and the agent forgets them. The message comes back into the composer to edit."
	url := "/api/threads/" + it.ThreadID + "/rewind/" + it.ID
	if files {
		msg += "\n\nThe worktree's files also go back to how they were when it was sent: changes and commits made since are undone (commits stay in the reflog)."
		url += "?files=1"
	} else {
		msg += "\n\nFiles stay as they are."
	}
	return "confirm(" + jsq(msg) + " + ($prompt.trim() ? '\\n\\nWhat is in the composer now is replaced.' : '')) && @post('" + url + "')"
}
