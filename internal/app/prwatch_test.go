package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
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
	if e := evaluatePRWatch(w, d, t0); len(e.changes) != 0 {
		t.Fatalf("the baseline is news: %s", kinds(e.changes))
	}

	// A check fails: told once, not again on the next read.
	d.Checks[0].Status = "failure"
	e := evaluatePRWatch(w, d, t0)
	if kinds(e.changes) != "failed" || e.changes[0].checks[0].Name != "test" {
		t.Fatalf("failure: %s", kinds(e.changes))
	}
	if e := evaluatePRWatch(e.next, d, t0); len(e.changes) != 0 {
		t.Fatalf("the same failure again: %s", kinds(e.changes))
	}
	w2 := e.next

	// A push and green checks: passed, and a conflict. Own comments do
	// not count; another's plain comment rides along with the wake.
	now := t0.Add(3 * time.Minute)
	d = prAt("bbb2222", gitx.Check{Name: "test", Status: "success"}, gitx.Check{Name: "skip", Status: "skipped"})
	d.Remarks = []gitx.Remark{
		{ID: "r1", Author: "me", Body: "done", CreatedAt: t0.Add(time.Minute)},
		{ID: "r2", Author: "rev", Body: "<!-- bot -->Looks   good", Path: "a.go", CreatedAt: t0.Add(2 * time.Minute)},
	}
	d.Mergeable = "CONFLICTING"
	e = evaluatePRWatch(w2, d, now)
	if kinds(e.changes) != "passed,conflict,remarks" || e.exhausted || len(e.changes[2].remarks) != 1 || e.changes[2].remarks[0].ID != "r2" || len(e.noted) != 0 {
		t.Fatalf("after push: %s %+v", kinds(e.changes), e)
	}
	text := prWatchPrompt(d, e.changes, false)
	for _, want := range []string{"pull request #7", "Needs action:\n- The branch now conflicts with main.", "for context", "- All 1 check passed on bbb2222.", `- rev on a.go: "Looks good"`} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, text)
		}
	}
	if e := evaluatePRWatch(e.next, d, now); len(e.changes) != 0 || len(e.next.Held) != 0 {
		t.Fatalf("told twice: %s", kinds(e.changes))
	}

	// Mention-only wakes stop at the limit; a remark at the same second
	// as the last one told still counts once.
	w = e.next
	for i := range prWatchWakeLimit {
		now = now.Add(prWatchQuiet)
		d.Remarks = append(d.Remarks, gitx.Remark{ID: "c" + string(rune('a'+i)), Author: "bot", Body: "@me hi", CreatedAt: t0.Add(2 * time.Minute)})
		e = evaluatePRWatch(w, d, now)
		if len(e.changes) != 0 {
			t.Fatalf("wake %d before the quiet time: %s", i, kinds(e.changes))
		}
		now = now.Add(prWatchQuiet)
		e = evaluatePRWatch(e.next, d, now)
		if kinds(e.changes) != "remarks" || len(e.changes[0].remarks) != 1 {
			t.Fatalf("wake %d: %s %+v", i, kinds(e.changes), e.changes)
		}
		if e.exhausted != (i == prWatchWakeLimit-1) {
			t.Fatalf("wake %d exhausted %v", i, e.exhausted)
		}
		w = e.next
	}
}

// Only a request for action wakes the agent; the rest waits for the
// next prompt and shows in the transcript.
func TestPRWatchFilter(t *testing.T) {
	for _, c := range []struct {
		name  string
		r     gitx.Remark
		wakes bool
		noted bool
	}{
		{"plain comment", gitx.Remark{Author: "rev", Body: "nice work"}, false, true},
		{"changes requested", gitx.Remark{Author: "rev", State: "CHANGES_REQUESTED", Body: "please split this"}, true, false},
		{"changes requested, no text", gitx.Remark{Author: "rev", State: "CHANGES_REQUESTED"}, true, false},
		{"mention", gitx.Remark{Author: "Danielv123", Body: "@me can you rebase?"}, true, false},
		{"mention in other case, end of sentence", gitx.Remark{Author: "rev", Body: "thoughts, @ME."}, true, false},
		{"mention of someone else", gitx.Remark{Author: "rev", Body: "@meow and @someone-me, @me-too"}, false, true},
		{"email address", gitx.Remark{Author: "rev", Body: "mail x@me.org"}, false, true},
		{"approval", gitx.Remark{Author: "rev", State: "APPROVED"}, false, true},
		{"commented review", gitx.Remark{Author: "rev", State: "COMMENTED", Body: "a few nits"}, false, true},
		{"own comment", gitx.Remark{Author: "Me", Body: "@me note to self"}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := prAt("aaa1111")
			w := prWatchBaseline(d, t0)
			c.r.ID, c.r.CreatedAt = "x", t0.Add(time.Minute)
			d.Remarks = []gitx.Remark{c.r}
			e := evaluatePRWatch(w, d, t0.Add(10*time.Minute))
			// Seen a while ago: a second read is past the quiet time.
			e = evaluatePRWatch(e.next, d, t0.Add(10*time.Minute+prWatchQuiet))
			if woke := len(e.changes) > 0; woke != c.wakes {
				t.Fatalf("woke = %v: %+v", woke, e)
			}
			first := evaluatePRWatch(w, d, t0.Add(10*time.Minute))
			if noted := len(first.noted) > 0; noted != c.noted {
				t.Fatalf("noted = %v", noted)
			}
			if !c.wakes && c.noted && len(e.next.Held) != 1 {
				t.Fatalf("not held: %+v", e.next)
			}
		})
	}
}

