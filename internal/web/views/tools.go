package views

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"starcode/internal/domain"
	"starcode/internal/store"
	"starcode/internal/textdiff"
)

// How a tool call reads in the transcript. A row is an icon for the kind
// of call and a line in words ("Read internal/app/app.go"); opening it
// shows what is worth reading for that kind: a command and its output, an
// edit as a diff, the plan as a checklist. Kinds come from the tool names
// the adapters use: Claude's own, and the Bash, Edit and WebSearch the
// codex adapter maps its items to.

// Tool kinds.
const (
	toolCommand = "command"
	toolRead    = "read"
	toolEdit    = "edit"
	toolSearch  = "search"
	toolWeb     = "web"
	toolAgent   = "agent"
	toolPlan    = "plan"
	toolOther   = "other"
)

// toolView is a tool call as the row and its body show it.
type toolView struct {
	Kind string
	Icon string
	// The row: Verb in plain text, then Text (words) or Code (a command,
	// a path, a pattern: monospace), then Note, dimmed.
	Verb, Text, Code, Note string
	// Command is the command a command tool ran, for the body.
	Command string
	// Files are an edit's changes, each with its diff.
	Files []fileDiff
	// Todos is a plan update's list.
	Todos []todo
	// Paths the call touched, for the group's summary: files read or
	// changed.
	Paths []string
}

// fileDiff is one file's change in an edit.
type fileDiff struct {
	Path  string
	Lines []diffLine
	// Clipped is how many lines were left out of a long diff.
	Clipped  int
	Add, Del int
}

type todo struct {
	Content, Status string
}

// diffLine is one line of a diff (see textdiff.Line).
type diffLine = textdiff.Line

type dirKey struct{}

// WithDir tells the views which folder a thread works in, so the paths
// in its tool calls show relative to it.
func WithDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, dirKey{}, filepath.Clean(dir))
}

// relPath is p relative to the thread's folder when it is inside it, and
// with the home directory as ~ otherwise.
func relPath(ctx context.Context, p string) string {
	if p == "" {
		return ""
	}
	if dir, _ := ctx.Value(dirKey{}).(string); dir != "" && dir != "." {
		if p == dir {
			return "."
		}
		if strings.HasPrefix(p, dir+"/") {
			return p[len(dir)+1:]
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+"/") {
		return "~/" + p[len(home)+1:]
	}
	return p
}

// relText drops the thread's folder from the paths in a command's text.
func relText(ctx context.Context, s string) string {
	if dir, _ := ctx.Value(dirKey{}).(string); dir != "" && dir != "." && dir != "/" {
		s = strings.ReplaceAll(s, dir+"/", "")
	}
	return s
}

// toolInput is the fields the kinds read from a tool's input; each tool
// fills its own.
type toolInput struct {
	Command     string        `json:"command"`
	Description string        `json:"description"`
	FilePath    string        `json:"file_path"`
	NotebookPth string        `json:"notebook_path"`
	Offset      int           `json:"offset"`
	Limit       int           `json:"limit"`
	OldString   string        `json:"old_string"`
	NewString   string        `json:"new_string"`
	Content     string        `json:"content"`
	NewSource   string        `json:"new_source"`
	Edits       []editPair    `json:"edits"`
	Changes     []codexChange `json:"changes"`
	Pattern     string        `json:"pattern"`
	Path        string        `json:"path"`
	Glob        string        `json:"glob"`
	URL         string        `json:"url"`
	Query       string        `json:"query"`
	Todos       []todo        `json:"todos"`
}

type editPair struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

type codexChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Diff string `json:"diff"`
}

