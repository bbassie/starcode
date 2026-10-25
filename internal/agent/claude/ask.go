package claude

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// askModel is the model for one-off prompts: small jobs that want speed
// and a low price over depth.
const askModel = "haiku"

// Ask runs prompt through `claude -p` on the small model, with no tools,
// no MCP servers, no settings and no saved session, in an empty folder so
// no project instructions load either.
func (a *Agent) Ask(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "starcode-ask-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, a.binary, "-p", "--model", askModel, "--output-format", "text",
		"--tools", "", "--no-session-persistence", "--strict-mcp-config", "--setting-sources", "")
	cmd.Dir = dir
	cmd.Env = childEnv(a.env)
	cmd.Stdin = strings.NewReader(prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude: %w: %s", err, lastLine(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// lastLine is the last line of s with text on it.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
