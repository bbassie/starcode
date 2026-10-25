package app

import (
	"context"
	"encoding/json"
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
	"starcode/internal/gitx"
	"starcode/internal/store"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func prAt(head string, checks ...gitx.Check) gitx.PRDetail {
	d := gitx.PRDetail{State: "open", HeadSHA: head, Viewer: "me", Checks: checks, Mergeable: "MERGEABLE"}
	d.Number, d.URL, d.Base, d.Author = 7, "https://github.com/o/r/pull/7", "main", "me"
	return d
}

func kinds(cs []prChange) string {
	var k []string
	for _, c := range cs {
		k = append(k, c.kind)
	}
	return strings.Join(k, ",")
}

func TestEvaluatePRWatch(t *testing.T) {
	d := prAt("aaa1111", gitx.Check{Name: "test", Status: "pending"}, gitx.Check{Name: "lint", Status: "success"})
	w := prWatchBaseline(d, t0)
	if ch, _, _ := evaluatePRWatch(w, d); len(ch) != 0 {
		t.Fatalf("the baseline is news: %s", kinds(ch))
	}

	// A check fails: told once, not again on the next read.
	d.Checks[0].Status = "failure"
	ch, w2, _ := evaluatePRWatch(w, d)
	if kinds(ch) != "failed" || ch[0].checks[0].Name != "test" {
		t.Fatalf("failure: %s", kinds(ch))
	}
	if ch, _, _ := evaluatePRWatch(w2, d); len(ch) != 0 {
		t.Fatalf("the same failure again: %s", kinds(ch))
	}

	// A push and green checks: passed. Own comments do not count;
	// another's does, and so does a conflict.
	d = prAt("bbb2222", gitx.Check{Name: "test", Status: "success"}, gitx.Check{Name: "skip", Status: "skipped"})
	d.Remarks = []gitx.Remark{
		{ID: "r1", Author: "me", Body: "done", CreatedAt: t0.Add(time.Minute)},
		{ID: "r2", Author: "rev", Body: "<!-- bot -->Looks   good", Path: "a.go", CreatedAt: t0.Add(2 * time.Minute)},
	}
	d.Mergeable = "CONFLICTING"
	ch, w3, exhausted := evaluatePRWatch(w2, d)
	if kinds(ch) != "passed,remarks,conflict" || exhausted || len(ch[1].remarks) != 1 || ch[1].remarks[0].ID != "r2" {
		t.Fatalf("after push: %s %+v", kinds(ch), ch)
	}
	text := prWatchPrompt(d, ch, false)
	for _, want := range []string{"pull request #7", "All 1 check passed on bbb2222.", `rev on a.go: "Looks good"`, "conflicts with main"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, text)
		}
	}
	if ch, _, _ := evaluatePRWatch(w3, d); len(ch) != 0 {
		t.Fatalf("told twice: %s", kinds(ch))
	}

	// Comment-only wakes stop at the limit; a remark at the same second
	// as the last one told still counts once.
	w = w3
	for i := range prWatchWakeLimit {
		d.Remarks = append(d.Remarks, gitx.Remark{ID: "c" + string(rune('a'+i)), Author: "bot", Body: "hi", CreatedAt: t0.Add(2 * time.Minute)})
		var ex bool
		ch, w, ex = evaluatePRWatch(w, d)
		if kinds(ch) != "remarks" || len(ch[0].remarks) != 1 {
			t.Fatalf("wake %d: %s %+v", i, kinds(ch), ch)
		}
		if ex != (i == prWatchWakeLimit-1) {
			t.Fatalf("wake %d exhausted %v", i, ex)
		}
	}
}

// TestWatchPRWakesTheAgent runs the sweep against a stubbed PR: starting
// wakes nobody, a failed check sends the automatic prompt, and a merge
// ends the watch with a note.
func TestWatchPRWakesTheAgent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Append(ctx, "", domain.ProjectAdded{ID: "p1", Path: t.TempDir(), Name: "test"})
	a := New(st, bus.New(256), map[string]agent.Agent{"fake": &fake.Agent{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.Shutdown()
	id, _ := a.CreateThread(ctx, "p1", "fake", "")
	a.SetThreadSettings(ctx, id, ThreadSettings{Agent: "fake", PermissionMode: "yolo"})

	cur := prAt("aaa1111", gitx.Check{Name: "test", Status: "pending"})
	old := prDetail
	prDetail = func(context.Context, string, string, int) (gitx.PRDetail, error) { return cur, nil }
	defer func() { prDetail = old }()

	if err := a.StartPRWatch(ctx, id); err == nil {
		t.Fatal("watching without a linked PR")
	}
	a.LinkPR(ctx, id, "o/r", 7, cur.URL)
	if err := a.StartPRWatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	a.watchPRs(ctx, time.Now())
	if n := len(userItems(t, st, id)); n != 0 {
		t.Fatalf("starting the watch sent %d prompts", n)
	}

	cur = prAt("aaa1111", gitx.Check{Name: "test", Status: "failure", URL: "https://ci/1"})
	a.watchPRs(ctx, time.Now())
	us := userItems(t, st, id)
	if len(us) != 1 || !strings.Contains(us[0].Body, "Checks failed on aaa1111") || !strings.Contains(us[0].Body, "test https://ci/1") {
		t.Fatalf("prompts = %+v", us)
	}
	if pm := promptMeta(us[0]); pm.Auto != "prwatch" {
		t.Errorf("auto = %q", pm.Auto)
	}
	waitIdle(t, st, id)
	th, _ := st.Thread(ctx, id)
	var w PRWatch
	json.Unmarshal([]byte(th.PRWatch), &w)
	if len(w.Failed) != 1 {
		t.Errorf("state after the wake = %+v", w)
	}

	cur.State = "merged"
	a.watchPRs(ctx, time.Now())
	th, _ = st.Thread(ctx, id)
	if Watching(th) {
		t.Fatal("still watching a merged PR")
	}
	items, _ := st.Items(ctx, id)
	if last := items[len(items)-1]; !strings.Contains(last.Body, "Stopped watching pull request #7: it was merged") {
		t.Errorf("last item = %q", last.Body)
	}

	// Unlinking ends a watch too.
	cur.State = "open"
	a.StartPRWatch(ctx, id)
	a.UnlinkPR(ctx, id)
	if th, _ = st.Thread(ctx, id); th.PRWatch != "" {
		t.Error("unlink kept the watch")
	}
}