// viewTool works out how tool item it shows.
func viewTool(ctx context.Context, it store.Item) toolView {
	m := meta(it)
	var in toolInput
	json.Unmarshal(m.Input, &in)
	name := it.ToolName
	lower := strings.ToLower(name)
	v := toolView{Kind: toolOther, Icon: "wrench", Verb: name, Text: m.Summary}
	switch {
	case name == "Bash" || name == "Monitor" || name == "BashOutput" || strings.Contains(lower, "shell") || strings.Contains(lower, "command"):
		v.Kind, v.Icon, v.Verb, v.Text = toolCommand, "terminal", "", ""
		v.Command = unwrapShell(in.Command)
		switch {
		case strings.TrimSpace(in.Description) != "":
			v.Text = strings.TrimSpace(in.Description)
		case v.Command != "":
			v.Code = relText(ctx, commandLine(v.Command))
		case name == "BashOutput":
			v.Text = "Read the output of a background command"
		default:
			v.Text = m.Summary
		}
	case name == "Read" || name == "NotebookRead":
		v.Kind, v.Icon, v.Verb, v.Text = toolRead, "eye", "Read", ""
		p := firstNonEmpty(in.FilePath, in.NotebookPth)
		v.Code = relPath(ctx, p)
		v.Paths = []string{p}
		if in.Offset > 0 || in.Limit > 0 {
			from := max(in.Offset, 1)
			if in.Limit > 0 {
				v.Note = fmt.Sprintf("lines %d–%d", from, from+in.Limit-1)
			} else {
				v.Note = fmt.Sprintf("from line %d", from)
			}
		}
	case name == "Edit" || name == "MultiEdit" || name == "Write" || name == "NotebookEdit":
		v.Kind, v.Icon, v.Text = toolEdit, "square-pen", ""
		v.Verb = "Edited"
		if len(in.Changes) > 0 {
			// Codex: a patch over one or more files.
			for _, c := range in.Changes {
				v.Files = append(v.Files, codexDiff(ctx, c))
				v.Paths = append(v.Paths, c.Path)
			}
			if len(in.Changes) == 1 {
				switch in.Changes[0].Kind {
				case "add":
					v.Verb, v.Icon = "Created", "file-plus"
				case "delete":
					v.Verb = "Deleted"
				}
			}
		} else {
			p := firstNonEmpty(in.FilePath, in.NotebookPth)
			f := fileDiff{Path: relPath(ctx, p)}
			switch name {
			case "Write":
				v.Verb, v.Icon = "Wrote", "file-plus"
				f.Lines = textdiff.Lines("", in.Content)
			case "NotebookEdit":
				f.Lines = textdiff.Lines("", in.NewSource)
			case "MultiEdit":
				for i, e := range in.Edits {
					if i > 0 {
						f.Lines = append(f.Lines, diffLine{Op: '@', Text: "…"})
					}
					f.Lines = append(f.Lines, textdiff.Lines(e.OldString, e.NewString)...)
				}
			default:
				f.Lines = textdiff.Lines(in.OldString, in.NewString)
			}
			f.count()
			v.Files = []fileDiff{f}
			v.Paths = []string{p}
		}
		switch len(v.Files) {
		case 0:
			v.Code = relPath(ctx, m.Summary)
		case 1:
			v.Code = v.Files[0].Path
		default:
			v.Code = fmt.Sprintf("%d files", len(v.Files))
		}
		add, del := 0, 0
		for i := range v.Files {
			v.Files[i].clip(400)
			add += v.Files[i].Add
			del += v.Files[i].Del
		}
		v.Note = diffStat(add, del)
	case name == "Grep" || name == "Glob":
		v.Kind, v.Icon, v.Text = toolSearch, "search", ""
		v.Verb = "Searched for"
		if name == "Glob" {
			v.Verb = "Listed"
		}
		v.Code = in.Pattern
		where := relPath(ctx, in.Path)
		if in.Glob != "" {
			where = strings.TrimPrefix(where+" "+in.Glob, " ")
		}
		if where != "" && where != "." {
			v.Note = "in " + where
		}
		if it.Status == domain.ItemDone {
			if n := resultCount(it.Output); n >= 0 {
				v.Note = strings.TrimPrefix(v.Note+" · "+strconv.Itoa(n)+" "+plural(n, "result", "results"), " · ")
			}
		}
	case name == "WebFetch":
		v.Kind, v.Icon, v.Verb, v.Text = toolWeb, "globe", "Fetched", ""
		v.Code = shortURL(in.URL)
	case name == "WebSearch":
		v.Kind, v.Icon, v.Verb, v.Text = toolWeb, "globe", "Searched the web", ""
		if q := firstNonEmpty(in.Query, m.Summary); q != "" {
			v.Verb = "Searched the web for"
			v.Text = q
		}
	case name == "Task" || name == "Agent":
		v.Kind, v.Icon = toolAgent, "bot"
	case name == "TodoWrite":
		v.Kind, v.Icon, v.Verb, v.Text = toolPlan, "list-checks", "Updated the plan", ""
		v.Todos = in.Todos
		done := 0
		for _, t := range in.Todos {
			if t.Status == "completed" {
				done++
			}
		}
		if len(in.Todos) > 0 {
			v.Note = fmt.Sprintf("%d of %d done", done, len(in.Todos))
		}
	case starcodeTool(name) != "":
		v = viewStarcodeTool(starcodeTool(name), m.Input, v)
	case strings.HasPrefix(name, "mcp__"):
		parts := strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)
		v.Verb = strings.Join(parts, " · ")
	}
	if v.Kind == toolOther && v.Text == v.Verb {
		v.Text = ""
	}
	return v
}

