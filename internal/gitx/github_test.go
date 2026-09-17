package gitx

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodePRs(t *testing.T) {
	raw := []byte(`[
	 {"number": 12, "title": "Add panes", "url": "https://github.com/o/r/pull/12", "headRefName": "panes", "baseRefName": "main",
	  "author": {"login": "bb"}, "isDraft": true, "reviewDecision": "REVIEW_REQUIRED", "updatedAt": "2026-09-06T05:00:00Z",
	  "statusCheckRollup": [
	    {"status": "COMPLETED", "conclusion": "SUCCESS"},
	    {"status": "IN_PROGRESS", "conclusion": ""},
	    {"state": "SUCCESS"}
	  ]},
	 {"number": 11, "title": "Fix", "url": "u", "headRefName": "fix", "baseRefName": "main", "author": {"login": "x"},
	  "statusCheckRollup": [{"status": "COMPLETED", "conclusion": "SUCCESS"}, {"status": "COMPLETED", "conclusion": "FAILURE"}]},
	 {"number": 10, "title": "Nothing", "url": "u", "headRefName": "n", "baseRefName": "main", "author": {"login": "x"}, "statusCheckRollup": []}
	]`)
	prs, err := decodePRs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 3 {
		t.Fatalf("prs = %+v", prs)
	}
	if p := prs[0]; p.Number != 12 || !p.Draft || p.Review != "review_required" || p.Checks != "pending" || p.Author != "bb" || p.UpdatedAt.IsZero() {
		t.Errorf("first = %+v", p)
	}
	if prs[1].Checks != "failure" || prs[2].Checks != "" {
		t.Errorf("checks = %q, %q", prs[1].Checks, prs[2].Checks)
	}

	l := PRList{RepoURL: "https://github.com/o/r", DefaultBranch: "main", Branch: "fix", PRs: prs}
	if cur, ok := l.Current(); !ok || cur.Number != 11 {
		t.Errorf("current = %+v, %v", cur, ok)
	}
	if got := l.CompareURL(); got != "https://github.com/o/r/compare/fix?expand=1" {
		t.Errorf("compare = %q", got)
	}
	l.Branch = "main"
	if got := l.CompareURL(); got != "" {
		t.Errorf("compare on default branch = %q", got)
	}
}

