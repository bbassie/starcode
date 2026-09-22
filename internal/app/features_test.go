package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// finishTurn completes the running turn of session s with anchor.
func finishTurn(t *testing.T, st *store.Store, threadID string, s *queueSession, anchor string) {
	t.Helper()
	s.events <- agent.Event{Kind: agent.KindTurnCompleted, TurnCompleted: &agent.TurnCompleted{TurnID: "t", Anchor: anchor, Status: "done"}}
	waitStatus(t, st, threadID, domain.StatusIdle)
}

func userItems(t *testing.T, st *store.Store, threadID string) []store.Item {
	t.Helper()
	items, err := st.Items(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Item
	for _, it := range items {
		if it.Kind == domain.KindUser {
			out = append(out, it)
		}
	}
	return out
}

func newFreshApp(t *testing.T, projectDir string) (*App, *store.Store, *freshAgent) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if projectDir == "" {
		projectDir = t.TempDir()
	}
	if _, err := st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: projectDir, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	ag := &freshAgent{}
	a := New(st, bus.New(256), map[string]agent.Agent{"fresh-test": ag}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.Shutdown)
	return a, st, ag
}

func (a *freshAgent) session(i int) *queueSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[i]
}

func (a *freshAgent) config(i int) agent.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.configs[i]
}

