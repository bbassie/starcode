package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"starcode/internal/domain"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsageReconstructsAgentAndModelHistory(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/usage", Name: "usage"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t1",
		domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "usage", Agent: "claude", Model: "sonnet"},
		domain.AgentSessionBound{ExternalID: "ext", Model: "claude-sonnet-5"},
		domain.TurnCompleted{TurnID: "one", Status: "done", CostUSD: 0.25, InputTok: 100, OutputTok: 20},
		domain.ThreadSettingsChanged{Agent: "codex", Model: "gpt-5.6"},
		domain.TurnCompleted{TurnID: "two", Status: "done", InputTok: 200, OutputTok: 40},
		domain.ThreadDeleted{},
	); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Usage(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	if got := entries[0]; got.Agent != "claude" || got.Model != "claude-sonnet-5" || got.SessionID != "ext" || got.CostUSD != 0.25 || got.InputTokens != 100 || got.OutputTokens != 20 {
		t.Errorf("first entry = %+v", got)
	}
	if got := entries[1]; got.Agent != "codex" || got.Model != "gpt-5.6" || got.CostUSD != 0 || got.InputTokens != 200 || got.OutputTokens != 40 {
		t.Errorf("second entry = %+v", got)
	}
	if future, err := s.Usage(ctx, time.Now().Add(time.Hour)); err != nil || len(future) != 0 {
		t.Errorf("future usage = %#v, %v", future, err)
	}
}