// starcodeTool is the name of one of starcode's own tools (internal/mcp)
// as Claude (mcp__starcode__x) or Codex (starcode/x) reports it, empty
// for any other.
func starcodeTool(name string) string {
	if t, ok := strings.CutPrefix(name, "mcp__starcode__"); ok {
		return t
	}
	if t, ok := strings.CutPrefix(name, "starcode/"); ok {
		return t
	}
	return ""
}

// viewStarcodeTool says in words what the agent did to starcode.
func viewStarcodeTool(tool string, input json.RawMessage, v toolView) toolView {
	var a struct {
		Title, PR, Prompt, ID, Message, Query, Until, Cron string
		Minutes                                            int
		WakePrompt                                         string          `json:"wake_prompt"`
		On                                                 *bool           `json:"on"`
		Arguments                                          json.RawMessage `json:"arguments"`
	}
	json.Unmarshal(input, &a)
	// Codex hands over its whole item, with the call's arguments inside.
	if len(a.Arguments) > 0 {
		json.Unmarshal(a.Arguments, &a)
	}
	v.Text, v.Code, v.Note = "", "", ""
	switch tool {
	case "thread_info":
		v.Icon, v.Verb = "message-square", "Looked up its thread"
	case "set_title":
		v.Icon, v.Verb, v.Text = "pencil", "Renamed the thread", a.Title
	case "link_pr":
		v.Icon, v.Verb, v.Code = "git-pull-request", "Linked", a.PR
	case "unlink_pr":
		v.Icon, v.Verb = "unlink", "Unlinked the pull request"
	case "watch_pr":
		v.Icon, v.Verb = "eye", "Watching the pull request"
		if a.On != nil && !*a.On {
			v.Icon, v.Verb = "eye-off", "Stopped watching the pull request"
		}
	case "snooze":
		v.Icon, v.Verb = "alarm-clock", "Snoozed the thread"
		if a.Minutes > 0 {
			v.Note = fmt.Sprintf("for %d min", a.Minutes)
		}
		v.Text = a.WakePrompt
	case "notify":
		v.Icon, v.Verb, v.Text = "triangle-alert", "Asked for your attention", a.Message
	case "list_threads":
		v.Icon, v.Verb, v.Text = "list-filter", "Listed threads", a.Query
	case "read_thread":
		v.Icon, v.Verb, v.Code = "file-text", "Read thread", a.ID
	case "spawn_thread":
		v.Icon, v.Verb, v.Text = "git-fork", "Started a thread", firstNonEmpty(a.Title, firstLine(a.Prompt))
	case "send_to_thread":
		v.Icon, v.Verb, v.Code = "message-square-plus", "Messaged thread", a.ID
	case "wait_thread":
		v.Icon, v.Verb, v.Code = "history", "Waited for thread", a.ID
	case "schedule":
		v.Icon, v.Verb, v.Text = "calendar-clock", "Scheduled", firstLine(a.Prompt)
		if a.Cron != "" {
			v.Note = a.Cron
		} else if a.Minutes > 0 {
			v.Note = fmt.Sprintf("in %d min", a.Minutes)
		}
	case "list_schedules":
		v.Icon, v.Verb = "calendar-clock", "Listed its schedules"
	case "cancel_schedule":
		v.Icon, v.Verb, v.Code = "x", "Cancelled schedule", a.ID
	case "stop_thread":
		v.Icon, v.Verb, v.Code = "square", "Stopped thread", a.ID
	default:
		v.Verb = "starcode · " + tool
	}
	return v
}