// A remark that asks for action waits until the PR is quiet for
// prWatchQuiet, then wakes once with everything since; a failed check
// wakes at once and takes the pending remarks along.
func TestPRWatchSettles(t *testing.T) {
	d := prAt("aaa1111")
	w := prWatchBaseline(d, t0)
	now := t0.Add(10 * time.Minute)
	d.Remarks = []gitx.Remark{{ID: "m", Author: "rev", Body: "@me please look", CreatedAt: now.Add(-30 * time.Second)}}
	e := evaluatePRWatch(w, d, now)
	if len(e.changes) != 0 || len(e.next.Held) != 1 || len(e.noted) != 0 {
		t.Fatalf("a remark 30s old woke: %+v", e)
	}
	// 90s on: still within two minutes of the sweep that saw it, and a
	// plain comment arrives, which starts the wait over.
	now = now.Add(90 * time.Second)
	d.Remarks = append(d.Remarks, gitx.Remark{ID: "p", Author: "bob", Body: "+1", CreatedAt: now.Add(-10 * time.Second)})
	e = evaluatePRWatch(e.next, d, now)
	if len(e.changes) != 0 || len(e.next.Held) != 2 || len(e.noted) != 1 {
		t.Fatalf("woke while the PR moves: %+v", e)
	}
	now = now.Add(prWatchQuiet - time.Second)
	if e2 := evaluatePRWatch(e.next, d, now); len(e2.changes) != 0 {
		t.Fatalf("woke a second early: %+v", e2)
	}
	now = now.Add(time.Second)
	e = evaluatePRWatch(e.next, d, now)
	if kinds(e.changes) != "remarks" || len(e.changes[0].remarks) != 2 || len(e.next.Held) != 0 || e.next.Wakes != 1 {
		t.Fatalf("after the quiet time: %s %+v", kinds(e.changes), e.next)
	}
	text := prWatchPrompt(d, e.changes, false)
	if !strings.Contains(text, "Needs action:\n- rev mentioned you: \"@me please look\"") || !strings.Contains(text, `- bob: "+1"`) {
		t.Fatalf("prompt:\n%s", text)
	}
	if e := evaluatePRWatch(e.next, d, now.Add(time.Hour)); len(e.changes) != 0 {
		t.Fatalf("woke twice: %s", kinds(e.changes))
	}

	// A failed check does not wait, and brings the fresh mention along.
	w = e.next
	d.Remarks = append(d.Remarks, gitx.Remark{ID: "m2", Author: "rev", Body: "@me also this", CreatedAt: now})
	d.Checks = []gitx.Check{{Name: "test", Status: "failure"}}
	e = evaluatePRWatch(w, d, now)
	if kinds(e.changes) != "failed,remarks" || e.changes[1].remarks[0].ID != "m2" || e.next.Wakes != 0 {
		t.Fatalf("failed check: %s %+v", kinds(e.changes), e.next)
	}
}

func newWatchApp(t *testing.T, cur *gitx.PRDetail) (*App, *store.Store, *freshAgent, string) {
	t.Helper()
	ctx := context.Background()
	a, st, ag := newFreshApp(t, "")
	old := prDetail
	prDetail = func(context.Context, string, string, int) (gitx.PRDetail, error) { return *cur, nil }
	t.Cleanup(func() { a.bg.Wait(); prDetail = old })
	id, _ := a.CreateThread(ctx, "p1", "fresh-test", "")
	return a, st, ag, id
}

