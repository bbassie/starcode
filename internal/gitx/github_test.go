package gitx

import (
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