func TestDecodePRDetailAndFixPrompt(t *testing.T) {
	raw := []byte(`{"data":{"repository":{"pullRequest":{
	  "number": 7, "title": "Add panes", "url": "https://github.com/o/r/pull/7", "headRefName": "panes", "baseRefName": "main",
	  "isDraft": false, "reviewDecision": "CHANGES_REQUESTED", "updatedAt": "2026-09-06T05:00:00Z", "body": "Splits the terminal.",
	  "additions": 120, "deletions": 8, "changedFiles": 3, "mergeable": "CONFLICTING", "mergeStateStatus": "DIRTY",
	  "author": {"login": "bb"}, "labels": {"nodes": [{"name": "ui"}]},
	  "reviews": {"nodes": [
	    {"author": {"login": "bb"}, "state": "COMMENTED", "body": "self note", "url": "u0", "submittedAt": "2026-09-05T00:00:00Z"},
	    {"author": {"login": "rev"}, "state": "CHANGES_REQUESTED", "body": "Please cap the panes.", "url": "u1", "submittedAt": "2026-09-05T01:00:00Z"},
	    {"author": {"login": "rev"}, "state": "COMMENTED", "body": "ping", "url": "u2", "submittedAt": "2026-09-05T02:00:00Z"},
	    {"author": {"login": "other"}, "state": "APPROVED", "body": "", "url": "u3", "submittedAt": "2026-09-05T03:00:00Z"}
	  ]},
	  "reviewThreads": {"nodes": [
	    {"isResolved": false, "isOutdated": false, "path": "term.js", "line": 40, "comments": {"nodes": [
	      {"author": {"login": "rev"}, "body": "No upper bound here.", "url": "c1", "createdAt": "2026-09-05T01:00:00Z"},
	      {"author": {"login": "bb"}, "body": "Will fix.", "url": "c2", "createdAt": "2026-09-05T01:10:00Z"}]}},
	    {"isResolved": true, "isOutdated": true, "path": "term.go", "line": null, "originalLine": 12, "comments": {"nodes": [
	      {"author": {"login": "rev"}, "body": "nit", "url": "c3", "createdAt": "2026-09-05T01:00:00Z"}]}}
	  ]},
	  "commits": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [
	    {"__typename": "CheckRun", "name": "test", "status": "COMPLETED", "conclusion": "FAILURE", "detailsUrl": "https://ci/1"},
	    {"__typename": "CheckRun", "name": "lint", "status": "COMPLETED", "conclusion": "SKIPPED", "detailsUrl": ""},
	    {"__typename": "StatusContext", "context": "deploy", "state": "PENDING", "targetUrl": "https://ci/2"}
	  ]}}}}]}
	}}}}`)
	d, err := decodePRDetail(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.Number != 7 || d.Author != "bb" || d.Additions != 120 || d.Labels[0] != "ui" || d.Review != "changes_requested" {
		t.Errorf("detail = %+v", d.PR)
	}
	// The author's own review is dropped; rev's later comment does not
	// replace the verdict; other's approval stays.
	if len(d.Reviews) != 2 || d.Reviews[0].Author != "rev" || d.Reviews[0].State != "CHANGES_REQUESTED" || d.Reviews[1].State != "APPROVED" {
		t.Errorf("reviews = %+v", d.Reviews)
	}
	if got := d.Unresolved(); len(got) != 1 || got[0].Path != "term.js" || got[0].Line != 40 || len(got[0].Comments) != 2 {
		t.Errorf("unresolved = %+v", got)
	}
	if d.Threads[1].Line != 12 || !d.Threads[1].Resolved {
		t.Errorf("resolved thread = %+v", d.Threads[1])
	}
	if len(d.Checks) != 3 || d.Checks[0].Status != "failure" || d.Checks[1].Status != "skipped" || d.Checks[2].Status != "pending" || d.PR.Checks != "failure" {
		t.Errorf("checks = %+v, rollup %q", d.Checks, d.PR.Checks)
	}
	prompt := d.FixPrompt()
	for _, want := range []string{"pull request #7", "`panes`", "@rev:\n    Please cap the panes.", "1. term.js:40", "@rev: No upper bound here.", "- test (https://ci/1)", "merge conflicts with `main`"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "deploy") || strings.Contains(prompt, "nit") {
		t.Errorf("prompt includes passing or resolved items:\n%s", prompt)
	}
}

func TestFindPullURL(t *testing.T) {
	repo, n, url, ok := FindPullURL("Creating pull request for fix into main in o/r\n\nhttps://github.com/o/r/pull/42\n")
	if !ok || repo != "o/r" || n != 42 || url != "https://github.com/o/r/pull/42" {
		t.Fatalf("got %q %d %q %v", repo, n, url, ok)
	}
	if _, _, _, ok := FindPullURL("https://github.com/o/r/issues/42"); ok {
		t.Error("an issue URL is not a pull request")
	}
	if _, n, _, _ := FindPullURL("see https://github.com/o/r/pull/7/files"); n != 7 {
		t.Errorf("number from a files URL = %d, want 7", n)
	}
}

func TestRepoFromRemote(t *testing.T) {
	for remote, want := range map[string]string{
		"git@github.com:owner/name.git":       "owner/name",
		"https://github.com/owner/name":       "owner/name",
		"https://github.com/owner/name.git":   "owner/name",
		"ssh://git@github.com/owner/name.git": "owner/name",
		"https://github.com/owner/na.me/":     "owner/na.me",
		"https://gitlab.com/owner/name.git":   "",
		"git@github.com:owner/name.git\n":     "",
		"https://github.com/owner":            "",
	} {
		if got := RepoFromRemote(remote); got != want {
			t.Errorf("RepoFromRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestDecodeMyPullRequests(t *testing.T) {
	raw := []byte(`{"data":{"viewer":{"login":"bb"},
	 "assigned":{"nodes":[{"number":3,"title":"A","url":"https://github.com/o/r/pull/3","state":"OPEN","isDraft":false,"reviewDecision":"CHANGES_REQUESTED","headRefName":"a","baseRefName":"main","updatedAt":"2026-09-01T10:00:00Z","totalCommentsCount":7,"author":{"login":"bb"},"repository":{"nameWithOwner":"o/r"},"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE"}}}]}}]},
	 "requested":{"nodes":[{}]},
	 "authored":{"nodes":[{"number":4,"title":"B","url":"https://github.com/o/r/pull/4","state":"OPEN","isDraft":true,"reviewDecision":null,"headRefName":"b","baseRefName":"main","updatedAt":"2026-09-02T10:00:00Z","author":{"login":"bb"},"repository":{"nameWithOwner":"o/r"},"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}]}}}`)
	got, err := decodeMyPullRequests(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Login != "bb" || len(got.Assigned) != 1 || len(got.ReviewRequested) != 0 || len(got.Authored) != 1 {
		t.Fatalf("got %+v", got)
	}
	a := got.Assigned[0]
	if a.Repo != "o/r" || a.Number != 3 || a.Review != "changes_requested" || a.Checks != "failure" || a.State != "open" || a.Head != "a" || a.Comments != 7 {
		t.Errorf("assigned row = %+v", a)
	}
	if b := got.Authored[0]; !b.Draft || b.Checks != "" || b.Review != "" {
		t.Errorf("authored row = %+v", b)
	}
	if _, err := decodeMyPullRequests([]byte(`{"data":{"viewer":{"login":""}},"errors":[{"message":"bad credentials"}]}`)); err == nil || err.Error() != "bad credentials" {
		t.Errorf("error not surfaced: %v", err)
	}
}

func TestPRNodeState(t *testing.T) {
	var n prNode
	if err := json.Unmarshal([]byte(`{"number":9,"title":"T","url":"u","state":"MERGED","isDraft":false,"reviewDecision":"APPROVED","headRefName":"h","repository":{"nameWithOwner":"o/r"},"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}}`), &n); err != nil {
		t.Fatal(err)
	}
	st := n.state()
	if st.State != "merged" || st.Review != "approved" || st.Checks != "success" || st.Repo != "o/r" || st.Number != 9 || st.Head != "h" {
		t.Errorf("state = %+v", st)
	}
}

func TestFixPromptWithoutWork(t *testing.T) {
	d := PRDetail{PR: PR{Number: 5, Title: "Quiet", URL: "u", Head: "h", Base: "main"}, Mergeable: "MERGEABLE"}
	if d.NeedsWork() {
		t.Fatal("nothing blocks this PR")
	}
	p := d.FixPrompt()
	if !strings.HasPrefix(p, "Work on pull request #5") || strings.Contains(p, "Work through each point") {
		t.Errorf("prompt for a clean PR:\n%s", p)
	}
}

func TestReviewWord(t *testing.T) {
	for _, tc := range []struct {
		decision            string
		blocking, requested []string
		want                string
	}{
		{"CHANGES_REQUESTED", []string{"a"}, nil, "changes_requested"},
		{"CHANGES_REQUESTED", []string{"a"}, []string{"a"}, "rereview_requested"},
		{"CHANGES_REQUESTED", []string{"a", "b"}, []string{"a"}, "changes_requested"},
		{"CHANGES_REQUESTED", []string{"a", "b"}, []string{"a", "b"}, "rereview_requested"},
		// gh pr list's latestReviews drops a re-requested reviewer, so the
		// blocking list is empty while the decision stands.
		{"CHANGES_REQUESTED", nil, []string{"a"}, "rereview_requested"},
		{"CHANGES_REQUESTED", nil, nil, "changes_requested"},
		{"APPROVED", nil, []string{"a"}, "approved"},
		{"", nil, nil, ""},
	} {
		if got := reviewWord(tc.decision, tc.blocking, tc.requested); got != tc.want {
			t.Errorf("reviewWord(%q, %v, %v) = %q, want %q", tc.decision, tc.blocking, tc.requested, got, tc.want)
		}
	}
}

func TestDetailReReview(t *testing.T) {
	raw := []byte(`{"data":{"repository":{"pullRequest":{"number":990,"title":"Sync","url":"u","headRefName":"fix","baseRefName":"master","reviewDecision":"CHANGES_REQUESTED","author":{"login":"bb"},
	 "reviewRequests":{"nodes":[{"requestedReviewer":{"login":"cool"}}]},
	 "reviews":{"nodes":[{"author":{"login":"cool"},"state":"CHANGES_REQUESTED","body":"sync all of them","url":"r","submittedAt":"2026-09-06T01:08:34Z"}]},
	 "reviewThreads":{"nodes":[]},"commits":{"nodes":[]}}}}}`)
	d, err := decodePRDetail(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.Review != "rereview_requested" || len(d.ChangesRequested()) != 0 || len(d.AwaitingReReview()) != 1 || d.NeedsWork() {
		t.Fatalf("detail = %+v, changes %d, awaiting %d", d.PR, len(d.ChangesRequested()), len(d.AwaitingReReview()))
	}
	if p := d.FixPrompt(); !strings.HasPrefix(p, "Work on pull request #990") {
		t.Errorf("a PR waiting on its reviewer got the fix prompt:\n%s", p)
	}
	// One blocking reviewer re-requested, another not: still changes requested.
	raw = []byte(strings.Replace(string(raw), `"reviews":{"nodes":[`, `"reviews":{"nodes":[{"author":{"login":"other"},"state":"CHANGES_REQUESTED","body":"no","url":"r2","submittedAt":"2026-09-06T02:00:00Z"},`, 1))
	d, _ = decodePRDetail(raw)
	if d.Review != "changes_requested" || len(d.ChangesRequested()) != 1 || !d.NeedsWork() {
		t.Errorf("mixed reviewers: review %q, changes %d", d.Review, len(d.ChangesRequested()))
	}
	if p := d.FixPrompt(); !strings.Contains(p, "@cool asked for changes earlier") || !strings.Contains(p, "@other:") {
		t.Errorf("prompt:\n%s", p)
	}
}

func TestReposFromRemoteList(t *testing.T) {
	out := "origin\tgit@github.com:bb/clusterio.git (fetch)\norigin\tgit@github.com:bb/clusterio.git (push)\nupstream\thttps://github.com/clusterio/clusterio (fetch)\nupstream\thttps://github.com/clusterio/clusterio (push)\nlab\thttps://gitlab.com/x/y.git (fetch)\n"
	got := reposFromRemoteList(out)
	if len(got) != 2 || got[0] != "bb/clusterio" || got[1] != "clusterio/clusterio" {
		t.Errorf("got %v", got)
	}
}

func TestFocusPrompt(t *testing.T) {
	d := PRDetail{PR: PR{Number: 9, Title: "T", URL: "u", Head: "h", Base: "main"},
		Reviews: []Review{{Author: "cool", State: "CHANGES_REQUESTED", Body: "sync all of them", URL: "https://github.com/o/r/pull/9#pullrequestreview-1"}},
		Threads: []ReviewThread{{Path: "a.go", Line: 3, Comments: []ReviewComment{{Author: "cool", Body: "rename this", URL: "https://github.com/o/r/pull/9#discussion_r1"}, {Author: "bb", Body: "why?", URL: "https://github.com/o/r/pull/9#discussion_r2"}}}},
		Checks:  []Check{{Name: "ci", Status: "failure"}},
	}
	p := d.FocusPrompt("https://github.com/o/r/pull/9#pullrequestreview-1")
	if !strings.Contains(p, "@cool changes requested") || !strings.Contains(p, "sync all of them") || strings.Contains(p, "rename this") || strings.Contains(p, "Failing checks") {
		t.Errorf("review prompt:\n%s", p)
	}
	p = d.FocusPrompt("https://github.com/o/r/pull/9#discussion_r1")
	if !strings.Contains(p, "Comment thread on a.go:3") || !strings.Contains(p, "@cool: rename this") || !strings.Contains(p, "@bb: why?") || strings.Contains(p, "sync all") {
		t.Errorf("thread prompt:\n%s", p)
	}
	if p = d.FocusPrompt("nope"); p != d.FixPrompt() {
		t.Error("unknown focus should give the whole prompt")
	}
}

func TestPRActionArgs(t *testing.T) {
	tests := []struct {
		name   string
		action string
		body   string
		method string
		auto   bool
		want   string // the gh arguments joined by spaces, or "" for an error
	}{
		{name: "approve", action: "approve", want: "pr review -R o/r 7 --approve"},
		{name: "approve with a word", action: "approve", body: " LGTM ", want: "pr review -R o/r 7 --approve -b LGTM"},
		{name: "request changes", action: "request-changes", body: "cap the panes", want: "pr review -R o/r 7 --request-changes -b cap the panes"},
		{name: "request changes needs a body", action: "request-changes", body: "  "},
		{name: "comment", action: "comment", body: "thanks", want: "pr comment -R o/r 7 -b thanks"},
		{name: "comment needs a body", action: "comment"},
		{name: "merge defaults to squash", action: "merge", want: "pr merge -R o/r 7 --squash"},
		{name: "rebase when checks pass", action: "merge", method: "rebase", auto: true, want: "pr merge -R o/r 7 --rebase --auto"},
		{name: "merge commit", action: "merge", method: "merge", want: "pr merge -R o/r 7 --merge"},
		{name: "no other merge method", action: "merge", method: "delete-branch"},
		{name: "update branch", action: "update-branch", want: "pr update-branch -R o/r 7"},
		{name: "ready", action: "ready", want: "pr ready -R o/r 7"},
		{name: "close", action: "close", want: "pr close -R o/r 7"},
		{name: "unknown", action: "force-push"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, err := prActionArgs(tc.action, "o/r", 7, tc.body, tc.method, tc.auto)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("args = %q, want an error", args)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(args, " "); got != tc.want {
				t.Errorf("args = %q, want %q", got, tc.want)
			}
			for _, a := range args {
				if strings.Contains(a, "delete-branch") || strings.Contains(a, "force") {
					t.Errorf("args %q touch branches", args)
				}
			}
		})
	}
}

func TestPRDetailStateAndMergeNote(t *testing.T) {
	raw := []byte(`{"data":{"repository":{"pullRequest":{"number": 3, "title": "t", "state": "MERGED", "mergeStateStatus": "BLOCKED", "author": {"login": "bb"}}}}}`)
	d, err := decodePRDetail(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "merged" || d.Open() {
		t.Errorf("state = %q, open %v", d.State, d.Open())
	}
	if got := d.MergeStateNote(); got != "blocked by branch rules or reviews" {
		t.Errorf("note = %q", got)
	}
	for st, want := range map[string]string{"CLEAN": "", "BEHIND": "behind the base branch", "DIRTY": "has conflicts", "UNSTABLE": "checks failing", "UNKNOWN": ""} {
		if got := (PRDetail{MergeState: st}).MergeStateNote(); got != want {
			t.Errorf("note for %s = %q, want %q", st, got, want)
		}
	}
	if !(PRDetail{}).Open() || !(PRDetail{State: "open"}).Open() || (PRDetail{State: "closed"}).Open() {
		t.Error("Open() wrong for one of the states")
	}
}