func watchOf(t *testing.T, st *store.Store, id string) (store.Thread, PRWatch) {
	t.Helper()
	th, err := st.Thread(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var w PRWatch
	if th.PRWatch != "" {
		json.Unmarshal([]byte(th.PRWatch), &w)
	}
	return th, w
}

// Linking starts the watch; a stop by hand sticks, through the sweep and
// a new link of the same PR, until another PR is linked; with the
// setting off a link watches nothing.
func TestPRWatchStartsOnLink(t *testing.T) {
	ctx := context.Background()
	cur := prAt("aaa1111")
	a, st, _, id := newWatchApp(t, &cur)

	a.LinkPR(ctx, id, "o/r", 7, cur.URL)
	a.bg.Wait()
	if th, _ := watchOf(t, st, id); !Watching(th) {
		t.Fatal("a link did not start the watch")
	}
	if err := a.StopPRWatch(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	st.SavePRStates(ctx, []store.PRState{{PR: store.PR{Repo: "o/r", Number: 7}, State: "open"}})
	a.watchPRs(ctx, time.Now())
	a.LinkPR(ctx, id, "o/r", 7, cur.URL)
	a.bg.Wait()
	if th, _ := watchOf(t, st, id); Watching(th) || !th.PRWatchOff {
		t.Fatal("the watch came back after a stop by hand")
	}
	a.LinkPR(ctx, id, "o/r", 8, "")
	a.bg.Wait()
	if th, _ := watchOf(t, st, id); !Watching(th) || th.PRWatchOff {
		t.Fatal("another PR was not watched")
	}
	// The agent stopping it sticks too, even with no watch running.
	a.StopPRWatch(ctx, id, StopByAgent)
	a.StopPRWatch(ctx, id, StopByAgent)
	if th, _ := watchOf(t, st, id); Watching(th) || !th.PRWatchOff {
		t.Fatal("the agent's stop did not stick")
	}
	// Starting it by hand clears the mark.
	if err := a.StartPRWatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if th, _ := watchOf(t, st, id); !Watching(th) || th.PRWatchOff {
		t.Fatal("a start by hand left the mark")
	}

	// A thread linked before the setting existed: the sweep starts it.
	id2, _ := a.CreateThread(ctx, "p1", "fresh-test", "")
	st.SetSetting(ctx, "watch_linked_prs", "0")
	a.LinkPR(ctx, id2, "o/r", 7, cur.URL)
	a.bg.Wait()
	a.watchPRs(ctx, time.Now())
	if th, _ := watchOf(t, st, id2); Watching(th) {
		t.Fatal("watching with the setting off")
	}
	st.SetSetting(ctx, "watch_linked_prs", "1")
	a.watchPRs(ctx, time.Now())
	if th, _ := watchOf(t, st, id2); !Watching(th) {
		t.Fatal("the sweep did not start the watch")
	}
}

// A plain comment wakes nobody: it shows as a quiet line and goes ahead
// of the user's next prompt, once.
func TestPRWatchHeldRidesWithNextPrompt(t *testing.T) {
	ctx := context.Background()
	cur := prAt("aaa1111")
	a, st, ag, id := newWatchApp(t, &cur)
	a.LinkPR(ctx, id, "o/r", 7, cur.URL)
	a.bg.Wait()

	cur.Remarks = []gitx.Remark{{ID: "c1", Author: "rev", Body: "Nice, thanks", URL: "https://c/1", CreatedAt: time.Now().Add(time.Second)}}
	a.watchPRs(ctx, time.Now())
	if us := userItems(t, st, id); len(us) != 0 {
		t.Fatalf("a plain comment woke the agent: %+v", us)
	}
	notes := systemNotes(t, st, id)
	if len(notes) != 1 || !strings.Contains(notes[0], `Pull request #7: rev: "Nice, thanks"`) {
		t.Fatalf("notes = %q", notes)
	}
	a.watchPRs(ctx, time.Now().Add(time.Hour))
	if n := systemNotes(t, st, id); len(n) != 1 {
		t.Fatalf("noted twice: %q", n)
	}

	if err := a.SendPrompt(ctx, id, "next"); err != nil {
		t.Fatal(err)
	}
	got := receivePrompt(t, ag.session(0).sent)
	if !strings.HasPrefix(got, "[starcode] New on pull request #7") || !strings.Contains(got, `- rev: "Nice, thanks" https://c/1`) || !strings.HasSuffix(got, "next") {
		t.Fatalf("prompt:\n%s", got)
	}
	if _, w := watchOf(t, st, id); len(w.Held) != 0 || !slices.Contains(w.Seen, "c1") {
		t.Fatalf("not marked told: %+v", w)
	}
	finishTurn(t, st, id, ag.session(0), "a1")
	a.watchPRs(ctx, time.Now().Add(time.Hour))
	if err := a.SendPrompt(ctx, id, "third"); err != nil {
		t.Fatal(err)
	}
	if got := receivePrompt(t, ag.session(0).sent); got != "third" {
		t.Fatalf("held twice: %q", got)
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
	a.bg.Wait()
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
