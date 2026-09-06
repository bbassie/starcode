package views

import (
	"context"
	"strings"
	"testing"
	"time"

	"starcode/internal/gitx"
	"starcode/internal/store"
)

func TestPullRequestsRender(t *testing.T) {
	l := gitx.PRList{Repo: "o/r", RepoURL: "https://github.com/o/r", DefaultBranch: "main", Branch: "fix", PRs: []gitx.PR{
		{Number: 7, Title: "Fix the thing", URL: "https://github.com/o/r/pull/7", Head: "fix", Base: "main", Author: "bb", Review: "approved", Checks: "failure", UpdatedAt: time.Now()},
		{Number: 6, Title: "Draft work", URL: "https://github.com/o/r/pull/6", Head: "wip", Base: "main", Author: "cc", Draft: true, Checks: "pending", UpdatedAt: time.Now()},
	}}
	var b strings.Builder
	if err := PullRequests(PRData{Project: store.Project{ID: "p1"}, List: l, Loaded: true}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{`href="https://github.com/o/r/pull/7"`, "this branch", "#7 · bb · fix", `class="current"`, "draft", `title="Approved"`, `title="Checks failed"`, `title="Checks running"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in\n%s", want, html)
		}
	}
	// The current branch has a PR, so no compare link.
	if strings.Contains(html, "open a pull request") {
		t.Error("compare link shown although the branch has a PR")
	}
	l.Branch = "other"
	b.Reset()
	PullRequests(PRData{List: l, Loaded: true}).Render(context.Background(), &b)
	if !strings.Contains(b.String(), "https://github.com/o/r/compare/other?expand=1") {
		t.Error("compare link missing on a branch without a PR")
	}
}