// TestRewindForksTheConversation sends three prompts, rewinds to the
// second, and checks the transcript is cut there, the prompt comes back,
// and the next session forks the conversation at the first turn's anchor.
func TestRewindForksTheConversation(t *testing.T) {
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	id, err := a.CreateThread(ctx, "p1", "fresh-test", "", ThreadStart{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SendPrompt(ctx, id, "one"); err != nil {
		t.Fatal(err)
	}
	s := ag.session(0)
	receivePrompt(t, s.sent)
	s.events <- agent.Event{Kind: agent.KindSessionInfo, SessionInfo: &agent.SessionInfo{ExternalID: "conv-1"}}
	finishTurn(t, st, id, s, "anchor-1")
	for i, p := range []string{"two", "three"} {
		if err := a.SendPrompt(ctx, id, p); err != nil {
			t.Fatal(err)
		}
		receivePrompt(t, s.sent)
		finishTurn(t, st, id, s, "anchor-"+string(rune('2'+i)))
	}
	prompts := userItems(t, st, id)
	if len(prompts) != 3 {
		t.Fatalf("prompts = %d", len(prompts))
	}
	var m PromptMeta
	json.Unmarshal(prompts[1].Meta, &m)
	if m.Session != "conv-1" || m.Anchor != "anchor-1" {
		t.Fatalf("second prompt meta = %+v", m)
	}

	text, err := a.Rewind(ctx, id, prompts[1].ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if text != "two" {
		t.Fatalf("rewind gave back %q", text)
	}
	if d, _ := st.Draft(ctx, id); d != "two" {
		t.Fatalf("draft = %q", d)
	}
	left := userItems(t, st, id)
	if len(left) != 1 || left[0].Body != "one" {
		t.Fatalf("prompts after rewind = %+v", left)
	}
	th, _ := st.Thread(ctx, id)
	if th.ExternalSessionID != "conv-1" || th.ForkAt != "anchor-1" {
		t.Fatalf("thread after rewind: session %q fork %q", th.ExternalSessionID, th.ForkAt)
	}

	if err := a.SendPrompt(ctx, id, "two again"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return ag.started() == 2 })
	if cfg := ag.config(1); cfg.ResumeID != "conv-1" || cfg.ForkAt != "anchor-1" {
		t.Fatalf("session after rewind started with %+v", cfg)
	}
	s2 := ag.session(1)
	receivePrompt(t, s2.sent)
	s2.events <- agent.Event{Kind: agent.KindSessionInfo, SessionInfo: &agent.SessionInfo{ExternalID: "conv-2"}}
	finishTurn(t, st, id, s2, "anchor-x")
	th, _ = st.Thread(ctx, id)
	if th.ExternalSessionID != "conv-2" || th.ForkAt != "" {
		t.Fatalf("fork not settled: session %q fork %q", th.ExternalSessionID, th.ForkAt)
	}

	// Back to the first prompt: a fresh conversation.
	if _, err := a.Rewind(ctx, id, left[0].ID, false); err != nil {
		t.Fatal(err)
	}
	th, _ = st.Thread(ctx, id)
	if th.ExternalSessionID != "" || th.ForkAt != "" || len(userItems(t, st, id)) != 0 {
		t.Fatalf("rewind to the first prompt: session %q fork %q", th.ExternalSessionID, th.ForkAt)
	}
}

// TestRewindRestoresWorktreeFiles checks the files come back with
// "revert files too" in a worktree thread.
func TestRewindRestoresWorktreeFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet", "-b", "main"}, {"commit", "--quiet", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	a, st, ag := newFreshApp(t, repo)
	a.WorktreeRoot = filepath.Join(t.TempDir(), "wt")
	id, err := a.CreateThread(ctx, "p1", "fresh-test", "", ThreadStart{Mode: "worktree", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	th, _ := st.Thread(ctx, id)
	if th.Worktree == "" {
		t.Fatal("no worktree")
	}
	file := filepath.Join(th.Worktree, "f.txt")
	os.WriteFile(file, []byte("before\n"), 0o644)
	if err := a.SendPrompt(ctx, id, "one"); err != nil {
		t.Fatal(err)
	}
	s := ag.session(0)
	receivePrompt(t, s.sent)
	s.events <- agent.Event{Kind: agent.KindSessionInfo, SessionInfo: &agent.SessionInfo{ExternalID: "conv"}}
	os.WriteFile(file, []byte("agent wrote this\n"), 0o644)
	finishTurn(t, st, id, s, "a1")

	prompt := userItems(t, st, id)[0]
	th, _ = st.Thread(ctx, id)
	if info := CanRewind(th, prompt, true, false); !info.OK || !info.Files {
		t.Fatalf("CanRewind = %+v", info)
	}
	if _, err := a.Rewind(ctx, id, prompt.ID, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != "before\n" {
		t.Fatalf("file after rewind = %q", b)
	}
}

func TestSnoozeWakesOnTime(t *testing.T) {
	ctx := context.Background()
	a, st, _ := newFreshApp(t, "")
	id, err := a.CreateThread(ctx, "p1", "fresh-test", "", ThreadStart{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.Thread(ctx, id)
	if err := a.Snooze(ctx, id, time.Now().Add(-time.Minute)); err == nil {
		t.Fatal("a snooze into the past was taken")
	}
	until := time.Now().Add(time.Hour)
	if err := a.Snooze(ctx, id, until); err != nil {
		t.Fatal(err)
	}
	th, _ := st.Thread(ctx, id)
	if !th.Snoozed() || th.SnoozedUntil.Sub(until).Abs() > time.Millisecond {
		t.Fatalf("snoozed until %v, want %v", th.SnoozedUntil, until)
	}
	if !th.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("snoozing counted as activity")
	}
	// The settle sweep leaves a snoozed thread alone.
	a.settleIdle(ctx, time.Now().Add(30*24*time.Hour))
	if th, _ := st.Thread(ctx, id); th.Archived {
		t.Fatal("a snoozed thread settled")
	}
	a.wakeDue(ctx, time.Now())
	if th, _ := st.Thread(ctx, id); !th.Snoozed() {
		t.Fatal("woke before its time")
	}
	a.wakeDue(ctx, until.Add(time.Second))
	th, _ = st.Thread(ctx, id)
	if th.Snoozed() {
		t.Fatal("did not wake")
	}
	if !th.UpdatedAt.After(before.UpdatedAt) {
		t.Fatal("waking on time should count as activity")
	}
}

// TestSettleKeepsPlace checks settling and unsettling leave updated_at
// alone (so an undo puts the row back), and that an unsettle holds the
// idle sweep off for another period.
func TestSettleKeepsPlace(t *testing.T) {
	ctx := context.Background()
	a, st, _ := newFreshApp(t, "")
	id, err := a.CreateThread(ctx, "p1", "fresh-test", "", ThreadStart{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.Thread(ctx, id)
	if err := a.ArchiveThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := a.UnarchiveThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	th, _ := st.Thread(ctx, id)
	if !th.UpdatedAt.Equal(before.UpdatedAt) || th.Archived || th.HeldAt.IsZero() {
		t.Fatalf("after settle and unsettle: %+v", th)
	}
	// Four days after the thread's last activity but only just after the
	// unsettle: nothing to settle yet.
	a.settleIdle(ctx, th.HeldAt.Add(2*24*time.Hour))
	if th, _ := st.Thread(ctx, id); th.Archived {
		t.Fatal("settled again right after an unsettle")
	}
	a.settleIdle(ctx, th.HeldAt.Add(4*24*time.Hour))
	if th, _ := st.Thread(ctx, id); !th.Archived {
		t.Fatal("not settled a full period after the unsettle")
	}
}

// TestRecoverResumesCutOffTurn plays a restart: the first App shuts down
// mid-turn, the second resumes the turn with the resume prompt.
func TestRecoverResumesCutOffTurn(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"})
	ag := &freshAgent{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(st, bus.New(64), map[string]agent.Agent{"fresh-test": ag}, log)
	id, _ := a.CreateThread(ctx, "p1", "fresh-test", "", ThreadStart{})
	if err := a.SendPrompt(ctx, id, "long job"); err != nil {
		t.Fatal(err)
	}
	s := ag.session(0)
	receivePrompt(t, s.sent)
	s.events <- agent.Event{Kind: agent.KindSessionInfo, SessionInfo: &agent.SessionInfo{ExternalID: "conv"}}
	waitFor(t, func() bool { th, _ := st.Thread(ctx, id); return th.ExternalSessionID == "conv" })
	// The dying session reports an error; the shutdown must not write it.
	a.closing.Store(true)
	s.events <- agent.Event{Kind: agent.KindClosed, Closed: &agent.Closed{Err: io.ErrUnexpectedEOF}}
	a.Shutdown()
	time.Sleep(20 * time.Millisecond)
	if th, _ := st.Thread(ctx, id); th.Status != domain.StatusRunning {
		t.Fatalf("status after shutdown = %q, want running", th.Status)
	}

	// A real restart opens the database again.
	st.Close()
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ag2 := &freshAgent{}
	b := New(st, bus.New(64), map[string]agent.Agent{"fresh-test": ag2}, log)
	defer b.Shutdown()
	if err := b.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if ag2.started() != 0 {
		t.Fatal("Recover resumed before the server was up")
	}
	b.ResumeCutOff(ctx)
	if ag2.started() != 1 || ag2.config(0).ResumeID != "conv" {
		t.Fatalf("resume did not start a session on conv")
	}
	if got := receivePrompt(t, ag2.session(0).sent); !strings.Contains(got, "restarted") {
		t.Fatalf("resume prompt = %q", got)
	}
	ps := userItems(t, st, id)
	if m := promptMeta(ps[len(ps)-1]); m.Auto != "resume" {
		t.Fatalf("resume prompt meta = %+v", m)
	}
	notes := systemNotes(t, st, id)
	if len(notes) == 0 || !strings.Contains(notes[len(notes)-1], "resuming") {
		t.Fatalf("notes = %q", notes)
	}
}

func TestProjectSettings(t *testing.T) {
	ctx := context.Background()
	a, st, _ := newFreshApp(t, "")
	if err := a.SetProjectSettings(ctx, "p1", ProjectSettings{Agent: "nope"}); err == nil {
		t.Fatal("an unknown agent was taken")
	}
	if err := a.SetProjectSettings(ctx, "p1", ProjectSettings{Agent: "fresh-test", Model: "m", Mode: "yolo", Cleanup: "off"}); err != nil {
		t.Fatal(err)
	}
	// The worktree toggle keeps the rest.
	if err := a.SetProjectWorktrees(ctx, "p1", true); err != nil {
		t.Fatal(err)
	}
	p, _ := st.Project(ctx, "p1")
	if !p.Worktrees || p.Agent != "fresh-test" || p.Model != "m" || p.Mode != "yolo" || p.Cleanup != "off" {
		t.Fatalf("project = %+v", p)
	}
	// Clearing the agent clears the model with it.
	if err := a.SetProjectSettings(ctx, "p1", ProjectSettings{Worktrees: true, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.Project(ctx, "p1"); p.HasDefaults() || p.Model != "" {
		t.Fatalf("project after clearing = %+v", p)
	}
}

// TestCleanWorktrees runs the cleanup rules over a worktree thread: off
// by default, kept while it holds work, removed once a settled thread's
// commits are all in the default branch, kept when the project says so.
func TestCleanWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet", "-b", "main"}, {"commit", "--quiet", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	a, st, _ := newFreshApp(t, repo)
	a.WorktreeRoot = filepath.Join(t.TempDir(), "wt")
	id, err := a.CreateThread(ctx, "p1", "fresh-test", "", ThreadStart{Mode: "worktree", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	th, _ := st.Thread(ctx, id)
	onDisk := func() bool { _, err := os.Stat(th.Worktree); return err == nil }
	if !onDisk() {
		t.Fatal("no worktree")
	}
	a.ArchiveThread(ctx, id)
	a.CleanWorktrees(ctx)
	if !onDisk() {
		t.Fatal("removed with the rules off")
	}
	st.SetSetting(ctx, "wt_clean_merged", "1")
	os.WriteFile(filepath.Join(th.Worktree, "wip.txt"), []byte("wip"), 0o644)
	a.CleanWorktrees(ctx)
	if !onDisk() {
		t.Fatal("removed a worktree with an untracked file")
	}
	os.Remove(filepath.Join(th.Worktree, "wip.txt"))
	a.SetProjectSettings(ctx, "p1", ProjectSettings{Cleanup: "off"})
	a.CleanWorktrees(ctx)
	if !onDisk() {
		t.Fatal("removed with the project's cleanup off")
	}
	a.SetProjectSettings(ctx, "p1", ProjectSettings{})
	a.KeepWorktree = func(dir string) bool { return dir == th.Worktree }
	a.CleanWorktrees(ctx)
	if !onDisk() {
		t.Fatal("removed a worktree KeepWorktree protects")
	}
	a.KeepWorktree = nil
	a.CleanWorktrees(ctx)
	if onDisk() {
		t.Fatal("a settled, merged, clean worktree was kept")
	}
	// The thread keeps its worktree and branch; the next session puts it back.
	th2, _ := st.Thread(ctx, id)
	if th2.Worktree != th.Worktree || th2.WorktreeBranch == "" {
		t.Fatalf("thread lost its worktree: %+v", th2)
	}
	if err := a.SendPrompt(ctx, id, "again"); err != nil {
		t.Fatal(err)
	}
	if !onDisk() {
		t.Fatal("the next prompt did not check the worktree out again")
	}
}
