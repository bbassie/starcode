package gitx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// PR is one pull request as the GitHub CLI reports it.
type PR struct {
	Number    int
	Title     string
	URL       string
	Head      string // branch the PR merges from
	Base      string
	Author    string
	Draft     bool
	Review    string // "approved" | "changes_requested" | "review_required" | ""
	Checks    string // "success" | "failure" | "pending" | ""
	UpdatedAt time.Time
}

// PRList is what the pull requests tab shows: the repository's open PRs
// with the one for the current branch first, or why none could be read.
type PRList struct {
	Repo          string // "owner/name"
	RepoURL       string
	DefaultBranch string
	Branch        string // the working tree's branch
	PRs           []PR
	Err           string
}

// Current is the PR whose head is the working tree's branch, if any.
func (l PRList) Current() (PR, bool) {
	for _, p := range l.PRs {
		if p.Head == l.Branch && l.Branch != "" {
			return p, true
		}
	}
	return PR{}, false
}

// CompareURL is GitHub's page for opening a PR from the current branch;
// empty on the default branch or when the repository is unknown.
func (l PRList) CompareURL() string {
	if l.RepoURL == "" || l.Branch == "" || l.Branch == l.DefaultBranch || l.Branch == "HEAD" {
		return ""
	}
	return l.RepoURL + "/compare/" + l.Branch + "?expand=1"
}

// gh runs the GitHub CLI in dir. Its stderr is the error message when it
// fails, which covers the useful cases: no GitHub remote, not signed in.
func gh(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("the GitHub CLI (gh) is not installed")
		}
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return nil, errors.New(firstLine(msg))
		}
		return nil, err
	}
	return out.Bytes(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// PullRequests lists the open PRs of the repository dir belongs to. It
// needs gh on PATH and signed in; anything else lands in Err.
func PullRequests(ctx context.Context, dir string) PRList {
	l := PRList{Branch: Read(ctx, dir).Branch}
	repo, err := gh(ctx, dir, "repo", "view", "--json", "nameWithOwner,url,defaultBranchRef")
	if err != nil {
		l.Err = err.Error()
		return l
	}
	var rv struct {
		NameWithOwner    string `json:"nameWithOwner"`
		URL              string `json:"url"`
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := json.Unmarshal(repo, &rv); err != nil {
		l.Err = "gh repo view: " + err.Error()
		return l
	}
	l.Repo, l.RepoURL, l.DefaultBranch = rv.NameWithOwner, rv.URL, rv.DefaultBranchRef.Name
	raw, err := gh(ctx, dir, "pr", "list", "--limit", "40", "--json", "number,title,url,headRefName,baseRefName,author,isDraft,reviewDecision,statusCheckRollup,updatedAt")
	if err != nil {
		l.Err = err.Error()
		return l
	}
	prs, err := decodePRs(raw)
	if err != nil {
		l.Err = "gh pr list: " + err.Error()
		return l
	}
	// The current branch's PR first, the rest as gh orders them (newest).
	for i, p := range prs {
		if p.Head == l.Branch && i > 0 {
			prs = append([]PR{p}, append(prs[:i:i], prs[i+1:]...)...)
			break
		}
	}
	l.PRs = prs
	return l
}

// decodePRs turns gh's JSON into PRs, folding the check rollup into one
// word.
func decodePRs(raw []byte) ([]PR, error) {
	var rows []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		Head   string `json:"headRefName"`
		Base   string `json:"baseRefName"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Draft     bool   `json:"isDraft"`
		Review    string `json:"reviewDecision"`
		UpdatedAt string `json:"updatedAt"`
		Checks    []struct {
			Status     string `json:"status"`     // check runs: COMPLETED, IN_PROGRESS, QUEUED
			Conclusion string `json:"conclusion"` // check runs: SUCCESS, FAILURE, NEUTRAL, SKIPPED, ...
			State      string `json:"state"`      // status contexts: SUCCESS, FAILURE, ERROR, PENDING
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]PR, 0, len(rows))
	for _, r := range rows {
		p := PR{Number: r.Number, Title: r.Title, URL: r.URL, Head: r.Head, Base: r.Base, Author: r.Author.Login, Draft: r.Draft, Review: strings.ToLower(r.Review)}
		p.UpdatedAt, _ = time.Parse(time.RFC3339, r.UpdatedAt)
		for _, c := range r.Checks {
			word := ""
			switch {
			case c.State != "":
				word = map[string]string{"SUCCESS": "success", "PENDING": "pending", "EXPECTED": "pending", "FAILURE": "failure", "ERROR": "failure"}[c.State]
			case c.Status != "COMPLETED":
				word = "pending"
			default:
				switch c.Conclusion {
				case "SUCCESS", "NEUTRAL", "SKIPPED":
					word = "success"
				case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
					word = "failure"
				}
			}
			p.Checks = worseCheck(p.Checks, word)
		}
		out = append(out, p)
	}
	return out, nil
}

// worseCheck folds two check words: a failure outranks pending, which
// outranks success.
func worseCheck(a, b string) string {
	rank := map[string]int{"": 0, "success": 1, "pending": 2, "failure": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
