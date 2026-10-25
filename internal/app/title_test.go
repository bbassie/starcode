package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"starcode/internal/agent"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// askAgent answers Ask with a fixed reply and keeps the prompt.
type askAgent struct {
	freshAgent
	reply  string
	prompt string
}

func (a *askAgent) Ask(_ context.Context, prompt string) (string, error) {
	a.prompt = prompt
	return a.reply, nil
}

func TestRetitleThread(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"})
	ag := &askAgent{reply: "\"Title: Find in changes for the diff panel.\"\nextra"}
	a := New(st, bus.New(64), map[string]agent.Agent{"fresh-test": ag}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.Shutdown()

	id, err := a.CreateThread(ctx, "p1", "fresh-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RetitleThread(ctx, id); err == nil {
		t.Fatal("a thread with no conversation got a title")
	}
	st.Append(ctx, id,
		domain.ItemStarted{ID: "u1", Kind: domain.KindUser, Status: domain.ItemDone, Body: "fix the thing"},
		domain.ItemStarted{ID: "a1", Kind: domain.KindAssistant, Status: domain.ItemDone, Body: "Added a search box."},
	)
	if err := a.RetitleThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	th, _ := st.Thread(ctx, id)
	if th.Title != "Find in changes for the diff panel" {
		t.Errorf("title = %q", th.Title)
	}
	if !strings.Contains(ag.prompt, "Me: fix the thing\n\nAgent: Added a search box.") || !strings.Contains(ag.prompt, `The current title is "new thread"`) {
		t.Errorf("prompt:\n%s", ag.prompt)
	}
}

func TestTitleExcerptKeepsStartAndEnd(t *testing.T) {
	var items []store.Item
	items = append(items, store.Item{Kind: domain.KindUser, Body: "first ask"})
	for range 40 {
		items = append(items, store.Item{Kind: domain.KindAssistant, Body: strings.Repeat("x", 1000)})
	}
	items = append(items, store.Item{Kind: domain.KindUser, Body: "last ask"})
	got := titleExcerpt(items)
	if !strings.HasPrefix(got, "Me: first ask\n\n[…]") || !strings.HasSuffix(got, "Me: last ask") || len(got) > titleExcerptCap+20 {
		t.Errorf("excerpt is %d bytes, starts %q", len(got), got[:30])
	}
}
