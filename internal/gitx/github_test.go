package gitx

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

}

func TestDecodePRDetail(t *testing.T) {
	raw := []byte(`{"data":{"viewer": {"login": "me"}, "repository":{"pullRequest":{
	  "number": 7, "title": "Add panes", "url": "https://github.com/o/r/pull/7", "headRefName": "panes", "headRefOid": "abc123", "baseRefName": "main",
	  "comments": {"nodes": [{"id": "IC1", "author": {"login": "bot"}, "body": "Coverage down.", "url": "ic1", "createdAt": "2026-09-05T00:30:00Z"}]},
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
	if got := d.Threads[0]; got.Resolved || got.Path != "term.js" || got.Line != 40 || len(got.Comments) != 2 {
		t.Errorf("open thread = %+v", got)
	}
	if d.Threads[1].Line != 12 || !d.Threads[1].Resolved {
		t.Errorf("resolved thread = %+v", d.Threads[1])
	}
	if len(d.Checks) != 3 || d.Checks[0].Status != "failure" || d.Checks[1].Status != "skipped" || d.Checks[2].Status != "pending" || d.PR.Checks != "failure" {
		t.Errorf("checks = %+v, rollup %q", d.Checks, d.PR.Checks)
	}
	if d.HeadSHA != "abc123" || d.Viewer != "me" {
		t.Errorf("head %q, viewer %q", d.HeadSHA, d.Viewer)
	}
	// Remarks: every review with words or a verdict, the conversation
	// comment and the line comments, oldest first.
	var who []string
	for _, r := range d.Remarks {
		who = append(who, r.Author+":"+r.Path+":"+r.State)
	}
	if got := strings.Join(who, " "); got != "bb::COMMENTED bot:: rev::CHANGES_REQUESTED rev:term.js: rev:term.go: bb:term.js: rev::COMMENTED other::APPROVED" {
		t.Errorf("remarks = %s", got)
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
	if d.Review != "rereview_requested" {
		t.Fatalf("detail = %+v", d.PR)
	}
	// One blocking reviewer re-requested, another not: still changes requested.
	raw = []byte(strings.Replace(string(raw), `"reviews":{"nodes":[`, `"reviews":{"nodes":[{"author":{"login":"other"},"state":"CHANGES_REQUESTED","body":"no","url":"r2","submittedAt":"2026-09-06T02:00:00Z"},`, 1))
	d, _ = decodePRDetail(raw)
	if d.Review != "changes_requested" {
		t.Errorf("mixed reviewers: review %q", d.Review)
	}
}

func TestReposFromRemoteList(t *testing.T) {
	out := "origin\tgit@github.com:bb/clusterio.git (fetch)\norigin\tgit@github.com:bb/clusterio.git (push)\nupstream\thttps://github.com/clusterio/clusterio (fetch)\nupstream\thttps://github.com/clusterio/clusterio (push)\nlab\thttps://gitlab.com/x/y.git (fetch)\n"
	got := reposFromRemoteList(out)
	if len(got) != 2 || got[0] != "bb/clusterio" || got[1] != "clusterio/clusterio" {
		t.Errorf("got %v", got)
	}
}

func TestPRDetailState(t *testing.T) {
	raw := []byte(`{"data":{"repository":{"pullRequest":{"number": 3, "title": "t", "state": "MERGED", "mergeStateStatus": "BLOCKED", "author": {"login": "bb"}}}}}`)
	d, err := decodePRDetail(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "merged" || d.Open() {
		t.Errorf("state = %q, open %v", d.State, d.Open())
	}
	if !(PRDetail{}).Open() || !(PRDetail{State: "open"}).Open() || (PRDetail{State: "closed"}).Open() {
		t.Error("Open() wrong for one of the states")
	}
}

// A batch where GitHub could not read one alias still yields the others,
// and the failed one gets GitHub's message.
func TestDecodeDetailsPartial(t *testing.T) {
	raw := []byte(`{"data":{"viewer":{"login":"me"},
	  "pr0":{"pullRequest":{"number":7,"title":"t","state":"OPEN","updatedAt":"2026-09-06T05:00:00Z","author":{"login":"me"},
	    "comments":{"nodes":[{"id":"c","author":{"login":"rev"},"authorAssociation":"MEMBER","body":"hi","url":"u","createdAt":"2026-09-07T01:00:00Z"}]},
	    "commits":{"nodes":[{"commit":{"committedDate":"2026-09-05T00:00:00Z","statusCheckRollup":{"contexts":{"nodes":[
	      {"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS","completedAt":"2026-09-08T00:00:00Z"}]}}}}]}}},
	  "pr1":null,
	  "pr2":{"pullRequest":null}},
	 "errors":[{"type":"NOT_FOUND","path":["pr1"],"message":"Could not resolve to a Repository with the name 'o/gone'."}]}`)
	rs := []PRRead{{PRRef: PRRef{"o/r", 7}}, {PRRef: PRRef{"o/gone", 1}}, {PRRef: PRRef{"o/r", 999}}}
	if err := decodeDetails(raw, rs); err != nil {
		t.Fatal(err)
	}
	d := rs[0].Detail
	if rs[0].Err != nil || d.Number != 7 || d.Viewer != "me" || len(d.Remarks) != 1 || d.Remarks[0].Role != "MEMBER" || !d.Open() {
		t.Fatalf("pr0 = %+v, %v", d, rs[0].Err)
	}
	if want := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC); !d.Activity.Equal(want) {
		t.Errorf("activity = %v, want the check finishing", d.Activity)
	}
	if rs[1].Err == nil || !strings.Contains(rs[1].Err.Error(), "o/gone") {
		t.Errorf("pr1 err = %v", rs[1].Err)
	}
	if rs[2].Err == nil {
		t.Error("pr2 read without a pull request")
	}
}
