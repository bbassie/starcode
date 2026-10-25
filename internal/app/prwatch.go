package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
)

// Watching a pull request: a thread linked to a PR can watch it, and the
// sweep reads the PR every minute. When a check fails, the checks all
// pass, someone else comments or reviews, or the branch starts to
// conflict, the thread's agent gets a prompt that says what changed. The
// watch starts from how the PR stands, so starting it wakes nobody.
// Watching ends when the PR merges or closes, when it is unlinked, after
// prWatchWakeLimit wakes in a row that brought only comments (a bot and
// the agent answering each other), or when the PR could not be read for
// prWatchReadGrace. The design follows T3 Code's watch_pull_request.

const (
	prWatchEvery     = time.Minute
	prWatchWakeLimit = 10
	prWatchReadGrace = 15 * time.Minute
	prWatchListed    = 10
	prWatchSnippet   = 200
)

// PRWatch is a watch's bookkeeping: what the agent has been told, so the
// next read reports only what is new. It is stored on the thread as JSON
// (thread.pr_watch_set).
type PRWatch struct {
	Started time.Time `json:"started"`
	// Head is the head commit the checks below belong to; a push starts
	// them over.
	Head   string   `json:"head,omitempty"`
	Failed []string `json:"failed,omitempty"`
	Passed bool     `json:"passed,omitempty"`
	// Through is the newest remark told; Seen the ids of the remarks at
	// that very second, since GitHub's times have no finer grain.
	Through     time.Time `json:"through"`
	Seen        []string  `json:"seen,omitempty"`
	Conflicting bool      `json:"conflicting,omitempty"`
	// Wakes counts wakes in a row that brought only comments.
	Wakes int `json:"wakes,omitempty"`
	// Unread is since when reads of the PR have failed; zero after a
	// good one.
	Unread time.Time `json:"unread,omitzero"`
}

// Watching is whether t watches its linked pull request.
func Watching(t store.Thread) bool { return t.PRWatch != "" && t.PRNumber > 0 }

// prChange is one thing the agent hears about.
type prChange struct {
	kind    string // "failed", "passed", "remarks", "conflict"
	checks  []gitx.Check
	count   int
	remarks []gitx.Remark
}

func failedChecks(cs []gitx.Check) []gitx.Check {
	var out []gitx.Check
	for _, c := range cs {
		if c.Status == "failure" {
			out = append(out, c)
		}
	}
	return out
}

// checksPassed is whether every check that ran passed, and how many ran.
// Skipped checks do not count either way.
func checksPassed(cs []gitx.Check) (bool, int) {
	n := 0
	for _, c := range cs {
		switch c.Status {
		case "skipped":
		case "success":
			n++
		default:
			return false, 0
		}
	}
	return n > 0, n
}

func checkNames(cs []gitx.Check) []string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	return names
}

// prWatchBaseline is a new watch on the PR as it stands: nothing in it
// is news.
func prWatchBaseline(d gitx.PRDetail, now time.Time) PRWatch {
	w := PRWatch{Started: now, Head: d.HeadSHA, Failed: checkNames(failedChecks(d.Checks)), Conflicting: d.Mergeable == "CONFLICTING", Through: now}
	w.Passed, _ = checksPassed(d.Checks)
	for _, r := range d.Remarks {
		if r.CreatedAt.After(w.Through) {
			w.Through = r.CreatedAt
		}
	}
	for _, r := range d.Remarks {
		if r.CreatedAt.Equal(w.Through) {
			w.Seen = append(w.Seen, r.ID)
		}
	}
	return w
}

