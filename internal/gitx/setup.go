package gitx

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SetupScript is the command a project wants run in every new worktree
// (dependencies, a symlinked .env). It is read from t3.json at the
// project root, the file T3 Code uses, so a project set up for either
// console works in both: the first script with runOnWorktreeCreate.
type SetupScript struct {
	Name    string
	Command string
	// Async lets the agent start while the script runs; false holds the
	// first turn until it ends. T3's default is true.
	Async bool
}

// ReadSetupScript returns the project's worktree setup script, or false
// when t3.json is missing or names none.
func ReadSetupScript(root string) (SetupScript, bool) {
	raw, err := os.ReadFile(filepath.Join(root, "t3.json"))
	if err != nil {
		return SetupScript{}, false
	}
	var f struct {
		Scripts []struct {
			Name                string `json:"name"`
			Command             string `json:"command"`
			RunOnWorktreeCreate bool   `json:"runOnWorktreeCreate"`
			Async               *bool  `json:"async"`
		} `json:"scripts"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return SetupScript{}, false
	}
	for _, s := range f.Scripts {
		if !s.RunOnWorktreeCreate || strings.TrimSpace(s.Command) == "" {
			continue
		}
		return SetupScript{Name: s.Name, Command: s.Command, Async: s.Async == nil || *s.Async}, true
	}
	return SetupScript{}, false
}

// RunSetup runs the script in the worktree through the user's shell,
// with T3CODE_PROJECT_ROOT and T3CODE_WORKTREE_PATH set as T3 Code sets
// them, and returns the last lines of what it printed and the error.
func RunSetup(ctx context.Context, s SetupScript, root, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", s.Command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "T3CODE_PROJECT_ROOT="+root, "T3CODE_WORKTREE_PATH="+dir, "NO_COLOR=1", "FORCE_COLOR=0")
	out, err := cmd.CombinedOutput()
	return Tail(string(out), 12), err
}

// Tail keeps the last n non-empty lines of s.
func Tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, strings.TrimRight(l, " \t\r"))
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}
