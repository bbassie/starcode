package app

import (
	"context"
	"encoding/json"

	"starcode/internal/domain"
	"starcode/internal/store"
	"starcode/internal/textdiff"
)

// TurnFiles is what a turn changed, recorded in its result line's meta
// for the transcript's changed-files card. The files come from the
// turn's edit tool calls, which miss what a command changed.
type TurnFiles struct {
	Files []FileStat `json:"files"`
}

// FileStat is one file a turn changed and its added and deleted lines.
type FileStat struct {
	Path string `json:"path"`
	Add  int    `json:"add"`
	Del  int    `json:"del"`
}

// recordTurnFiles works out what the turn that ended with result item
// resultID changed and puts it on that item.
func (a *App) recordTurnFiles(threadID, resultID string) {
	ctx := context.Background()
	items, err := a.Store.Items(ctx, threadID)
	if err != nil {
		return
	}
	end := -1
	for i, it := range items {
		if it.ID == resultID {
			end = i
			break
		}
	}
	if end < 0 {
		return
	}
	// The turn runs back to its prompt, or to the end of the turn
	// before when it had none (a compaction).
	start := 0
	for i := end - 1; i >= 0; i-- {
		if items[i].Kind == domain.KindUser {
			start = i + 1
			break
		}
		if items[i].Kind == domain.KindResult {
			start = i + 1
			break
		}
	}
	tf := TurnFiles{Files: editedFiles(items[start:end])}
	if len(tf.Files) == 0 {
		return
	}
	raw, _ := json.Marshal(tf)
	if _, err := a.Store.Append(ctx, threadID, domain.ItemCompleted{ID: resultID, Meta: raw}); err != nil {
		a.Log.Warn("record turn files", "thread", threadID, "err", err)
	}
}

// editedFiles sums up the edit tool calls that went through, per file:
// Claude's Edit, MultiEdit, Write and NotebookEdit and codex's patches,
// in the order they were first touched. A Write counts its lines as
// added, since what it replaced is not in the call.
func editedFiles(items []store.Item) []FileStat {
	var order []string
	byPath := map[string]*FileStat{}
	add := func(path string, lines []textdiff.Line) {
		if path == "" {
			return
		}
		st := byPath[path]
		if st == nil {
			st = &FileStat{Path: path}
			byPath[path] = st
			order = append(order, path)
		}
		n, d := textdiff.Count(lines)
		st.Add += n
		st.Del += d
	}
	for _, it := range items {
		if it.Kind != domain.KindTool || it.Status != domain.ItemDone {
			continue
		}
		var m struct {
			Input struct {
				FilePath     string `json:"file_path"`
				NotebookPath string `json:"notebook_path"`
				OldString    string `json:"old_string"`
				NewString    string `json:"new_string"`
				Content      string `json:"content"`
				NewSource    string `json:"new_source"`
				Edits        []struct {
					OldString string `json:"old_string"`
					NewString string `json:"new_string"`
				} `json:"edits"`
				Changes []struct {
					Path string `json:"path"`
					Kind string `json:"kind"`
					Diff string `json:"diff"`
				} `json:"changes"`
			} `json:"input"`
		}
		if json.Unmarshal(it.Meta, &m) != nil {
			continue
		}
		in := m.Input
		switch it.ToolName {
		case "Edit":
			if len(in.Changes) > 0 {
				for _, c := range in.Changes {
					if c.Kind == "add" && len(c.Diff) > 0 && c.Diff[0] != '@' {
						add(c.Path, textdiff.Lines("", c.Diff))
					} else {
						add(c.Path, textdiff.Hunks(c.Diff))
					}
				}
				continue
			}
			add(in.FilePath, textdiff.Lines(in.OldString, in.NewString))
		case "MultiEdit":
			var lines []textdiff.Line
			for _, e := range in.Edits {
				lines = append(lines, textdiff.Lines(e.OldString, e.NewString)...)
			}
			add(in.FilePath, lines)
		case "Write":
			add(in.FilePath, textdiff.Lines("", in.Content))
		case "NotebookEdit":
			add(in.NotebookPath, textdiff.Lines("", in.NewSource))
		}
	}
	out := make([]FileStat, 0, len(order))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	return out
}
