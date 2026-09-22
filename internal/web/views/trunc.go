package views

import (
	"os"

	"starcode/internal/store"
)

// MidTrunc shortens s to about n characters by cutting its middle, so a
// branch like starcode/fix-the-thing-that-broke keeps both its prefix
// and its end, which is where two names usually differ. Callers put the
// whole name in a title.
func MidTrunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 5 {
		return s
	}
	head := (n - 1) / 2
	tail := n - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// worktreeOnDisk is whether the thread's worktree directory exists; the
// automatic cleanup removes it and the next prompt puts it back.
func worktreeOnDisk(t store.Thread) bool {
	if t.Worktree == "" {
		return false
	}
	_, err := os.Stat(t.Worktree)
	return err == nil
}
