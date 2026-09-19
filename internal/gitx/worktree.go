package gitx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Worktrees: a thread can work in a checkout of its own, so two agents on
// one project do not trample each other's working tree. The worktree is
// `git worktree add` on a new branch from the repository's default
// branch, under a directory starcode owns; the repository itself keeps
// the main checkout.

// DefaultBranch is the branch new worktrees start from: the remote's
// HEAD when the repository has one, else the branch checked out now.
func DefaultBranch(ctx context.Context, repo string) string {
	if out, err := run(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "" {
			return b // origin/main
		}
	}
	if out, err := run(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "" && b != "HEAD" {
			return b
		}
	}
	return "HEAD"
}

// AddWorktree makes dir a checkout of repo on a new branch from base.
// With a remote base (origin/main) it fetches first, so the branch starts
// from what is on GitHub rather than a stale local copy. The branch
// name gets a number when it is taken.
func AddWorktree(ctx context.Context, repo, dir, branch, base string) (string, error) {
	if strings.HasPrefix(base, "origin/") {
		run(ctx, repo, "fetch", "--quiet", "origin", strings.TrimPrefix(base, "origin/"))
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	name := branch
	for i := 2; i < 100; i++ {
		if _, err := run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+name); err != nil {
			break
		}
		name = fmt.Sprintf("%s-%d", branch, i)
	}
	if _, err := run(ctx, repo, "worktree", "add", "--quiet", "-b", name, dir, base); err != nil {
		return "", fmt.Errorf("git worktree add: %w", err)
	}
	return name, nil
}

// WorktreeChanges counts the entries `git status --porcelain` lists in
// dir: modified, staged and untracked files. It is 0 for a clean
// checkout and for a directory that is not a repository.
func WorktreeChanges(ctx context.Context, dir string) int {
	out, err := run(ctx, dir, "status", "--porcelain", "-z")
	if err != nil {
		return 0
	}
	n := 0
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if len(rec) < 4 {
			continue
		}
		n++
		if rec[0] == 'R' || rec[0] == 'C' {
			i++ // with -z a rename or copy puts its old path in the next record
		}
	}
	return n
}

// RemoveWorktree drops dir from repo. Without force it refuses a
// worktree that holds changes, since removing it would lose work. With
// force it runs `git worktree remove --force`, which deletes modified
// and untracked files too. The branch is kept either way: it may be
// pushed, or merged later.
func RemoveWorktree(ctx context.Context, repo, dir string, force bool) error {
	args := []string{"worktree", "remove", dir}
	if force {
		args = []string{"worktree", "remove", "--force", dir}
	} else if WorktreeChanges(ctx, dir) > 0 {
		return fmt.Errorf("worktree has uncommitted changes")
	}
	if _, err := run(ctx, repo, args...); err != nil {
		return err
	}
	run(ctx, repo, "worktree", "prune")
	return nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a title into a branch and directory name: lower case, words
// joined with dashes, at most 40 characters, "thread" when nothing is left.
func Slug(title string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		return "thread"
	}
	return s
}

// RenameBranch renames the worktree's branch: the thread starts on a
// throwaway name and takes one from its title once it has one.
func RenameBranch(ctx context.Context, dir, from, to string) (string, error) {
	name := to
	for i := 2; i < 100; i++ {
		if _, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+name); err != nil {
			break
		}
		name = fmt.Sprintf("%s-%d", to, i)
	}
	if _, err := run(ctx, dir, "branch", "-m", from, name); err != nil {
		return "", err
	}
	return name, nil
}

// EnsureWorktree puts a worktree back that was deleted outside starcode:
// the agent's session resumes into its directory, so a missing one would
// fail every later turn. The stale admin entry is pruned first, then the
// same branch is checked out at the same path.
func EnsureWorktree(ctx context.Context, repo, dir, branch string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	run(ctx, repo, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if _, err := run(ctx, repo, "worktree", "add", "--quiet", dir, branch); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	return nil
}

// Branches lists what a new worktree can start from: the local branches,
// then the remote ones (origin/HEAD left out, it is an alias), with the
// default branch first. A repository without branches gives nil.
func Branches(ctx context.Context, repo string) []string {
	def := DefaultBranch(ctx, repo)
	var out []string
	seen := map[string]bool{}
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b == "" || b == "origin/HEAD" || seen[b] {
			return
		}
		seen[b] = true
		out = append(out, b)
	}
	if def != "HEAD" {
		add(def)
	}
	for _, args := range [][]string{
		{"branch", "--format=%(refname:short)"},
		{"branch", "-r", "--format=%(refname:short)"},
	} {
		lines, err := run(ctx, repo, args...)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(lines, "\n") {
			add(l)
		}
	}
	return out
}
