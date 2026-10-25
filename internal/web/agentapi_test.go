package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
	"starcode/internal/agent/fake"
	"starcode/internal/app"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/mcp"
	"starcode/internal/store"
)

// agentServer is a Server with the fake agent, project p1 and one thread
// in the yolo mode, for the agent API.
func agentServer(t *testing.T) (*Server, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "p"})
	a := app.New(st, bus.New(256), map[string]agent.Agent{"fake": &fake.Agent{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.Shutdown)
	id, err := a.CreateThread(ctx, "p1", "fake", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetThreadSettings(ctx, id, app.ThreadSettings{Agent: "fake", PermissionMode: "yolo"}); err != nil {
		t.Fatal(err)
	}
	return &Server{App: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, id
}

// call runs a tool as thread id over the handler.
func call(t *testing.T, s *Server, id, name string, args any) mcp.Result {
	t.Helper()
	b, _ := json.Marshal(mcp.Call{Name: name, Arguments: mustJSON(args)})
	r := httptest.NewRequest("POST", "/call", strings.NewReader(string(b)))
	r.Header.Set(mcp.HeaderThread, id)
	r.Header.Set(mcp.HeaderKey, s.AgentKey(id))
	w := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", name, w.Code, w.Body)
	}
	var res mcp.Result
	json.Unmarshal(w.Body.Bytes(), &res)
	return res
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestAgentAPIRejectsAnotherThreadsKey(t *testing.T) {
	s, id := agentServer(t)
	r := httptest.NewRequest("GET", "/tools", nil)
	r.Header.Set(mcp.HeaderThread, id)
	r.Header.Set(mcp.HeaderKey, s.AgentKey("someone-else"))
	w := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code %d", w.Code)
	}
}

func TestAgentToolsOnOwnThread(t *testing.T) {
	ctx := context.Background()
	s, id := agentServer(t)
	if r := call(t, s, id, "set_title", map[string]any{"title": "  Fix   the login  "}); r.IsError {
		t.Fatal(r.Text)
	}
	if r := call(t, s, id, "link_pr", map[string]any{"pr": "acme/web#42"}); r.IsError {
		t.Fatal(r.Text)
	}
	if r := call(t, s, id, "snooze", map[string]any{"minutes": 20, "wake_prompt": "check CI"}); r.IsError {
		t.Fatal(r.Text)
	}
	th, _ := s.App.Store.Thread(ctx, id)
	if th.Title != "Fix the login" || th.PRRepo != "acme/web" || th.PRNumber != 42 || !th.Snoozed() || th.SnoozePrompt != "check CI" {
		t.Fatalf("thread %+v", th)
	}
	info := call(t, s, id, "thread_info", nil)
	if !strings.Contains(info.Text, "https://github.com/acme/web/pull/42") {
		t.Fatalf("info:\n%s", info.Text)
	}
	if r := call(t, s, id, "link_pr", map[string]any{"pr": "nonsense"}); !r.IsError {
		t.Fatal("bad reference linked")
	}
	if r := call(t, s, id, "no_such_tool", nil); !r.IsError {
		t.Fatal("unknown tool ran")
	}
}

func TestSpawnAndWait(t *testing.T) {
	ctx := context.Background()
	s, id := agentServer(t)
	r := call(t, s, id, "spawn_thread", map[string]any{"prompt": "do the thing", "title": "helper", "wake_me": false})
	if r.IsError {
		t.Fatal(r.Text)
	}
	kids := s.App.Children(ctx, id)
	if len(kids) != 1 || kids[0].Title != "helper" || kids[0].PermissionMode != "yolo" || kids[0].WakeParent {
		t.Fatalf("children %+v", kids)
	}
	child := kids[0].ID
	w := call(t, s, id, "wait_thread", map[string]any{"id": child, "timeout_seconds": 10})
	if w.IsError || !strings.Contains(w.Text, "finished") {
		t.Fatalf("wait: %s", w.Text)
	}
	got := call(t, s, id, "read_thread", map[string]any{"id": child})
	if !strings.Contains(got.Text, "do the thing") {
		t.Fatalf("read:\n%s", got.Text)
	}
	// Only its own children and its parent are the thread's to message.
	other, _ := s.App.CreateThread(ctx, "p1", "fake", "")
	if r := call(t, s, id, "send_to_thread", map[string]any{"id": other, "prompt": "hi"}); !r.IsError {
		t.Fatal("sent to a stranger")
	}
	if r := call(t, s, id, "stop_thread", map[string]any{"id": other}); !r.IsError {
		t.Fatal("stopped a stranger")
	}
	if r := call(t, s, child, "send_to_thread", map[string]any{"id": id, "prompt": "done here"}); r.IsError {
		t.Fatal(r.Text)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, _ := s.App.Store.Items(ctx, id)
		found := false
		for _, it := range items {
			if it.Kind == domain.KindUser && strings.Contains(it.Body, "done here") {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the parent never got the child's message")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScheduleTools(t *testing.T) {
	ctx := context.Background()
	s, id := agentServer(t)
	if r := call(t, s, id, "schedule", map[string]any{"prompt": "check CI", "minutes": 30}); r.IsError {
		t.Fatal(r.Text)
	}
	if r := call(t, s, id, "schedule", map[string]any{"prompt": "daily report", "cron": "0 9 * * *", "new_thread": true, "title": "Report"}); r.IsError {
		t.Fatal(r.Text)
	}
	scs, _ := s.App.Store.Schedules(ctx, "")
	if len(scs) != 2 {
		t.Fatalf("schedules %+v", scs)
	}
	var cronID string
	for _, sc := range scs {
		if sc.Cron != "" {
			cronID = sc.ID
			if sc.ThreadID != "" || sc.By != id || sc.Mode != "yolo" || sc.Agent != "fake" {
				t.Fatalf("cron schedule %+v", sc)
			}
		} else if sc.ThreadID != id {
			t.Fatalf("one-off %+v", sc)
		}
	}
	if l := call(t, s, id, "list_schedules", nil); !strings.Contains(l.Text, cronID) || !strings.Contains(l.Text, "check CI") {
		t.Fatalf("list:\n%s", l.Text)
	}
	other, _ := s.App.CreateThread(ctx, "p1", "fake", "")
	if r := call(t, s, other, "cancel_schedule", map[string]any{"id": cronID}); !r.IsError {
		t.Fatal("a stranger cancelled it")
	}
	if r := call(t, s, id, "cancel_schedule", map[string]any{"id": cronID}); r.IsError {
		t.Fatal(r.Text)
	}
	if r := call(t, s, id, "schedule", map[string]any{"prompt": "x", "cron": "61 * * * *"}); !r.IsError {
		t.Fatal("bad cron taken")
	}
}

func TestThreadIDOf(t *testing.T) {
	for in, want := range map[string]string{
		"abc123":                               "abc123",
		" /threads/abc123 ":                    "abc123",
		"https://host:4000/threads/abc123?x=1": "abc123",
		"/threads/abc123/raw":                  "abc123",
	} {
		if got := threadIDOf(in); got != want {
			t.Errorf("threadIDOf(%q) = %q, want %q", in, got, want)
		}
	}
}