// shellWrap matches a command wrapped for a shell: bash -lc '...'.
var shellWrap = regexp.MustCompile(`^(?:/\S+/)?(?:ba|z)?sh\s+-l?c\s+(.+)$`)

// unwrapShell is the command inside a `bash -lc '...'` wrapper (codex
// reports its commands so), or the command as it is.
func unwrapShell(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	m := shellWrap.FindStringSubmatch(cmd)
	if m == nil {
		return cmd
	}
	inner := strings.TrimSpace(m[1])
	if len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[len(inner)-1] == inner[0] {
		inner = inner[1 : len(inner)-1]
		if cmd[len(cmd)-1] == '\'' {
			inner = strings.ReplaceAll(inner, `'\''`, `'`)
		}
	}
	return inner
}

// shellPrelude is a line a script starts with that says nothing about
// what it does.
var shellPrelude = regexp.MustCompile(`^(set\s+[-+][a-z]+(\s+\w+)*|#.*)$`)

// commandLine is the line that names a command: its first, past any
// set -e and comments, with "…" when more follow.
func commandLine(cmd string) string {
	lines := strings.Split(strings.TrimSpace(cmd), "\n")
	for len(lines) > 1 && shellPrelude.MatchString(strings.TrimSpace(lines[0])) {
		lines = lines[1:]
	}
	line := binPath.ReplaceAllString(strings.TrimSpace(lines[0]), "$1")
	if len(lines) > 1 {
		line += " …"
	}
	return line
}

// binPath matches a program named by its full path, /usr/local/go/bin/go,
// at the start of a line or after a pipe or &&: the row shows the name.
var binPath = regexp.MustCompile(`(?:/[\w.+-]+)+/bin/([\w.+-]+)`)

// cwdReset matches the note Claude's shell tool appends when a command
// changed directory; it says nothing about the command.
var cwdReset = regexp.MustCompile(`\n*Shell cwd was reset to [^\n]*\s*$`)

// commandOutput is a command's output without that note.
func commandOutput(out string) string {
	return cwdReset.ReplaceAllString(out, "")
}

// shortURL drops the scheme and a trailing slash.
func shortURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	out := u.Host + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return strings.TrimSuffix(out, "/")
}

// resultCount is how many lines a search printed, -1 when it said
// nothing countable.
func resultCount(out string) int {
	out = strings.TrimSpace(out)
	if out == "" {
		return 0
	}
	if strings.HasPrefix(out, "No files found") || strings.HasPrefix(out, "No matches found") {
		return 0
	}
	if strings.HasPrefix(out, "Found ") {
		// Grep's files mode: "Found 3 files\na\nb\nc".
		out = strings.TrimSpace(out[strings.IndexByte(out+"\n", '\n'):])
		if out == "" {
			return -1
		}
	}
	return strings.Count(out, "\n") + 1
}

func diffStat(add, del int) string {
	switch {
	case add == 0 && del == 0:
		return ""
	case del == 0:
		return fmt.Sprintf("+%d", add)
	case add == 0:
		return fmt.Sprintf("−%d", del)
	}
	return fmt.Sprintf("+%d −%d", add, del)
}

func (f *fileDiff) count() {
	f.Add, f.Del = textdiff.Count(f.Lines)
}

// clip keeps the first n lines of a long diff.
func (f *fileDiff) clip(n int) {
	if len(f.Lines) > n {
		f.Clipped = len(f.Lines) - n
		f.Lines = f.Lines[:n]
	}
}

// codexDiff reads one file of a codex patch. An added file's diff is its
// content, other kinds carry unified hunks.
func codexDiff(ctx context.Context, c codexChange) fileDiff {
	f := fileDiff{Path: relPath(ctx, c.Path)}
	if c.Kind == "add" && !strings.HasPrefix(c.Diff, "@@") {
		f.Lines = textdiff.Lines("", c.Diff)
	} else {
		f.Lines = textdiff.Hunks(c.Diff)
	}
	f.count()
	return f
}

// toolDiffClass is the CSS class of a line in a tool call's diff.
func toolDiffClass(op byte) string {
	switch op {
	case '+':
		return "dl add"
	case '-':
		return "dl del"
	case '@':
		return "dl hunk"
	}
	return "dl"
}