// evaluatePRWatch compares the PR with what the agent was last told. A
// check is reported as soon as it fails, so one that never finishes
// cannot hold the news back; "passed" once every check that ran passed.
// Remarks count when someone other than the viewer (or, without one, the
// PR's author) wrote them, so the agent's own replies never wake it.
// exhausted is the last comment-only wake before the limit.
func evaluatePRWatch(w PRWatch, d gitx.PRDetail) (changes []prChange, next PRWatch, exhausted bool) {
	next = w
	next.Unread = time.Time{}
	headMoved := d.HeadSHA != w.Head
	failed, passed := w.Failed, w.Passed
	if headMoved {
		failed, passed = nil, false
	}
	if len(d.Checks) > 0 {
		// An empty list keeps the state: a read can come back without the
		// checks while they are created after a push.
		now := failedChecks(d.Checks)
		var fresh []gitx.Check
		for _, c := range now {
			if !slices.Contains(failed, c.Name) {
				fresh = append(fresh, c)
			}
		}
		if len(fresh) > 0 {
			changes = append(changes, prChange{kind: "failed", checks: fresh})
		}
		// A check that runs again leaves the list, so failing again is news.
		failed = checkNames(now)
		ok, n := checksPassed(d.Checks)
		if ok && !passed {
			changes = append(changes, prChange{kind: "passed", count: n})
		}
		passed = ok
	}
	next.Head, next.Failed, next.Passed = d.HeadSHA, failed, passed

	own := strings.ToLower(d.Viewer)
	if own == "" {
		own = strings.ToLower(d.Author)
	}
	var fresh []gitx.Remark
	for _, r := range d.Remarks {
		newer := r.CreatedAt.After(w.Through) || r.CreatedAt.Equal(w.Through) && !slices.Contains(w.Seen, r.ID)
		if newer && strings.ToLower(r.Author) != own {
			fresh = append(fresh, r)
		}
	}
	if len(fresh) > 0 {
		changes = append(changes, prChange{kind: "remarks", remarks: fresh})
		latest := w.Through
		for _, r := range fresh {
			if r.CreatedAt.After(latest) {
				latest = r.CreatedAt
			}
		}
		if !latest.Equal(w.Through) {
			next.Through, next.Seen = latest, nil
		}
		for _, r := range fresh {
			if r.CreatedAt.Equal(next.Through) {
				next.Seen = append(next.Seen, r.ID)
			}
		}
	}

	if d.Mergeable == "CONFLICTING" && !w.Conflicting {
		changes = append(changes, prChange{kind: "conflict"})
	}
	// UNKNOWN is GitHub still working it out after a push; only a clear
	// answer ends a conflict.
	if d.Mergeable != "UNKNOWN" {
		next.Conflicting = d.Mergeable == "CONFLICTING"
	}

	commentsOnly := len(changes) > 0
	for _, c := range changes {
		if c.kind != "remarks" {
			commentsOnly = false
		}
	}
	progress := headMoved || len(changes) > 0 && !commentsOnly
	if progress {
		next.Wakes = 0
	}
	if commentsOnly {
		next.Wakes++
	}
	return changes, next, commentsOnly && next.Wakes >= prWatchWakeLimit
}

// prWatchPrompt is what the agent reads when the PR changed.
func prWatchPrompt(d gitx.PRDetail, changes []prChange, exhausted bool) string {
	var b strings.Builder
	commit := ""
	if len(d.HeadSHA) >= 7 {
		commit = " on " + d.HeadSHA[:7]
	}
	fmt.Fprintf(&b, "Update on pull request #%d (%s), which starcode is watching for you:\n", d.Number, d.URL)
	for _, c := range changes {
		switch c.kind {
		case "failed":
			fmt.Fprintf(&b, "- Checks failed%s:\n", commit)
			listed(&b, len(c.checks), func(i int) string {
				ch := c.checks[i]
				return strings.TrimSpace(ch.Name + " " + ch.URL)
			})
		case "passed":
			fmt.Fprintf(&b, "- All %d %s passed%s.\n", c.count, plural(c.count, "check", "checks"), commit)
		case "remarks":
			fmt.Fprintf(&b, "- %d new %s:\n", len(c.remarks), plural(len(c.remarks), "comment", "comments"))
			listed(&b, len(c.remarks), func(i int) string {
				r := c.remarks[i]
				who := r.Author
				if who == "" {
					who = "someone"
				}
				if r.Path != "" {
					who += " on " + r.Path
				}
				said := snippet(r.Body)
				if said == "" {
					said = strings.ToLower(strings.ReplaceAll(r.State, "_", " "))
					if said == "" {
						said = "reviewed"
					}
				} else {
					said = `"` + said + `"`
				}
				return strings.TrimSpace(who + ": " + said + " " + r.URL)
			})
		case "conflict":
			fmt.Fprintf(&b, "- The branch now conflicts with %s.\n", d.Base)
		}
	}
	b.WriteString("\n")
	if exhausted {
		fmt.Fprintf(&b, "starcode stopped watching after %d updates in a row that were only comments. Say so if the pull request needs more attention.", prWatchWakeLimit)
	} else {
		b.WriteString("Look into each item and act on it as the task needs. starcode keeps watching and tells you about the next change, so end your turn when you are done.")
	}
	return b.String()
}

func listed(b *strings.Builder, n int, line func(int) string) {
	for i := 0; i < n && i < prWatchListed; i++ {
		b.WriteString("  - " + line(i) + "\n")
	}
	if n > prWatchListed {
		fmt.Fprintf(b, "  - and %d more\n", n-prWatchListed)
	}
}

