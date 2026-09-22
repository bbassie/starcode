package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
)

// CleanWorktrees removes worktrees that are done with, on the rules of
// the wt_clean_days and wt_clean_merged settings (both off by default):
// a thread quiet for that many days, or one whose work has landed (its
// pull request merged, or a settled thread whose commits are all in the
// default branch). Only checkouts starcode made under WorktreeRoot go,
// and only when nothing would be lost (gitx.Cleanable), no turn runs in
// them, no other thread shares them and the project has not turned the
// cleanup off. The thread keeps its branch and its worktree path, and the
// next prompt checks the branch out there again (see session).
func (a *App) CleanWorktrees(ctx context.Context) {
	days := a.Store.WorktreeCleanDays(ctx)
	merged := a.Store.WorktreeCleanMerged(ctx)
	if (days <= 0 && !merged) || a.WorktreeRoot == "" {
		return
	}
	ts, err := a.Store.Threads(ctx)
	if err != nil {
		a.Log.Warn("clean worktrees", "err", err)
		return
	}
	prs, err := a.Store.PRStates(ctx)
	if err != nil {
		a.Log.Warn("clean worktrees", "err", err)
		return
	}
	projects := map[string]store.Project{}
	if ps, err := a.Store.Projects(ctx); err == nil {
		for _, p := range ps {
			projects[p.ID] = p
		}
	}
	shared := map[string]int{}
	for _, t := range ts {
		if t.Worktree != "" {
			shared[t.Worktree]++
		}
	}
	defaults := map[string]string{}
	now := time.Now()
	for _, t := range ts {
		p, ok := projects[t.ProjectID]
		if !ok || p.Cleanup == "off" || t.Worktree == "" || shared[t.Worktree] > 1 || !a.ownWorktree(t.Worktree) {
			continue
		}
		if t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
			continue
		}
		if a.KeepWorktree != nil && a.KeepWorktree(t.Worktree) {
			continue
		}
		if !exists(t.Worktree) {
			continue
		}
		reason := ""
		if days > 0 && now.Sub(t.QuietSince()) >= time.Duration(days)*24*time.Hour {
			reason = fmt.Sprintf("after %d %s without activity", days, plural(days, "day", "days"))
		}
		if reason == "" && merged {
			if ref, ok := t.Linked(); ok && prs[ref].State == "merged" {
				reason = fmt.Sprintf("now that pull request #%d is merged", ref.Number)
			} else if t.Archived {
				def, ok := defaults[p.ID]
				if !ok {
					def = gitx.DefaultBranch(ctx, p.Path)
					defaults[p.ID] = def
				}
				if def != "HEAD" && gitx.Contained(ctx, t.Worktree, def) {
					reason = "now that its commits are all in " + def
				}
			}
		}
		if reason == "" {
			continue
		}
		if err := gitx.Cleanable(ctx, t.Worktree); err != nil {
			a.Log.Debug("worktree kept", "thread", t.ID, "dir", t.Worktree, "err", err)
			continue
		}
		// promptMu keeps a prompt from starting a session in the checkout
		// while it goes; the status is read again under it.
		a.promptMu.Lock()
		if cur, err := a.Store.Thread(ctx, t.ID); err != nil || cur.Status == domain.StatusRunning || cur.Status == domain.StatusAwaitingApproval {
			a.promptMu.Unlock()
			continue
		}
		a.closeSession(t.ID)
		err := gitx.RemoveWorktree(ctx, p.Path, t.Worktree, false)
		a.promptMu.Unlock()
		if err != nil {
			a.Log.Info("worktree cleanup", "thread", t.ID, "dir", t.Worktree, "err", err)
			continue
		}
		// No note in the transcript: a line there is activity, which
		// would bring a settled thread back to the top as unread. The
		// header says the checkout is gone (views.MainHead).
		a.Log.Info("removed worktree", "thread", t.ID, "dir", t.Worktree, "branch", t.WorktreeBranch, "why", reason)
	}
}

// ownWorktree is whether dir is one of the checkouts starcode made.
func (a *App) ownWorktree(dir string) bool {
	rel, err := filepath.Rel(a.WorktreeRoot, dir)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
