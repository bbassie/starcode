package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// worktreeRepo makes a repository with one commit and a worktree of it
// on a branch "wt", and returns both directories.
func worktreeRepo(t *testing.T) (repo, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	dir = filepath.Join(root, "wt")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	for _, args := range [][]string{
		{"init", "--quiet", "-b", "main"},
		{"add", "a.txt"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "first"},
		{"worktree", "add", "--quiet", "-b", "wt", dir, "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo, dir
}

func TestRemoveWorktreeClean(t *testing.T) {
	ctx := context.Background()
	repo, dir := worktreeRepo(t)
	if n := WorktreeChanges(ctx, dir); n != 0 {
		t.Fatalf("WorktreeChanges on a clean worktree = %d", n)
	}
	if err := RemoveWorktree(ctx, repo, dir, false); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree still on disk: %v", err)
	}
	if _, err := run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/wt"); err != nil {
		t.Errorf("branch wt is gone: %v", err)
	}
}

func TestRemoveWorktreeDirty(t *testing.T) {
	ctx := context.Background()
	repo, dir := worktreeRepo(t)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644)
	if n := WorktreeChanges(ctx, dir); n != 2 {
		t.Fatalf("WorktreeChanges = %d, want 2", n)
	}
	if err := RemoveWorktree(ctx, repo, dir, false); err == nil {
		t.Fatal("RemoveWorktree without force removed a dirty worktree")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatalf("refused removal lost a file: %v", err)
	}
	if err := RemoveWorktree(ctx, repo, dir, true); err != nil {
		t.Fatalf("RemoveWorktree with force: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree still on disk: %v", err)
	}
	if _, err := run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/wt"); err != nil {
		t.Errorf("branch wt is gone: %v", err)
	}
}

func TestWorktreeChangesNotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if n := WorktreeChanges(context.Background(), t.TempDir()); n != 0 {
		t.Errorf("WorktreeChanges outside a repository = %d", n)
	}
}
