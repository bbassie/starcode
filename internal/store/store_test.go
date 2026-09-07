package store

import (
	"context"
	"encoding/json"
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
