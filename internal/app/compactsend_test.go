package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
	"starcode/internal/agent/fake"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// TestCompactAndSend: the compaction runs as its own turn and the prompt
// goes out after it, from the queue.
func TestCompactAndSend(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"})
	a := New(st, bus.New(256), map[string]agent.Agent{"fake": &fake.Agent{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.Shutdown()
	id, err := a.CreateThread(ctx, "p1", "fake", "")
	if err != nil {
		t.Fatal(err)
	}
	// yolo: the fake asks for no approval.
	if err := a.SetThreadSettings(ctx, id, ThreadSettings{Agent: "fake", PermissionMode: "yolo"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SendPrompt(ctx, id, "hello"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, st, id)
	if err := a.CompactAndSend(ctx, id, "after the summary"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, _ := st.Items(ctx, id)
		notice, prompt := -1, -1
		for i, it := range items {
			if strings.Contains(it.Body, "Context was compacted") && notice < 0 {
				notice = i
			}
			if it.Kind == domain.KindUser && it.Body == "after the summary" {
				prompt = i
			}
		}
		if notice >= 0 && prompt >= 0 {
			if prompt < notice {
				t.Fatalf("the prompt went out before the compaction (prompt %d, notice %d)", prompt, notice)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no compaction then prompt: notice %d, prompt %d", notice, prompt)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitIdle(t *testing.T, st *store.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		th, _ := st.Thread(context.Background(), id)
		if th.Status == domain.StatusIdle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %q", th.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
