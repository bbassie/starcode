package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// TestCheckpointRoundTrip changes a worktree in every way an agent can
// after a checkpoint (edit, delete, add, commit) and checks the restore
// puts back the files, the untracked state and the branch head.
func TestCheckpointRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo, dir := worktreeRepo(t)
	// The state to come back to: a.txt edited, b.txt tracked, u.txt
	// untracked, gone.txt deleted but not staged, and an ignored file.
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "gone.txt"), []byte("gone\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644)
	gitIn(t, dir, "add", "b.txt", "gone.txt", ".gitignore")
	gitIn(t, dir, "commit", "--quiet", "-m", "second")
	head := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a edited\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "u.txt"), []byte("untracked\n"), 0o644)
	os.Remove(filepath.Join(dir, "gone.txt"))
	os.WriteFile(filepath.Join(dir, "keep.log"), []byte("ignored\n"), 0o644)

	sha, err := Checkpoint(ctx, dir, "thread1")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if n := WorktreeChanges(ctx, dir); n != 3 {
		t.Fatalf("checkpoint touched the worktree: %d changes, want 3", n)
	}

	// The agent's turn: more edits, a new file, a deletion and a commit.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a by agent\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644)
	os.Remove(filepath.Join(dir, "b.txt"))
	os.WriteFile(filepath.Join(dir, "u.txt"), []byte("u by agent\n"), 0o644)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "agent")
	os.WriteFile(filepath.Join(dir, "later.txt"), []byte("later\n"), 0o644)

	if err := RestoreCheckpoint(ctx, dir, sha); err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	for name, want := range map[string]string{
		"a.txt":     "a edited\n",
		"b.txt":     "b\n",
		"u.txt":     "untracked\n",
		"gone.txt":  "<missing>",
		"new.txt":   "<missing>",
		"later.txt": "<missing>",
		"keep.log":  "ignored\n",
	} {
		if got := readFile(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD")); got != head {
		t.Errorf("HEAD = %s, want %s", got, head)
	}
	status := gitIn(t, dir, "status", "--porcelain")
	for _, want := range []string{" M a.txt", "?? u.txt", " D gone.txt"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}

	DropCheckpoints(ctx, repo, "thread1")
	if refs := gitIn(t, repo, "for-each-ref", "refs/starcode/"); strings.TrimSpace(refs) != "" {
		t.Errorf("refs left after DropCheckpoints: %s", refs)
	}
}

func TestCleanable(t *testing.T) {
	ctx := context.Background()
	_, dir := worktreeRepo(t)
	if err := Cleanable(ctx, dir); err != nil {
		t.Fatalf("clean worktree: %v", err)
	}
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n.env\n"), 0o644)
	gitIn(t, dir, "add", ".gitignore")
	gitIn(t, dir, "commit", "--quiet", "-m", "ignore")
	os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "x", "i.js"), []byte("x"), 0o644)
	if err := Cleanable(ctx, dir); err != nil {
		t.Fatalf("node_modules should not keep a worktree: %v", err)
	}
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o644)
	if err := Cleanable(ctx, dir); err == nil {
		t.Fatal("an ignored .env should keep the worktree")
	}
	os.Remove(filepath.Join(dir, ".env"))
	os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("wip\n"), 0o644)
	if err := Cleanable(ctx, dir); err == nil {
		t.Fatal("an untracked file should keep the worktree")
	}
	os.Remove(filepath.Join(dir, "wip.txt"))
	gitIn(t, dir, "checkout", "--quiet", "--detach")
	if err := Cleanable(ctx, dir); err == nil {
		t.Fatal("a detached HEAD should keep the worktree")
	}
}

func TestContained(t *testing.T) {
	ctx := context.Background()
	repo, dir := worktreeRepo(t)
	if !Contained(ctx, dir, "main") {
		t.Fatal("a fresh branch is contained in main")
	}
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644)
	gitIn(t, dir, "add", "c.txt")
	gitIn(t, dir, "commit", "--quiet", "-m", "wt work")
	if Contained(ctx, dir, "main") {
		t.Fatal("a commit only on wt is not in main")
	}
	gitIn(t, repo, "merge", "--quiet", "--ff-only", "wt")
	if !Contained(ctx, dir, "main") {
		t.Fatal("after the merge wt is in main")
	}
}