// diffMark is the gutter mark of a diff line.
func diffMark(op byte) string {
	switch op {
	case '+', '-':
		return string(op)
	}
	return " "
}

// todoIcon is a plan step's icon by status.
func todoIcon(status string) string {
	switch status {
	case "completed":
		return "circle-check"
	case "in_progress":
		return "circle-dot"
	}
	return "circle"
}

// workSummary sums up a run of work in words, for its header: "ran 3
// commands, read 2 files, edited 1 file".
func workSummary(ctx context.Context, items []store.Item) string {
	counts := map[string]int{}
	files := map[string]map[string]bool{toolRead: {}, toolEdit: {}}
	web, searchWeb := 0, 0
	for _, it := range items {
		if it.Kind != domain.KindTool || ParentOf(it) != "" {
			continue
		}
		v := viewTool(ctx, it)
		counts[v.Kind]++
		if f := files[v.Kind]; f != nil {
			for _, p := range v.Paths {
				f[p] = true
			}
			if len(v.Paths) == 0 {
				f[it.ID] = true
			}
		}
		if v.Kind == toolWeb {
			if it.ToolName == "WebSearch" {
				searchWeb++
			} else {
				web++
			}
		}
	}
	var parts []string
	add := func(n int, one, many string) {
		switch {
		case n == 1 && !strings.Contains(one, "%d"):
			parts = append(parts, one)
		case n > 0:
			parts = append(parts, fmt.Sprintf(plural(n, one, many), n))
		}
	}
	add(counts[toolCommand], "ran %d command", "ran %d commands")
	add(len(files[toolRead]), "read %d file", "read %d files")
	add(len(files[toolEdit]), "edited %d file", "edited %d files")
	add(counts[toolSearch], "searched once", "searched %d times")
	add(searchWeb, "searched the web once", "searched the web %d times")
	add(web, "fetched %d page", "fetched %d pages")
	add(counts[toolAgent], "ran %d agent", "ran %d agents")
	if counts[toolPlan] > 0 {
		parts = append(parts, "updated the plan")
	}
	add(counts[toolOther], "used %d other tool", "used %d other tools")
	return strings.Join(parts, ", ")
}

// liveStep is what a running work block is doing now, for its header:
// the newest top-level step that is not done, or else the newest step.
func liveStep(ctx context.Context, items []store.Item) (icon, text string) {
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if ParentOf(it) != "" {
			continue
		}
		switch it.Kind {
		case domain.KindThinking:
			// The newest line of the thought, as it streams: what the
			// agent is weighing says more than "Thinking".
			if line := lastLine(it.Body); line != "" {
				return "brain", line
			}
			return "brain", "Thinking"
		case domain.KindTool:
			v := viewTool(ctx, it)
			if v.Kind == toolAgent {
				in := agentInputOf(meta(it))
				return v.Icon, strings.TrimSpace("Agent: " + firstNonEmpty(in.Description, meta(it).Summary))
			}
			var words []string
			for _, w := range []string{v.Verb, v.Text, v.Code} {
				if w != "" {
					words = append(words, w)
				}
			}
			return v.Icon, strings.Join(words, " ")
		}
	}
	return "", ""
}

// lastLine is the last line of s with text on it, without markdown
// emphasis markers, clipped for a one-line header.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.Trim(strings.TrimSpace(lines[i]), "*_#> "); l != "" {
			if r := []rune(l); len(r) > 300 {
				l = string(r[:300])
			}
			return l
		}
	}
	return ""
}

// fileStat is one file of a turn's changes (app.TurnFiles).
type fileStat struct {
	Path string `json:"path"`
	Add  int    `json:"add"`
	Del  int    `json:"del"`
}

// turnFiles is what a turn changed, from its result line's meta.
type turnFiles struct {
	Files []fileStat `json:"files"`
}

func turnFilesOf(it store.Item) turnFiles {
	var tf turnFiles
	json.Unmarshal(it.Meta, &tf)
	return tf
}

func (tf turnFiles) total() (add, del int) {
	for _, f := range tf.Files {
		add += f.Add
		del += f.Del
	}
	return add, del
}
