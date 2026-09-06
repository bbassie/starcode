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
	for _, want := range []string{`@get(&#39;/api/projects/p1/prs/7&#39;)`, "this branch", "#7 · bb · fix", `class="current"`, "draft", `title="Approved"`, `title="Checks failed"`, `title="Checks running"`} {
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

func TestPRDetailRender(t *testing.T) {
	d := gitx.PRDetail{
		PR:        gitx.PR{Number: 9, Title: "Cap the panes", URL: "https://github.com/o/r/pull/9", Head: "panes", Base: "main", Author: "bb", Review: "changes_requested", Checks: "failure", UpdatedAt: time.Now()},
		Additions: 10, Deletions: 2, ChangedFiles: 1, Mergeable: "CONFLICTING", MergeState: "DIRTY", Labels: []string{"ui"},
		Reviews: []gitx.Review{{Author: "rev", State: "CHANGES_REQUESTED", Body: "Please **cap** it.", URL: "r1", SubmittedAt: time.Now()}},
		Threads: []gitx.ReviewThread{
			{Path: "term.js", Line: 40, Comments: []gitx.ReviewComment{{Author: "rev", Body: "No upper bound.", CreatedAt: time.Now()}}},
			{Path: "term.go", Line: 3, Resolved: true, Comments: []gitx.ReviewComment{{Author: "rev", Body: "nit", CreatedAt: time.Now()}}},
		},
		Checks: []gitx.Check{{Name: "test", Status: "failure", URL: "https://ci/1"}, {Name: "lint", Status: "success"}},
	}
	var b strings.Builder
	if err := PRDetailView(PRDetailData{Project: store.Project{ID: "p1"}, Detail: d, Loaded: true}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{"Cap the panes", "changes requested", "merge conflicts", "+10", "−2", "Needs work", "1 reviewer asking for changes · 1 open comment · 1 failing check · merge conflicts",
		"<strong>cap</strong>", "term.js:40", "No upper bound.", `href="https://ci/1"`, "/api/projects/p1/prs/9/thread", "/api/projects/p1/prs/9/draft", "1 resolved comment", "2 checks", "1 failed, 1 passed"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in\n%s", want, html)
		}
	}
	// A clean, approved PR has no Needs work block.
	clean := gitx.PRDetail{PR: gitx.PR{Number: 1, Title: "ok", Review: "approved", Checks: "success"}, MergeState: "CLEAN"}
	b.Reset()
	PRDetailView(PRDetailData{Project: store.Project{ID: "p1"}, Detail: clean, Loaded: true}).Render(context.Background(), &b)
	if strings.Contains(b.String(), "Needs work") || !strings.Contains(b.String(), "ready to merge") {
		t.Errorf("clean PR rendered wrong:\n%s", b.String())
	}
}
