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
	if err := PRDetailView(PRDetailData{Project: store.Project{ID: "p1"}, ProjectID: "p1", Base: "/api/projects/p1/prs/9", Detail: d, Loaded: true}).Render(context.Background(), &b); err != nil {
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
	PRDetailView(PRDetailData{Project: store.Project{ID: "p1"}, ProjectID: "p1", Base: "/api/projects/p1/prs/9", Detail: clean, Loaded: true}).Render(context.Background(), &b)
	if strings.Contains(b.String(), "Needs work") || !strings.Contains(b.String(), "ready to merge") {
		t.Errorf("clean PR rendered wrong:\n%s", b.String())
	}
}

func TestPRsPageRender(t *testing.T) {
	mine := gitx.MyPullRequests{Login: "bb",
		Assigned: []gitx.MyPR{{PR: gitx.PR{Number: 3, Title: "Assigned one", URL: "https://github.com/o/r/pull/3", Head: "a", Base: "main", Author: "cc", Review: "changes_requested", Checks: "failure", Comments: 4, UpdatedAt: time.Now()}, Repo: "o/r", State: "open"}},
		Authored: []gitx.MyPR{{PR: gitx.PR{Number: 5, Title: "Elsewhere", URL: "https://github.com/x/y/pull/5", Head: "b", Base: "main", Author: "bb", UpdatedAt: time.Now()}, Repo: "x/y", State: "open"}},
	}
	d := PRsPageData{Mine: mine, Loaded: true, At: time.Now(),
		Repos:   map[string]store.Project{"o/r": {ID: "p1", Name: "r"}},
		Threads: map[store.PR][]store.Thread{{Repo: "o/r", Number: 3}: {{ID: "t1", Title: "Fix the review", Status: "idle"}}},
	}
	var b strings.Builder
	if err := PRsPage(d).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{"Assigned to you", "Waiting for your review", "Opened by you", "changes requested", "checks failed", `href="/threads/t1"`, "Fix the review", `/api/prs/o/r/3/thread?project=p1`, `@get(&#39;/api/prs/o/r/3&#39;)`, "https://github.com/x/y/pull/5", "none.", `title="4 comments"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	b.Reset()
	PRsPage(PRsPageData{Err: "the GitHub CLI (gh) is not installed", Loaded: true}).Render(context.Background(), &b)
	if !strings.Contains(b.String(), "gh) is not installed") {
		t.Error("error not shown")
	}
}

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
}

func TestPRDetailPageActions(t *testing.T) {
	det := gitx.PRDetail{PR: gitx.PR{Number: 4, Title: "P", URL: "https://github.com/o/r/pull/4", Head: "h", Base: "main", Review: "changes_requested"},
		Reviews: []gitx.Review{{Author: "cool", State: "CHANGES_REQUESTED", Body: "fix it", URL: "https://github.com/o/r/pull/4#pullrequestreview-7"}}}
	// Known project: the thread posts straight to it, no draft button on a page.
	d := PRDetailData{Detail: det, Loaded: true, Page: true, Repo: "o/r", Base: "/api/prs/o/r/4", ProjectID: "p1", Project: store.Project{ID: "p1"}, Projects: []store.Project{{ID: "p1", Name: "r"}}}
	var b strings.Builder
	PRPageDetail(d).Render(context.Background(), &b)
	html := b.String()
	for _, want := range []string{`@post(&#39;/api/prs/o/r/4/thread?project=p1&#39;)`, `@post(&#39;/api/prs/o/r/4/thread?focus=https://github.com/o/r/pull/4%23pullrequestreview-7&amp;project=p1&#39;)`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "/draft") || strings.Contains(html, "pr-pick") {
		t.Error("page shows a draft button or a picker it should not")
	}
	// Unknown repository: a picker, and the thread action reads it.
	d.ProjectID, d.Project = "", store.Project{}
	b.Reset()
	PRPageDetail(d).Render(context.Background(), &b)
	html = b.String()
	if !strings.Contains(html, "pr-pick") || !strings.Contains(html, `project=&#39; + $prproject)`) {
		t.Errorf("picker missing:\n%s", html)
	}
	// The panel: draft buttons, no picker, project routes.
	pd := PRDetailData{Detail: det, Loaded: true, Repo: "o/r", Base: "/api/projects/p1/prs/4", ProjectID: "p1", Project: store.Project{ID: "p1"}, ThreadID: "t1"}
	b.Reset()
	PRDetailView(pd).Render(context.Background(), &b)
	html = b.String()
	for _, want := range []string{`@post(&#39;/api/projects/p1/prs/4/thread&#39;)`, `@post(&#39;/api/projects/p1/prs/4/draft?focus=`, "link to thread"} {
		if !strings.Contains(html, want) {
			t.Errorf("panel missing %q", want)
		}
	}
}

// The review card offers merge on an open PR and nothing on a merged one.
func TestPRReviewCard(t *testing.T) {
	open := gitx.PRDetail{PR: gitx.PR{Number: 9, Base: "main"}, State: "open", MergeState: "BEHIND"}
	var b strings.Builder
	PRDetailView(PRDetailData{Project: store.Project{ID: "p1"}, ProjectID: "p1", Base: "/api/projects/p1/prs/9", Detail: open, Loaded: true}).Render(context.Background(), &b)
	for _, want := range []string{"prs/9/merge", "prs/9/approve", "prs/9/request-changes", "prs/9/comment", "prs/9/update-branch"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("open PR card lacks %s", want)
		}
	}
	b.Reset()
	merged := gitx.PRDetail{PR: gitx.PR{Number: 9, Base: "main"}, State: "merged"}
	PRDetailView(PRDetailData{Project: store.Project{ID: "p1"}, ProjectID: "p1", Base: "/api/projects/p1/prs/9", Detail: merged, Loaded: true}).Render(context.Background(), &b)
	if strings.Contains(b.String(), "prs/9/merge") || strings.Contains(b.String(), "prs/9/approve") {
		t.Error("merged PR card still offers merge or approve")
	}
}
