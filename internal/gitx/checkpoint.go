package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Checkpoints: before a prompt goes out in a worktree, starcode records
// the checkout as a commit, so a rewind to that prompt can put the files
// back. The commit holds the working tree as it is (tracked changes and
// untracked files, ignored ones left out) with HEAD as its parent, and is
// written through a scratch index, so the branch, the real index and the
// files are not touched. A ref under refs/starcode/checkpoints/<thread>
// keeps it from garbage collection until the thread goes.

// checkpointTimeout bounds one checkpoint or restore; git add on a big
// tree takes longer than the 10s the readers get.
const checkpointTimeout = 2 * time.Minute

func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkpointTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], firstLine(msg))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return out.String(), nil
}

// checkpointRef is the ref prefix of thread's checkpoints.
func checkpointRef(thread string) string {
	return "refs/starcode/checkpoints/" + thread
}

// Checkpoint records dir's working tree and returns the commit. thread
// names the ref that keeps it.
func Checkpoint(ctx context.Context, dir, thread string) (string, error) {
	head, err := gitEnv(ctx, dir, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	head = strings.TrimSpace(head)
	tmp, err := os.CreateTemp("", "starcode-index-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	env := []string{"GIT_INDEX_FILE=" + tmp.Name()}
	// A copy of the checkout's own index keeps git's stat data, so add
	// only hashes the files that changed; an index read from HEAD's tree
	// has none and would hash every file. Without one, HEAD's tree it is.
	copied := false
	if p, err := gitEnv(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"); err == nil {
		if src, err := os.Open(strings.TrimSpace(p)); err == nil {
			_, err = io.Copy(tmp, src)
			src.Close()
			copied = err == nil
		}
	}
	tmp.Close()
	if !copied {
		if _, err := gitEnv(ctx, dir, env, "read-tree", head); err != nil {
			return "", err
		}
	}
	if _, err := gitEnv(ctx, dir, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	tree, err := gitEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	sha, err := gitEnv(ctx, dir, []string{
		"GIT_AUTHOR_NAME=starcode", "GIT_AUTHOR_EMAIL=starcode@localhost",
		"GIT_COMMITTER_NAME=starcode", "GIT_COMMITTER_EMAIL=starcode@localhost",
	}, "commit-tree", strings.TrimSpace(tree), "-p", head, "-m", "starcode checkpoint")
	if err != nil {
		return "", err
	}
	sha = strings.TrimSpace(sha)
	if _, err := gitEnv(ctx, dir, nil, "update-ref", checkpointRef(thread)+"/"+sha, sha); err != nil {
		return "", err
	}
	return sha, nil
}

// RestoreCheckpoint puts dir back the way Checkpoint found it: the branch
// back on the commit it was on (later commits stay in the reflog), the
// files as they were, the ones added since removed, and what was
// untracked untracked again. Ignored files are left alone.
func RestoreCheckpoint(ctx context.Context, dir, sha string) error {
	parent, err := gitEnv(ctx, dir, nil, "rev-parse", "--verify", sha+"^")
	if err != nil {
		return err
	}
	parent = strings.TrimSpace(parent)
	if _, err := gitEnv(ctx, dir, nil, "reset", "--quiet", "--hard", parent); err != nil {
		return err
	}
	if _, err := gitEnv(ctx, dir, nil, "clean", "-fdq"); err != nil {
		return err
	}
	if _, err := gitEnv(ctx, dir, nil, "checkout", sha, "--", "."); err != nil {
		return err
	}
	// Files the checkpoint had already deleted came back with the reset.
	gone, err := gitEnv(ctx, dir, nil, "diff", "--name-only", "-z", "--diff-filter=D", parent, sha)
	if err != nil {
		return err
	}
	for _, p := range strings.Split(gone, "\x00") {
		if p != "" {
			os.Remove(filepath.Join(dir, filepath.FromSlash(p)))
		}
	}
	// The checkout staged everything; unstage it, so files that were
	// untracked read as untracked again.
	_, err = gitEnv(ctx, dir, nil, "reset", "--quiet")
	return err
}

// DropCheckpoints deletes thread's checkpoint refs in repo, so git can
// collect the commits.
func DropCheckpoints(ctx context.Context, repo, thread string) {
	out, err := gitEnv(ctx, repo, nil, "for-each-ref", "--format=%(refname)", checkpointRef(thread))
	if err != nil {
		return
	}
	for _, ref := range strings.Fields(out) {
		gitEnv(ctx, repo, nil, "update-ref", "-d", ref)
	}
}

// ErrKeepWorktree is why Cleanable refuses a checkout.
var ErrKeepWorktree = errors.New("worktree has work in it")

// Cleanable is nil when dir can go without losing anything: HEAD on a
// branch, no changed or untracked files, and no ignored ones other than
// node_modules, which a setup script can put back. A .env the setup
// linked in counts as work, since it may be the only copy.
func Cleanable(ctx context.Context, dir string) error {
	// Commits made on a detached HEAD are only held by the worktree's
	// own reflog, which goes with it.
	if _, err := run(ctx, dir, "symbolic-ref", "-q", "HEAD"); err != nil {
		return fmt.Errorf("%w: HEAD is not on a branch", ErrKeepWorktree)
	}
	if WorktreeChanges(ctx, dir) > 0 {
		return fmt.Errorf("%w: uncommitted or untracked files", ErrKeepWorktree)
	}
	out, err := run(ctx, dir, "status", "--porcelain", "--ignored", "-z")
	if err != nil {
		return err
	}
	for _, rec := range strings.Split(out, "\x00") {
		if !strings.HasPrefix(rec, "!! ") {
			continue
		}
		p := strings.TrimSuffix(rec[3:], "/")
		if p == "node_modules" || strings.HasPrefix(p, "node_modules/") || strings.HasSuffix(p, "/node_modules") || strings.Contains(p, "/node_modules/") {
			continue
		}
		return fmt.Errorf("%w: ignored file %s", ErrKeepWorktree, p)
	}
	return nil
}

// Contained is whether every commit on dir's HEAD is in branch (origin/main,
// say), fetched first when it is a remote branch. An error reads as no.
func Contained(ctx context.Context, dir, branch string) bool {
	if strings.HasPrefix(branch, "origin/") {
		run(ctx, dir, "fetch", "--quiet", "origin", strings.TrimPrefix(branch, "origin/"))
	}
	_, err := run(ctx, dir, "merge-base", "--is-ancestor", "HEAD", branch)
	return err == nil
}
