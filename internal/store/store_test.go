package store

import (
	"context"
	"path/filepath"
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
	if got := entries[0]; got.Agent != "claude" || got.Model != "claude-sonnet-5" || got.CostUSD != 0.25 || got.InputTokens != 100 || got.OutputTokens != 20 {
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
		if err != nil || len(aps) != 1 || aps[0].ToolName != "Bash" {
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

	if _, err := s.Append(ctx, "t1", domain.PromptDequeued{ID: "q1"}, domain.ApprovalResolved{ID: "a1", Decision: domain.DecisionAllow}, domain.ThreadDeleted{}); err != nil {
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