func TestAppendProjectsAndReplay(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	var published int
	s.Published = func(evs []domain.Event) { published += len(evs) }

	if _, err := s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/x", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t1",
		domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "new thread", Agent: "fake"},
		domain.ItemStarted{ID: "i1", Kind: domain.KindAssistant, Status: domain.ItemRunning, Body: "hel"},
		domain.ItemDelta{ID: "i1", Text: "lo"},
		domain.ItemStarted{ID: "i2", Kind: domain.KindTool, ToolName: "Bash", Status: domain.ItemRunning, Meta: []byte(`{"summary":"ls"}`)},
		domain.ItemDelta{ID: "i2", Field: "output", Text: "a\n"},
		domain.ItemDelta{ID: "i2", Field: "output", Text: "b\n"},
		domain.ItemCompleted{ID: "i2", Status: domain.ItemDone},
		domain.ApprovalRequested{ID: "a1", ItemID: "i2", ToolName: "Bash", Input: []byte(`{"command":"ls"}`)},
		domain.ThreadStatusChanged{Status: domain.StatusAwaitingApproval},
		domain.PromptQueued{ID: "q1", Body: "do this next"},
	); err != nil {
		t.Fatal(err)
	}
	if published != 11 {
		t.Fatalf("published %d events, want 11", published)
	}

	check := func() {
		t.Helper()
		th, err := s.Thread(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if th.Status != domain.StatusAwaitingApproval || th.ProjectID != "p1" {
			t.Fatalf("thread = %+v", th)
		}
		items, err := s.Items(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 || items[0].Body != "hello" || items[1].Output != "a\nb\n" || items[1].Status != domain.ItemDone {
			t.Fatalf("items = %+v", items)
		}
		aps, err := s.PendingApprovals(ctx, "t1")
		if err != nil || len(aps) != 1 || aps[0].ToolName != "Bash" || !aps[0].ResolvedAt.IsZero() {
			t.Fatalf("approvals = %+v, %v", aps, err)
		}
		queued, err := s.QueuedPrompts(ctx, "t1")
		if err != nil || len(queued) != 1 || queued[0].ID != "q1" || queued[0].Body != "do this next" {
			t.Fatalf("queued = %+v, %v", queued, err)
		}
	}
	check()

	n, err := s.Replay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Fatalf("replayed %d, want 11", n)
	}
	check()

	if _, err := s.Append(ctx, "t1", domain.PromptDequeued{ID: "q1"}, domain.ApprovalResolved{ID: "a1", Decision: domain.DecisionAllow}); err != nil {
		t.Fatal(err)
	}
	// The answer time is kept, so the transcript can leave the wait out
	// of its "worked for" counts.
	if all, err := s.Approvals(ctx, "t1"); err != nil || len(all) != 1 || all[0].Decision != domain.DecisionAllow || all[0].ResolvedAt.IsZero() || all[0].ResolvedAt.Before(all[0].CreatedAt) {
		t.Fatalf("approvals after answer = %+v, %v", all, err)
	}
	if _, err := s.Append(ctx, "t1", domain.ThreadDeleted{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Thread(ctx, "t1"); err != ErrNotFound {
		t.Fatalf("thread after delete: %v", err)
	}
	if items, _ := s.Items(ctx, "t1"); len(items) != 0 {
		t.Fatalf("items not cascaded: %d", len(items))
	}
	if queued, _ := s.QueuedPrompts(ctx, "t1"); len(queued) != 0 {
		t.Fatalf("queued prompts not removed: %d", len(queued))
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Second open must not re-run the ALTER TABLE migrations.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	// A database from before schema_migrations existed: columns present,
	// no bookkeeping. Opening must recover instead of failing.
	if _, err := s.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("legacy reopen: %v", err)
	}
	s.Close()
}

func TestArchiveProjectionAndReplay(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/arch", Name: "arch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t1", domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "one", Agent: "claude"}, domain.ThreadArchived{}); err != nil {
		t.Fatal(err)
	}
	th, err := s.Thread(ctx, "t1")
	if err != nil || !th.Archived {
		t.Fatalf("after archive: %+v, %v", th, err)
	}
	if _, err := s.Append(ctx, "t1", domain.ThreadUnarchived{}); err != nil {
		t.Fatal(err)
	}
	if th, _ = s.Thread(ctx, "t1"); th.Archived {
		t.Fatalf("after unarchive: %+v", th)
	}
	if _, err := s.Append(ctx, "t1", domain.ThreadArchived{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	if th, _ = s.Thread(ctx, "t1"); !th.Archived {
		t.Fatalf("after replay: %+v", th)
	}
}

func TestContextProjectionAndReplay(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/ctx", Name: "ctx"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t1", domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "one", Agent: "claude", Model: "sonnet"},
		domain.ContextUsed{Tokens: 42_000, Window: 200_000}); err != nil {
		t.Fatal(err)
	}
	th, _ := s.Thread(ctx, "t1")
	if th.ContextTokens != 42_000 || th.ContextWindow != 200_000 {
		t.Fatalf("after first reading: %d/%d", th.ContextTokens, th.ContextWindow)
	}
	// A reading without a window keeps the one already known.
	if _, err := s.Append(ctx, "t1", domain.ContextUsed{Tokens: 51_000}); err != nil {
		t.Fatal(err)
	}
	if th, _ = s.Thread(ctx, "t1"); th.ContextTokens != 51_000 || th.ContextWindow != 200_000 {
		t.Fatalf("after windowless reading: %d/%d", th.ContextTokens, th.ContextWindow)
	}
	// Another model has another window, so the old one is dropped; the
	// count belongs to the conversation and stays.
	if _, err := s.Append(ctx, "t1", domain.ThreadSettingsChanged{Agent: "claude", Model: "opus[1m]"}); err != nil {
		t.Fatal(err)
	}
	if th, _ = s.Thread(ctx, "t1"); th.ContextTokens != 51_000 || th.ContextWindow != 0 {
		t.Fatalf("after model change: %d/%d", th.ContextTokens, th.ContextWindow)
	}
	// Settings saved without touching the model leave it alone.
	if _, err := s.Append(ctx, "t1", domain.ContextUsed{Tokens: 60_000, Window: 1_000_000},
		domain.ThreadSettingsChanged{Agent: "claude", Model: "opus[1m]", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if th, _ = s.Thread(ctx, "t1"); th.ContextWindow != 1_000_000 {
		t.Fatalf("after effort change: %d", th.ContextWindow)
	}
	if _, err := s.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	if th, _ = s.Thread(ctx, "t1"); th.ContextTokens != 60_000 || th.ContextWindow != 1_000_000 {
		t.Fatalf("after replay: %d/%d", th.ContextTokens, th.ContextWindow)
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/search", Name: "search"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t1",
		domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "Fix the login_page bug", Agent: "claude"},
		domain.ItemStarted{ID: "i1", Kind: domain.KindUser, Body: "please look at the login page,\nit shows 100% wrong"},
		domain.ItemStarted{ID: "i2", Kind: domain.KindTool, ToolName: "Bash", Body: "login page grep"},
		domain.ItemStarted{ID: "i3", Kind: domain.KindAssistant, Body: "The LOGIN page reads the cookie twice."},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t2", domain.ThreadCreated{ID: "t2", ProjectID: "p1", Title: "unrelated", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}

	threads, err := s.SearchThreads(ctx, "login", 10)
	if err != nil || len(threads) != 1 || threads[0].ID != "t1" {
		t.Fatalf("threads = %+v, %v", threads, err)
	}
	// LIKE wildcards in the query are literal.
	if threads, _ = s.SearchThreads(ctx, "login_page", 10); len(threads) != 1 {
		t.Fatalf("underscore search = %+v", threads)
	}
	if threads, _ = s.SearchThreads(ctx, "login%page", 10); len(threads) != 0 {
		t.Fatalf("percent search = %+v", threads)
	}
	if threads, _ = s.SearchThreads(ctx, "", 10); len(threads) != 2 {
		t.Fatalf("empty search = %+v", threads)
	}

	hits, err := s.SearchItems(ctx, "login page", 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	// Newest first, tool output excluded, snippet on one line.
	if hits[0].ItemID != "i3" || hits[1].ItemID != "i1" || hits[0].ThreadTitle != "Fix the login_page bug" {
		t.Fatalf("hit order = %+v", hits)
	}
	if hits[1].Snippet != "please look at the login page, it shows 100% wrong" {
		t.Fatalf("snippet = %q", hits[1].Snippet)
	}
	if hits, _ = s.SearchItems(ctx, "100%", 10); len(hits) != 1 {
		t.Fatalf("percent in body = %+v", hits)
	}
	if got := snippet("aaaa bbbb "+strings.Repeat("x", 300)+" needle "+strings.Repeat("y", 300), "needle", 60); !strings.Contains(got, "needle") || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || len([]rune(got)) > 62 {
		t.Fatalf("long snippet = %q", got)
	}
}

func TestSnippetFoldsRunesOneToOne(t *testing.T) {
	// İ lowercases to a shorter byte sequence; byte offsets from the
	// folded text would cut the original mid-rune.
	got := snippet("İİİİabc def", "ABC", 40)
	if got != "İİİİabc def" {
		t.Fatalf("snippet = %q", got)
	}
	if got := snippet(strings.Repeat("x ", 100)+"İİ needle", "NEEDLE", 30); !strings.HasSuffix(got, "İİ needle") {
		t.Fatalf("snippet = %q", got)
	}
}

func TestSessionRulesProjection(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/r", Name: "r"})
	s.Append(ctx, "t1", domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "one", Agent: "claude"},
		domain.ApprovalRequested{ID: "a1", ToolName: "Bash", Input: json.RawMessage(`{"command":"go test ./..."}`)},
		domain.ApprovalResolved{ID: "a1", Decision: domain.DecisionAllowSession},
		domain.ApprovalRequested{ID: "a2", ToolName: "Edit"},
		domain.ApprovalResolved{ID: "a2", Decision: domain.DecisionAllow},
		domain.ApprovalRequested{ID: "a3", ToolName: "Bash", Input: json.RawMessage(`{"command":"go vet"}`)},
		domain.ApprovalResolved{ID: "a3", Decision: domain.DecisionAllowSession, Auto: true},
	)
	rules, err := s.SessionRules(ctx, "t1")
	if err != nil || len(rules) != 1 || rules[0] != "Bash:go" {
		t.Fatalf("rules = %v, %v", rules, err)
	}
	s.Append(ctx, "t1", domain.RuleRevoked{Key: "Bash:go"})
	if rules, _ = s.SessionRules(ctx, "t1"); len(rules) != 0 {
		t.Fatalf("after revoke = %v", rules)
	}
	s.Append(ctx, "t1", domain.ApprovalRequested{ID: "a4", ToolName: "Write"}, domain.ApprovalResolved{ID: "a4", Decision: domain.DecisionAllowSession})
	if _, err := s.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	if rules, _ = s.SessionRules(ctx, "t1"); len(rules) != 1 || rules[0] != "Write" {
		t.Fatalf("after replay = %v", rules)
	}
}

func TestDraftsAndSeen(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/d", Name: "d"})
	s.Append(ctx, "t1", domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "one", Agent: "claude"})
	if err := s.SaveDraft(ctx, "t1", "half a thought"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Draft(ctx, "t1"); d != "half a thought" {
		t.Fatalf("draft = %q", d)
	}
	s.SaveDraft(ctx, "t1", "")
	if d, _ := s.Draft(ctx, "t1"); d != "" {
		t.Fatalf("draft after clear = %q", d)
	}
	// First look at an idle thread: it was unread (never opened).
	if changed, err := s.MarkSeen(ctx, "t1", time.Now()); err != nil || !changed {
		t.Fatalf("first mark changed = %v, %v", changed, err)
	}
	first, _ := s.Seen(ctx)
	if changed, _ := s.MarkSeen(ctx, "t1", time.Now().Add(-time.Hour)); changed { // never moves back
		t.Fatal("older mark reported a change")
	}
	if seen, _ := s.Seen(ctx); !seen["t1"].Equal(first["t1"]) {
		t.Fatalf("older mark moved seen_at: %v -> %v", first["t1"], seen["t1"])
	}
	if changed, _ := s.MarkSeen(ctx, "t1", time.Now()); changed {
		t.Fatal("re-marking a read thread reported a change")
	}
	// Activity while nobody looks makes it unread again; a running thread
	// is never counted as unread, so marking it changes nothing.
	s.Append(ctx, "t1", domain.ThreadStatusChanged{Status: domain.StatusRunning})
	if changed, _ := s.MarkSeen(ctx, "t1", time.Now()); changed {
		t.Fatal("running thread reported a change")
	}
	s.Append(ctx, "t1", domain.ThreadStatusChanged{Status: domain.StatusIdle})
	if changed, _ := s.MarkSeen(ctx, "t1", time.Now()); !changed {
		t.Fatal("idle thread with new activity did not report a change")
	}
	s.Append(ctx, "t1", domain.ThreadDeleted{})
	if seen, _ := s.Seen(ctx); len(seen) != 0 {
		t.Fatalf("seen after delete = %v", seen)
	}
}

func TestCompactFoldsDeltas(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/c", Name: "c"})
	s.Append(ctx, "t1", domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "one", Agent: "claude"},
		domain.ItemStarted{ID: "i1", Kind: domain.KindAssistant, Status: domain.ItemRunning, Body: "He"},
		domain.ItemDelta{ID: "i1", Text: "llo"},
		domain.ItemDelta{ID: "i1", Text: " wor"},
		domain.ItemDelta{ID: "i1", Text: "ld"},
		domain.ItemStarted{ID: "i2", Kind: domain.KindTool, ToolName: "Bash", Status: domain.ItemRunning},
		domain.ItemDelta{ID: "i2", Field: "output", Text: "a"},
		domain.ItemDelta{ID: "i2", Field: "output", Text: "b"},
		domain.ItemDelta{ID: "i2", Text: "x"},
		domain.ItemCompleted{ID: "i2", Status: domain.ItemDone},
		domain.ItemCompleted{ID: "i1", Status: domain.ItemDone},
	)
	before, _ := s.Items(ctx, "t1")
	removed, err := s.Compact(ctx, "t1")
	if err != nil || removed != 3 {
		t.Fatalf("removed = %d, %v", removed, err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM events WHERE type='item.delta'`).Scan(&n)
	if n != 3 {
		t.Fatalf("delta rows = %d", n)
	}
	if _, err := s.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Items(ctx, "t1")
	if len(after) != 2 || after[0].Body != before[0].Body || after[1].Output != before[1].Output || after[1].Body != before[1].Body || after[0].Body != "Hello world" || after[1].Output != "ab" {
		t.Fatalf("after replay = %+v", after)
	}
	if again, _ := s.Compact(ctx, ""); again != 0 {
		t.Fatalf("second compact removed %d", again)
	}
}

func TestPRLinkProjectionAndStates(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/pr", Name: "pr"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t1", domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "t", Agent: "fake"}); err != nil {
		t.Fatal(err)
	}
	th, _ := s.Thread(ctx, "t1")
	before := th.UpdatedAt
	time.Sleep(2 * time.Millisecond)
	if _, err := s.Append(ctx, "t1", domain.ThreadPRLinked{Repo: "o/r", Number: 12, URL: "https://github.com/o/r/pull/12"}); err != nil {
		t.Fatal(err)
	}
	th, _ = s.Thread(ctx, "t1")
	pr, ok := th.Linked()
	if !ok || pr != (PR{Repo: "o/r", Number: 12}) || th.PRURL != "https://github.com/o/r/pull/12" {
		t.Fatalf("linked = %+v %v", th, ok)
	}
	if !th.UpdatedAt.Equal(before) {
		t.Error("a link touched updated_at, which would mark the thread unread")
	}
	if err := s.SavePRStates(ctx, []PRState{{PR: pr, Title: "T", State: "open", Review: "changes_requested", CheckedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	states, err := s.PRStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st := states[pr]; st.Title != "T" || st.State != "open" || st.Review != "changes_requested" || st.CheckedAt.IsZero() {
		t.Errorf("state = %+v", st)
	}
	if _, err := s.Append(ctx, "t1", domain.ThreadPRUnlinked{}); err != nil {
		t.Fatal(err)
	}
	th, _ = s.Thread(ctx, "t1")
	if _, ok := th.Linked(); ok {
		t.Error("still linked after unlink")
	}
	// Replay rebuilds the link from the log.
	if _, err := s.Append(ctx, "t1", domain.ThreadPRLinked{Repo: "o/r", Number: 13, URL: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	th, _ = s.Thread(ctx, "t1")
	if th.PRNumber != 13 {
		t.Errorf("after replay PRNumber = %d, want 13", th.PRNumber)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if v := s.Setting(ctx, "settle_merged", "1"); v != "1" {
		t.Fatalf("missing setting = %q, want the default", v)
	}
	if !s.SettleMerged(ctx) || s.SettleIdleDays(ctx) != 3 {
		t.Fatalf("defaults: merged=%v days=%d", s.SettleMerged(ctx), s.SettleIdleDays(ctx))
	}
	if err := s.SetSetting(ctx, "settle_merged", "0"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "settle_idle_days", "7"); err != nil {
		t.Fatal(err)
	}
	if s.SettleMerged(ctx) || s.SettleIdleDays(ctx) != 7 {
		t.Fatalf("after set: merged=%v days=%d", s.SettleMerged(ctx), s.SettleIdleDays(ctx))
	}
	// Setting a key again replaces the value rather than adding a row.
	if err := s.SetSetting(ctx, "settle_idle_days", "0"); err != nil {
		t.Fatal(err)
	}
	if n := s.SettleIdleDays(ctx); n != 0 {
		t.Fatalf("days after overwrite = %d", n)
	}
	if v := s.Setting(ctx, "settle_idle_days", "3"); v != "0" {
		t.Fatalf("raw value = %q", v)
	}
}

// ftsRows counts the search index rows of a thread.
func ftsRows(t *testing.T, s *Store, threadID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM items_fts WHERE thread_id=?`, threadID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSearchIndexFollowsItems(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	hitIDs := func(q string) string {
		t.Helper()
		hits, err := s.SearchItems(ctx, q, 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ItemID)
		}
		return strings.Join(ids, ",")
	}
	if _, err := s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/fts", Name: "fts"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t1",
		domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "one", Agent: "claude"},
		domain.ItemStarted{ID: "u1", Kind: domain.KindUser, Status: domain.ItemDone, Body: "why does the scheduler stall?"},
		domain.ItemStarted{ID: "a1", Kind: domain.KindAssistant, Status: domain.ItemRunning, Body: "The sched"},
		domain.ItemDelta{ID: "a1", Text: "uler waits on a café mutex"},
		domain.ItemStarted{ID: "x1", Kind: domain.KindTool, ToolName: "Bash", Status: domain.ItemDone, Body: "scheduler grep"},
	); err != nil {
		t.Fatal(err)
	}
	// The prompt is searchable at once, the streaming reply is not yet,
	// the tool call never is.
	if got := hitIDs("scheduler"); got != "u1" {
		t.Fatalf("while streaming = %q", got)
	}
	if _, err := s.Append(ctx, "t1", domain.ItemCompleted{ID: "a1", Status: domain.ItemDone}); err != nil {
		t.Fatal(err)
	}
	if got := hitIDs("scheduler"); got != "a1,u1" {
		t.Fatalf("after completion = %q", got)
	}
	// Words match by prefix and all of them must match; case and
	// diacritics do not count.
	for q, want := range map[string]string{
		"sched":        "a1,u1",
		"SCHED stal":   "u1",
		"cafe mut":     "a1",
		"sched absent": "",
		"heduler":      "",
	} {
		if got := hitIDs(q); got != want {
			t.Fatalf("search %q = %q, want %q", q, got, want)
		}
	}
	// A completion that carries the whole body replaces the indexed text.
	if _, err := s.Append(ctx, "t1", domain.ItemCompleted{ID: "a1", Status: domain.ItemDone, Body: "It waits on a lock."}); err != nil {
		t.Fatal(err)
	}
	if got := hitIDs("mutex"); got != "" {
		t.Fatalf("old body still indexed: %q", got)
	}
	if got := hitIDs("lock"); got != "a1" {
		t.Fatalf("new body = %q", got)
	}
	if n := ftsRows(t, s, "t1"); n != 2 {
		t.Fatalf("index rows = %d", n)
	}

	// Query syntax in the text is plain text.
	for _, q := range []string{
		`"`, `""`, `"lock`, `lock"`, `wa"its`, `*`, `lock*`, `-lock`, `- lock`, `lock -waits`,
		`NEAR`, `NEAR(lock waits)`, `lock NEAR waits`, `(lock`, `lock)`, `()`, `lock AND`, `OR`, `NOT lock`,
		`body:lock`, `{body}:lock`, `^lock`, `lock + waits`, `a:b:c`, `'`, `\`, `%`, "lock\twaits\n",
	} {
		if _, err := s.SearchItems(ctx, q, 10); err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
	}
	for q, want := range map[string]string{`"lock`: "a1", `-lock`: "a1", `lock)`: "a1", `- lock`: "a1", `lock* wa"`: "a1", `*`: "", `NEAR`: ""} {
		if got := hitIDs(q); got != want {
			t.Fatalf("search %q = %q, want %q", q, got, want)
		}
	}

	// Replay empties the index and fills it again from the log.
	if _, err := s.db.Exec(`INSERT INTO items_fts(rowid, item_id, thread_id, body) VALUES(999999, 'ghost', 't1', 'lock')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	if got := hitIDs("lock"); got != "a1" {
		t.Fatalf("after replay = %q", got)
	}
	if got := hitIDs("sched"); got != "u1" {
		t.Fatalf("after replay = %q", got)
	}
	if n := ftsRows(t, s, "t1"); n != 2 {
		t.Fatalf("index rows after replay = %d", n)
	}

	// A deleted thread leaves nothing in the index; other threads stay.
	if _, err := s.Append(ctx, "t2",
		domain.ThreadCreated{ID: "t2", ProjectID: "p1", Title: "two", Agent: "claude"},
		domain.ItemStarted{ID: "u2", Kind: domain.KindUser, Status: domain.ItemDone, Body: "lock order?"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "t1", domain.ThreadDeleted{}); err != nil {
		t.Fatal(err)
	}
	if n := ftsRows(t, s, "t1"); n != 0 {
		t.Fatalf("index rows after delete = %d", n)
	}
	if got := hitIDs("lock"); got != "u2" {
		t.Fatalf("after delete = %q", got)
	}
}

// The migration indexes the items a database already has.
func TestSearchIndexBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "b.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/b", Name: "b"})
	s.Append(ctx, "t1", domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "one", Agent: "claude"},
		domain.ItemStarted{ID: "u1", Kind: domain.KindUser, Status: domain.ItemDone, Body: "an older prompt"},
		domain.ItemStarted{ID: "x1", Kind: domain.KindTool, Status: domain.ItemDone, Body: "older tool"},
	)
	// Back to the state before the migration: no index, no record of it.
	if _, err := s.db.Exec(`DROP TABLE items_fts`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE name LIKE '%014_items_fts.sql'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hits, err := s.SearchItems(ctx, "older", 10)
	if err != nil || len(hits) != 1 || hits[0].ItemID != "u1" {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
}

func TestCompactInBatches(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	old := compactBatch
	compactBatch = 7
	t.Cleanup(func() { compactBatch = old })

	s.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: "/tmp/cb", Name: "cb"})
	s.Append(ctx, "t1", domain.ThreadCreated{ID: "t1", ProjectID: "p1", Title: "one", Agent: "claude"})
	s.Append(ctx, "t2", domain.ThreadCreated{ID: "t2", ProjectID: "p1", Title: "two", Agent: "claude"})
	// 60 items over two threads. Each streams while the one before it is
	// still open, so the seq ranges of neighbouring items overlap and a
	// batch's range holds deltas of items outside it. Every third item has
	// one delta only and must stay as it is.
	const items = 60
	wantRows := 0
	for i := 0; i < items; i++ {
		tid := "t1"
		if i%2 == 1 {
			tid = "t2"
		}
		id := fmt.Sprintf("i%02d", i)
		s.Append(ctx, tid, domain.ItemStarted{ID: id, Kind: domain.KindAssistant, Status: domain.ItemRunning, Body: id + ":"})
		if i%3 == 0 {
			s.Append(ctx, tid, domain.ItemDelta{ID: id, Text: "only"})
			wantRows++
		} else {
			s.Append(ctx, tid,
				domain.ItemDelta{ID: id, Text: "a"},
				domain.ItemDelta{ID: id, Field: "output", Text: "1"},
				domain.ItemDelta{ID: id, Text: "b"},
			)
			wantRows += 2
		}
		if i >= 2 {
			prev := fmt.Sprintf("i%02d", i-2)
			s.Append(ctx, tid,
				domain.ItemDelta{ID: prev, Text: "z"},
				domain.ItemDelta{ID: prev, Field: "output", Text: "9"},
				domain.ItemCompleted{ID: prev, Status: domain.ItemDone},
			)
			if (i-2)%3 == 0 {
				wantRows++ // its first output delta
			}
		}
	}
	snapshot := func() []Item {
		t.Helper()
		var all []Item
		for _, tid := range []string{"t1", "t2"} {
			its, err := s.Items(ctx, tid)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, its...)
		}
		return all
	}
	before := snapshot()
	var deltas int
	s.db.QueryRow(`SELECT count(*) FROM events WHERE type='item.delta'`).Scan(&deltas)

	// One thread first: the other thread's rows are not touched.
	var t2Before, t2After int
	s.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id='t2'`).Scan(&t2Before)
	r1, err := s.Compact(ctx, "t1")
	if err != nil || r1 == 0 {
		t.Fatalf("compact t1 = %d, %v", r1, err)
	}
	s.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id='t2'`).Scan(&t2After)
	if t2Before != t2After {
		t.Fatalf("t2 events %d -> %d", t2Before, t2After)
	}
	r2, err := s.Compact(ctx, "")
	if err != nil || r2 == 0 {
		t.Fatalf("compact all = %d, %v", r2, err)
	}
	var left, multi int
	s.db.QueryRow(`SELECT count(*) FROM events WHERE type='item.delta'`).Scan(&left)
	if left != wantRows || r1+r2 != deltas-left {
		t.Fatalf("delta rows = %d, want %d; removed %d of %d", left, wantRows, r1+r2, deltas)
	}
	s.db.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM events WHERE type='item.delta'
		GROUP BY json_extract(payload,'$.id'), COALESCE(json_extract(payload,'$.field'),'') HAVING count(*) > 1)`).Scan(&multi)
	if multi != 0 {
		t.Fatalf("%d groups still have several rows", multi)
	}
	if _, err := s.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	if len(after) != items || len(before) != items {
		t.Fatalf("items = %d, %d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Body != after[i].Body || before[i].Output != after[i].Output || before[i].Status != after[i].Status {
			t.Fatalf("item %d: %+v became %+v", i, before[i], after[i])
		}
	}
	if b := before[1]; b.Body != b.ID+":abz" || b.Output != "19" {
		t.Fatalf("folded text = %+v", b)
	}
	if again, _ := s.Compact(ctx, ""); again != 0 {
		t.Fatalf("second compact removed %d", again)
	}
}
