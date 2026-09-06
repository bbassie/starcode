package gitx

import "testing"

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
