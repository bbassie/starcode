package views

import (
	"context"
	"encoding/json"
	"testing"

	"starcode/internal/domain"
	"starcode/internal/store"
)

func toolItem(name string, input any, output string) store.Item {
	in, _ := json.Marshal(input)
	m, _ := json.Marshal(map[string]any{"input": json.RawMessage(in), "summary": "sum"})
	return store.Item{ID: name, Kind: domain.KindTool, ToolName: name, Status: domain.ItemDone, Meta: m, Output: output}
}

func TestViewTool(t *testing.T) {
	ctx := WithDir(context.Background(), "/work/repo")
	cases := []struct {
		it                     store.Item
		kind, verb, text, code string
		note                   string
	}{
		{toolItem("Bash", map[string]any{"command": "go test ./...", "description": "Run the tests"}, ""), toolCommand, "", "Run the tests", "", ""},
		{toolItem("Bash", map[string]any{"command": "/bin/bash -lc 'cd /work/repo/x && ls'"}, ""), toolCommand, "", "", "cd x && ls", ""},
		{toolItem("Read", map[string]any{"file_path": "/work/repo/internal/a.go", "offset": 10, "limit": 5}, ""), toolRead, "Read", "", "internal/a.go", "lines 10–14"},
		{toolItem("Edit", map[string]any{"file_path": "/work/repo/a.go", "old_string": "a\nb\nc", "new_string": "a\nB\nc\nd"}, ""), toolEdit, "Edited", "", "a.go", "+2 −1"},
		{toolItem("Write", map[string]any{"file_path": "/elsewhere/n.txt", "content": "x\ny\n"}, ""), toolEdit, "Wrote", "", "/elsewhere/n.txt", "+2"},
		{toolItem("Grep", map[string]any{"pattern": "func main", "path": "/work/repo/cmd"}, "a.go\nb.go"), toolSearch, "Searched for", "", "func main", "in cmd · 2 results"},
		{toolItem("WebFetch", map[string]any{"url": "https://example.com/a/b/"}, ""), toolWeb, "Fetched", "", "example.com/a/b", ""},
		{toolItem("TodoWrite", map[string]any{"todos": []map[string]string{{"content": "a", "status": "completed"}, {"content": "b", "status": "pending"}}}, ""), toolPlan, "Updated the plan", "", "", "1 of 2 done"},
		{toolItem("mcp__github__create_issue", map[string]any{}, ""), toolOther, "github · create_issue", "sum", "", ""},
	}
	for _, c := range cases {
		v := viewTool(ctx, c.it)
		if v.Kind != c.kind || v.Verb != c.verb || v.Text != c.text || v.Code != c.code || v.Note != c.note {
			t.Errorf("%s: got %s|%q|%q|%q|%q, want %s|%q|%q|%q|%q", c.it.ToolName, v.Kind, v.Verb, v.Text, v.Code, v.Note, c.kind, c.verb, c.text, c.code, c.note)
		}
	}
}

func TestCodexEdit(t *testing.T) {
	ctx := WithDir(context.Background(), "/w")
	it := toolItem("Edit", map[string]any{"changes": []map[string]string{
		{"path": "/w/a.go", "kind": "update", "diff": "@@ -1,2 +1,2 @@\n ctx\n-old\n+new\n"},
		{"path": "/w/b.go", "kind": "add", "diff": "one\ntwo\n"},
	}}, "")
	v := viewTool(ctx, it)
	if v.Code != "2 files" || v.Note != "+3 −1" || len(v.Files) != 2 || v.Files[0].Path != "a.go" {
		t.Fatalf("codex edit = %+v", v)
	}
	if got := v.Files[0].Lines; len(got) != 4 || got[0].Op != '@' || got[2] != (diffLine{Op: '-', Text: "old"}) {
		t.Fatalf("hunk lines = %+v", got)
	}
}

func TestWorkSummary(t *testing.T) {
	ctx := context.Background()
	items := []store.Item{
		toolItem("Bash", map[string]any{"command": "ls"}, ""),
		toolItem("Bash", map[string]any{"command": "pwd"}, ""),
		toolItem("Read", map[string]any{"file_path": "/a"}, ""),
		toolItem("Read", map[string]any{"file_path": "/a", "offset": 5}, ""),
		toolItem("Edit", map[string]any{"file_path": "/b", "old_string": "x", "new_string": "y"}, ""),
		{Kind: domain.KindThinking, Body: "hm"},
	}
	if got := workSummary(ctx, items); got != "ran 2 commands, read 1 file, edited 1 file" {
		t.Fatalf("summary = %q", got)
	}
	items[len(items)-1].Status = domain.ItemRunning
	if icon, text := liveStep(ctx, items); icon != "brain" || text != "hm" {
		t.Fatalf("live = %s %q", icon, text)
	}
	items[len(items)-1].Body = "**Weighing the cache**\n\nThe probe runs every hour.\n"
	if _, text := liveStep(ctx, items); text != "The probe runs every hour." {
		t.Fatalf("live = %q", text)
	}
	items[len(items)-1].Body = ""
	if _, text := liveStep(ctx, items); text != "Thinking" {
		t.Fatalf("live = %q", text)
	}
	if icon, text := liveStep(ctx, items[:5]); icon != "square-pen" || text != "Edited /b" {
		t.Fatalf("live = %s %q", icon, text)
	}
}

func TestCommandLine(t *testing.T) {
	if got := commandLine("set -euo pipefail\n# build\ngo build ./...\ngo test"); got != "go build ./... …" {
		t.Errorf("commandLine = %q", got)
	}
	if got := commandLine("/usr/local/go/bin/go test ./... && /usr/bin/git status"); got != "go test ./... && git status" {
		t.Errorf("commandLine = %q", got)
	}
	if got := commandOutput("ok\nShell cwd was reset to /home/x\n"); got != "ok" {
		t.Errorf("commandOutput = %q", got)
	}
}

func TestUnwrapShell(t *testing.T) {
	for in, want := range map[string]string{
		`bash -lc 'echo '\''hi'\'''`: `echo 'hi'`,
		`/bin/zsh -c "make build"`:   `make build`,
		`git status`:                 `git status`,
	} {
		if got := unwrapShell(in); got != want {
			t.Errorf("unwrapShell(%q) = %q, want %q", in, got, want)
		}
	}
}
