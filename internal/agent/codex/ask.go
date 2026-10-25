package codex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Ask runs prompt through `codex exec` at low reasoning effort, read-only
// and ephemeral, in an empty folder so no project instructions load. The
// answer is the last message, which codex writes to a file.
func (a *Agent) Ask(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "starcode-ask-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	last := filepath.Join(dir, "answer.txt")
	cmd := exec.CommandContext(ctx, a.binary, "exec", "--ephemeral", "--skip-git-repo-check", "-s", "read-only",
		"-c", `model_reasoning_effort="low"`, "-o", last, "-")
	cmd.Dir = dir
	cmd.Env = childEnv(a.env)
	cmd.Stdin = strings.NewReader(prompt)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(errb.String()), "\n")
		return "", fmt.Errorf("codex: %w: %s", err, strings.TrimSpace(lines[len(lines)-1]))
	}
	out, err := os.ReadFile(last)
	if err != nil {
		return "", fmt.Errorf("codex: no answer: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
