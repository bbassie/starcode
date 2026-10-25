package views

import (
	"encoding/json"

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