// snippet is a comment's text on one line, without HTML comments (bots
// hide markers in them), cut to prWatchSnippet characters.
func snippet(body string) string {
	for {
		i := strings.Index(body, "<!--")
		if i < 0 {
			break
		}
		j := strings.Index(body[i:], "-->")
		if j < 0 {
			body = body[:i]
			break
		}
		body = body[:i] + " " + body[i+j+3:]
	}
	body = strings.Join(strings.Fields(body), " ")
	if r := []rune(body); len(r) > prWatchSnippet {
		body = string(r[:prWatchSnippet-3]) + "..."
	}
	return body
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// prDetail reads a pull request; tests replace it.
var prDetail = gitx.PullRequestDetail

// StartPRWatch starts watching the thread's linked pull request from how
// it stands now.
func (a *App) StartPRWatch(ctx context.Context, id string) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.PRNumber <= 0 {
		return errors.New("link a pull request to the thread first")
	}
	p, err := a.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return err
	}
	d, err := prDetail(ctx, p.Path, t.PRRepo, t.PRNumber)
	if err != nil {
		return fmt.Errorf("could not read pull request #%d: %w", t.PRNumber, err)
	}
	if !d.Open() {
		return fmt.Errorf("pull request #%d is %s", t.PRNumber, d.State)
	}
	return a.setPRWatch(ctx, id, prWatchBaseline(d, time.Now()))
}

// StopPRWatch ends the watch; reason is empty when the reader stopped it.
func (a *App) StopPRWatch(ctx context.Context, id, reason string) error {
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.PRWatch == "" {
		return nil
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadPRWatchStopped{Reason: reason})
	return err
}

func (a *App) setPRWatch(ctx context.Context, id string, w PRWatch) error {
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = a.Store.Append(ctx, id, domain.ThreadPRWatchSet{State: raw})
	return err
}

// prWatchBusy keeps sweeps from overlapping when GitHub is slow.
var prWatchBusy atomic.Bool

// watchPRs reads every watched pull request once and wakes the agents
// whose PR changed. A thread with a turn running or an approval waiting
// is left for a later sweep, so the news reaches the agent between
// turns and nothing is lost meanwhile.
func (a *App) watchPRs(ctx context.Context, now time.Time) {
	if !prWatchBusy.CompareAndSwap(false, true) {
		return
	}
	defer prWatchBusy.Store(false)
	ts, err := a.Store.Threads(ctx)
	if err != nil {
		return
	}
	for _, t := range ts {
		select {
		case <-a.stop:
			return
		default:
		}
		if Watching(t) && !busyThread(t) {
			a.watchPR(ctx, t, now)
		}
	}
}

func (a *App) watchPR(ctx context.Context, t store.Thread, now time.Time) {
	var w PRWatch
	if json.Unmarshal([]byte(t.PRWatch), &w) != nil {
		a.StopPRWatch(ctx, t.ID, "its record could not be read")
		return
	}
	p, err := a.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	d, err := prDetail(rctx, p.Path, t.PRRepo, t.PRNumber)
	cancel()
	if err != nil {
		if w.Unread.IsZero() {
			w.Unread = now
			a.setPRWatch(ctx, t.ID, w)
		} else if now.Sub(w.Unread) >= prWatchReadGrace {
			a.stopWatchWithNote(ctx, t, fmt.Sprintf("Stopped watching pull request #%d: it could not be read for %d minutes (%v).", t.PRNumber, int(prWatchReadGrace.Minutes()), err))
		}
		return
	}
	if !d.Open() {
		a.stopWatchWithNote(ctx, t, fmt.Sprintf("Stopped watching pull request #%d: it was %s.", t.PRNumber, d.State))
		return
	}
	changes, next, exhausted := evaluatePRWatch(w, d)
	if len(changes) > 0 {
		if err := a.wakeForPR(ctx, t.ID, prWatchPrompt(d, changes, exhausted)); err != nil {
			// Not told: keep the old state, and the next sweep tries again.
			a.Log.Warn("wake for pull request", "thread", t.ID, "err", err)
			return
		}
	}
	if exhausted {
		a.StopPRWatch(ctx, t.ID, "too many comment-only updates in a row")
		return
	}
	if raw, _ := json.Marshal(next); string(raw) != t.PRWatch {
		a.setPRWatch(ctx, t.ID, next)
	}
}

// stopWatchWithNote ends the watch with a line in the transcript, since
// nothing else would tell the reader it stopped.
func (a *App) stopWatchWithNote(ctx context.Context, t store.Thread, note string) {
	if err := a.StopPRWatch(ctx, t.ID, note); err != nil {
		return
	}
	a.Store.Append(ctx, t.ID, domain.ItemStarted{ID: newID(), Kind: domain.KindSystem, Status: domain.ItemDone, Body: note})
}

// wakeForPR sends the update as an automatic prompt. Like a reply, it
// brings a settled or snoozed thread back into the list.
func (a *App) wakeForPR(ctx context.Context, id, text string) error {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	t, err := a.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if busyThread(t) {
		return errors.New("a turn started meanwhile")
	}
	if t.Archived {
		if _, err := a.Store.Append(ctx, id, domain.ThreadUnarchived{}); err != nil {
			return err
		}
	}
	if t.Snoozed() {
		if _, err := a.Store.Append(ctx, id, domain.ThreadWoken{Auto: true}); err != nil {
			return err
		}
	}
	l, err := a.session(ctx, t)
	if err != nil {
		return err
	}
	return a.startPromptMeta(ctx, l, t, "", text, map[string]any{"auto": "prwatch", "pr": t.PRNumber})
}
