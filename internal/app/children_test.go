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

func childApp(t *testing.T) (*App, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"})
	a := New(st, bus.New(256), map[string]agent.Agent{"fake": &fake.Agent{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.Shutdown)
	id, err := a.CreateThread(ctx, "p1", "fake", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetThreadSettings(ctx, id, ThreadSettings{Agent: "fake", PermissionMode: "yolo"}); err != nil {
		t.Fatal(err)
	}
	return a, id
}

func spawn(t *testing.T, a *App, parent string, report string) string {
	t.Helper()
	ctx := context.Background()
	id, err := a.SpawnThread(ctx, parent, "", "fake", "", report)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetThreadSettings(ctx, id, ThreadSettings{Agent: "fake", PermissionMode: "yolo"}); err != nil {
		t.Fatal(err)
	}
	return id
}

// Children that finish close together wake their parent once, with each
// one's reply.
func TestChildrenWakeTheirParentOnce(t *testing.T) {
	defer func(d time.Duration) { childBatch = d }(childBatch)
	childBatch = 300 * time.Millisecond
	ctx := context.Background()
	a, parent := childApp(t)
	c1, c2 := spawn(t, a, parent, ReportNow), spawn(t, a, parent, ReportNow)
	for _, c := range []string{c1, c2} {
		if err := a.SendPrompt(ctx, c, "work"); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, _ := a.Store.Items(ctx, parent)
		var wakes []store.Item
		for _, it := range items {
			if it.Kind == domain.KindUser {
				wakes = append(wakes, it)
			}
		}
		if len(wakes) > 0 {
			if len(wakes) != 1 || !strings.Contains(wakes[0].Body, c1) || !strings.Contains(wakes[0].Body, c2) {
				t.Fatalf("wakes %+v", wakes)
			}
			if m := promptMeta(wakes[0]); m.Auto != "child" {
				t.Fatalf("meta %+v", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the parent was never woken")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitIdle(t, a.Store, parent)
}

// A child that reports with the next prompt does not start a turn on its
// parent; its reply goes along with the next prompt the parent gets.
func TestChildReportsWithTheNextPrompt(t *testing.T) {
	defer func(d time.Duration) { childBatch = d }(childBatch)
	childBatch = 50 * time.Millisecond
	ctx := context.Background()
	a, parent := childApp(t)
	c := spawn(t, a, parent, ReportNext)
	if err := a.SendPrompt(ctx, c, "work"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a.Store, c)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if th, _ := a.Store.Thread(ctx, c); th.ReportPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child's report was never marked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(4 * childBatch)
	if items, _ := a.Store.Items(ctx, parent); len(items) != 0 {
		t.Fatalf("the parent was woken: %+v", items)
	}
	if err := a.SendPrompt(ctx, parent, "next thing"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a.Store, parent)
	items, _ := a.Store.Items(ctx, parent)
	if len(items) == 0 || items[0].Kind != domain.KindUser || items[0].Body != "next thing" {
		t.Fatalf("items %+v", items)
	}
	if m := promptMeta(items[0]); len(m.Reports) != 1 || m.Reports[0] != c {
		t.Fatalf("meta %+v", m)
	}
	if th, _ := a.Store.Thread(ctx, c); th.ReportPending {
		t.Fatal("the report is still pending after it went out")
	}
}

func TestSpawnDepth(t *testing.T) {
	a, root := childApp(t)
	c := spawn(t, a, root, ReportNone)
	g := spawn(t, a, c, ReportNone)
	if _, err := a.SpawnThread(context.Background(), g, "", "fake", "", ReportNone); err == nil {
		t.Fatal("a fourth level was allowed")
	}
}

// A snooze that holds a prompt sends it when it runs out.
func TestSnoozeWithPrompt(t *testing.T) {
	ctx := context.Background()
	a, id := childApp(t)
	if err := a.SnoozeWith(ctx, id, time.Now().Add(time.Minute), "look at CI again"); err != nil {
		t.Fatal(err)
	}
	a.wakeDue(ctx, time.Now().Add(2*time.Minute))
	items, _ := a.Store.Items(ctx, id)
	if len(items) == 0 || items[0].Body != "look at CI again" {
		t.Fatalf("items %+v", items)
	}
	waitIdle(t, a.Store, id)
	th, _ := a.Store.Thread(ctx, id)
	if th.Snoozed() || th.SnoozePrompt != "" {
		t.Fatalf("thread %+v", th)
	}
}

func TestImagePaths(t *testing.T) {
	text := "look at this\n\n[Attached image \"a b.png\" is saved at: /data/attachments/t/a b.png]\n[Attached file \"log.txt\" is saved at: /data/attachments/t/log.txt]\n[Attached image \"c.jpg\" is saved at: /x/c.jpg]"
	got := imagePaths(text)
	if len(got) != 2 || got[0] != "/data/attachments/t/a b.png" || got[1] != "/x/c.jpg" {
		t.Fatalf("got %q", got)
	}
}
