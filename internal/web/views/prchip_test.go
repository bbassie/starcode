package views

import (
	"context"
	"strings"
	"testing"
	"time"

	"starcode/internal/store"
)

func TestPRChipStates(t *testing.T) {
	for _, tc := range []struct {
		st   store.PRState
		word string
		icon string
	}{
		{store.PRState{PR: store.PR{Number: 1}}, "", "git-pull-request"},
		{store.PRState{PR: store.PR{Number: 1}, State: "open"}, "open", "git-pull-request"},
		{store.PRState{PR: store.PR{Number: 1}, State: "open", Draft: true}, "draft", "git-pull-request-draft"},
		{store.PRState{PR: store.PR{Number: 1}, State: "open", Draft: true, Review: "changes_requested"}, "changes requested", "message-square-x"},
		{store.PRState{PR: store.PR{Number: 1}, State: "open", Review: "approved"}, "approved", "check"},
		{store.PRState{PR: store.PR{Number: 1}, State: "open", Review: "rereview_requested"}, "re-review requested", "eye"},
		{store.PRState{PR: store.PR{Number: 1}, State: "merged", Review: "changes_requested"}, "merged", "git-merge"},
		{store.PRState{PR: store.PR{Number: 1}, State: "closed"}, "closed", "git-pull-request-closed"},
	} {
		if got := prWord(tc.st); got != tc.word {
			t.Errorf("prWord(%+v) = %q, want %q", tc.st, got, tc.word)
		}
		if got := prChipIcon(tc.st); got != tc.icon {
			t.Errorf("prChipIcon(%+v) = %q, want %q", tc.st, got, tc.icon)
		}
	}
	th := store.Thread{ID: "t1", Title: "T", Status: "idle", PRRepo: "o/r", PRNumber: 12, PRURL: "u"}
	var b strings.Builder
	ThreadRow(th, store.Project{Name: "p"}, false, false, AgentLook{}, false, prOf(th, map[store.PR]store.PRState{{Repo: "o/r", Number: 12}: {State: "merged", PR: store.PR{Repo: "o/r", Number: 12}, Title: "Merged one"}})).Render(context.Background(), &b)
	if html := b.String(); !strings.Contains(html, "#12") || !strings.Contains(html, "pr-chip merged") || !strings.Contains(html, "Merged one (merged)") {
		t.Errorf("row chip:\n%s", html)
	}
	// The row is a link to the thread; the chip opens GitHub instead and
	// must not be a second <a> inside it.
	if html := b.String(); !strings.Contains(html, `class="pr-chip-link"`) || !strings.Contains(html, `data-href="https://github.com/o/r/pull/12"`) || strings.Count(html, "<a ") != 1 {
		t.Errorf("row chip link:\n%s", html)
	}
	// Before the first poll or a stored URL, the link is built.
	if got := prURL(store.PRState{PR: store.PR{Repo: "o/r", Number: 3}}); got != "https://github.com/o/r/pull/3" {
		t.Errorf("prURL = %q", got)
	}
}

// The watch's next read shows as a countdown the page's clock keeps
// in the header.
func TestPRCheck(t *testing.T) {
	next := time.Now().Add(4*time.Minute + 10*time.Second)
	var b strings.Builder
	if err := PRCheck("t1", next).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{`id="pr-check"`, `/api/threads/t1/pr/check`, `data-until="` + next.UTC().Format(time.RFC3339) + `"`, `>4m</span>`} {
		if !strings.Contains(html, want) {
			t.Errorf("lacks %q:\n%s", want, html)
		}
	}
	if got := untilText(time.Now().Add(-time.Minute)); got != "now" {
		t.Errorf("past = %q", got)
	}
}
