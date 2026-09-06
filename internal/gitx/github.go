package gitx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// PRDetail is what the pull requests tab shows for one PR: the review
// state broken out so requested changes can be read and acted on.
type PRDetail struct {
	PR
	Body         string
	Additions    int
	Deletions    int
	ChangedFiles int
	Mergeable    string // "MERGEABLE" | "CONFLICTING" | "UNKNOWN"
	MergeState   string // gh's mergeStateStatus: CLEAN, BLOCKED, BEHIND, DIRTY, UNSTABLE, ...
	Labels       []string
	Reviews      []Review
	Threads      []ReviewThread
	Checks       []Check
}

// Review is one submitted review; only the latest per reviewer is kept.
type Review struct {
	Author      string
	State       string // "APPROVED" | "CHANGES_REQUESTED" | "COMMENTED" | "DISMISSED"
	Body        string
	URL         string
	SubmittedAt time.Time
}

// ReviewThread is a conversation on a line of the diff.
type ReviewThread struct {
	Path     string
	Line     int
	Resolved bool
	Outdated bool
	Comments []ReviewComment
}

type ReviewComment struct {
	Author    string
	Body      string
	URL       string
	CreatedAt time.Time
}

// Check is one check run or commit status on the head commit.
type Check struct {
	Name   string
	Status string // "success" | "failure" | "pending" | "skipped"
	URL    string
}

// Unresolved returns the review threads still open.
func (d PRDetail) Unresolved() []ReviewThread {
	var out []ReviewThread
	for _, t := range d.Threads {
		if !t.Resolved {
			out = append(out, t)
		}
	}
	return out
}

// Failing returns the checks that failed.
func (d PRDetail) Failing() []Check {
	var out []Check
	for _, c := range d.Checks {
		if c.Status == "failure" {
			out = append(out, c)
		}
	}
	return out
}

// ChangesRequested lists the reviewers whose latest review asks for
// changes.
func (d PRDetail) ChangesRequested() []Review {
	var out []Review
	for _, r := range d.Reviews {
		if r.State == "CHANGES_REQUESTED" {
			out = append(out, r)
		}
	}
	return out
}

const prDetailQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      number title url headRefName baseRefName isDraft reviewDecision updatedAt body
      additions deletions changedFiles mergeable mergeStateStatus
      author { login }
      labels(first: 20) { nodes { name } }
      reviews(last: 50) { nodes { author { login } state body url submittedAt } }
      reviewThreads(first: 100) {
        nodes {
          isResolved isOutdated path line originalLine
          comments(first: 50) { nodes { author { login } body url createdAt } }
        }
      }
      commits(last: 1) {
        nodes { commit { statusCheckRollup { contexts(first: 100) { nodes {
          __typename
          ... on CheckRun { name status conclusion detailsUrl }
          ... on StatusContext { context state targetUrl }
        } } } } }
      }
    }
  }
}`

// PullRequestDetail reads one PR of repo ("owner/name") in full.
func PullRequestDetail(ctx context.Context, dir, repo string, number int) (PRDetail, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return PRDetail{}, errors.New("unknown repository")
	}
	raw, err := gh(ctx, dir, "api", "graphql", "-f", "query="+prDetailQuery, "-F", "owner="+owner, "-F", "name="+name, "-F", fmt.Sprintf("number=%d", number))
	if err != nil {
		return PRDetail{}, err
	}
	return decodePRDetail(raw)
}

func decodePRDetail(raw []byte) (PRDetail, error) {
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					Number       int    `json:"number"`
					Title        string `json:"title"`
					URL          string `json:"url"`
					Head         string `json:"headRefName"`
					Base         string `json:"baseRefName"`
					Draft        bool   `json:"isDraft"`
					Review       string `json:"reviewDecision"`
					UpdatedAt    string `json:"updatedAt"`
					Body         string `json:"body"`
					Additions    int    `json:"additions"`
					Deletions    int    `json:"deletions"`
					ChangedFiles int    `json:"changedFiles"`
					Mergeable    string `json:"mergeable"`
					MergeState   string `json:"mergeStateStatus"`
					Author       struct {
						Login string `json:"login"`
					} `json:"author"`
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					Reviews struct {
						Nodes []struct {
							Author struct {
								Login string `json:"login"`
							} `json:"author"`
							State       string `json:"state"`
							Body        string `json:"body"`
							URL         string `json:"url"`
							SubmittedAt string `json:"submittedAt"`
						} `json:"nodes"`
					} `json:"reviews"`
					ReviewThreads struct {
						Nodes []struct {
							IsResolved   bool   `json:"isResolved"`
							IsOutdated   bool   `json:"isOutdated"`
							Path         string `json:"path"`
							Line         *int   `json:"line"`
							OriginalLine *int   `json:"originalLine"`
							Comments     struct {
								Nodes []struct {
									Author struct {
										Login string `json:"login"`
									} `json:"author"`
									Body      string `json:"body"`
									URL       string `json:"url"`
									CreatedAt string `json:"createdAt"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								Rollup *struct {
									Contexts struct {
										Nodes []struct {
											Type       string `json:"__typename"`
											Name       string `json:"name"`
											Status     string `json:"status"`
											Conclusion string `json:"conclusion"`
											DetailsURL string `json:"detailsUrl"`
											Context    string `json:"context"`
											State      string `json:"state"`
											TargetURL  string `json:"targetUrl"`
										} `json:"nodes"`
									} `json:"contexts"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return PRDetail{}, err
	}
	if len(resp.Errors) > 0 {
		return PRDetail{}, errors.New(resp.Errors[0].Message)
	}
	pr := resp.Data.Repository.PullRequest
	if pr == nil {
		return PRDetail{}, errors.New("pull request not found")
	}
	d := PRDetail{
		PR:        PR{Number: pr.Number, Title: pr.Title, URL: pr.URL, Head: pr.Head, Base: pr.Base, Author: pr.Author.Login, Draft: pr.Draft, Review: strings.ToLower(pr.Review)},
		Body:      pr.Body,
		Additions: pr.Additions, Deletions: pr.Deletions, ChangedFiles: pr.ChangedFiles,
		Mergeable: pr.Mergeable, MergeState: pr.MergeState,
	}
	d.UpdatedAt, _ = time.Parse(time.RFC3339, pr.UpdatedAt)
	for _, l := range pr.Labels.Nodes {
		d.Labels = append(d.Labels, l.Name)
	}
	// Reviews arrive oldest first; the last one per reviewer counts, and
	// plain comments do not replace a verdict.
	latest := map[string]int{}
	for _, r := range pr.Reviews.Nodes {
		if r.State == "PENDING" || r.Author.Login == d.Author {
			continue
		}
		rv := Review{Author: r.Author.Login, State: r.State, Body: r.Body, URL: r.URL}
		rv.SubmittedAt, _ = time.Parse(time.RFC3339, r.SubmittedAt)
		if i, ok := latest[rv.Author]; ok {
			if rv.State == "COMMENTED" && d.Reviews[i].State != "COMMENTED" {
				continue
			}
			d.Reviews[i] = rv
			continue
		}
		latest[rv.Author] = len(d.Reviews)
		d.Reviews = append(d.Reviews, rv)
	}
	for _, t := range pr.ReviewThreads.Nodes {
		rt := ReviewThread{Path: t.Path, Resolved: t.IsResolved, Outdated: t.IsOutdated}
		if t.Line != nil {
			rt.Line = *t.Line
		} else if t.OriginalLine != nil {
			rt.Line = *t.OriginalLine
		}
		for _, c := range t.Comments.Nodes {
			rc := ReviewComment{Author: c.Author.Login, Body: c.Body, URL: c.URL}
			rc.CreatedAt, _ = time.Parse(time.RFC3339, c.CreatedAt)
			rt.Comments = append(rt.Comments, rc)
		}
		d.Threads = append(d.Threads, rt)
	}
	if len(pr.Commits.Nodes) > 0 && pr.Commits.Nodes[0].Commit.Rollup != nil {
		for _, c := range pr.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes {
			ch := Check{Name: c.Name, URL: c.DetailsURL}
			if c.Type == "StatusContext" {
				ch.Name, ch.URL = c.Context, c.TargetURL
				ch.Status = map[string]string{"SUCCESS": "success", "PENDING": "pending", "EXPECTED": "pending", "FAILURE": "failure", "ERROR": "failure"}[c.State]
			} else {
				switch {
				case c.Status != "COMPLETED":
					ch.Status = "pending"
				case c.Conclusion == "SUCCESS":
					ch.Status = "success"
				case c.Conclusion == "NEUTRAL" || c.Conclusion == "SKIPPED":
					ch.Status = "skipped"
				default:
					ch.Status = "failure"
				}
			}
			d.Checks = append(d.Checks, ch)
			if ch.Status != "skipped" {
				d.PR.Checks = worseCheck(d.PR.Checks, ch.Status)
			}
		}
	}
	return d, nil
}

// FixPrompt writes the prompt for a thread that should get the PR ready:
// the requested changes, the open review threads and the failing checks,
// in a form an agent can work through.
func (d PRDetail) FixPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Address the review feedback on pull request #%d \"%s\" (%s).\n", d.Number, d.Title, d.URL)
	fmt.Fprintf(&b, "The PR branch is `%s`, merging into `%s`. Make sure that branch is checked out before changing anything.\n", d.Head, d.Base)
	if crs := d.ChangesRequested(); len(crs) > 0 {
		b.WriteString("\n## Reviews requesting changes\n")
		for _, r := range crs {
			fmt.Fprintf(&b, "\n@%s:\n", r.Author)
			if body := strings.TrimSpace(r.Body); body != "" {
				b.WriteString(indent(body) + "\n")
			} else {
				b.WriteString("    (see the comments below)\n")
			}
		}
	}
	if ts := d.Unresolved(); len(ts) > 0 {
		b.WriteString("\n## Unresolved review comments\n")
		for i, t := range ts {
			where := t.Path
			if t.Line > 0 {
				where = fmt.Sprintf("%s:%d", t.Path, t.Line)
			}
			if t.Outdated {
				where += " (on an older revision)"
			}
			fmt.Fprintf(&b, "\n%d. %s\n", i+1, where)
			for _, c := range t.Comments {
				fmt.Fprintf(&b, "   @%s: %s\n", c.Author, strings.TrimSpace(strings.ReplaceAll(c.Body, "\n", "\n   ")))
			}
		}
	}
	if fs := d.Failing(); len(fs) > 0 {
		b.WriteString("\n## Failing checks\n")
		for _, c := range fs {
			if c.URL != "" {
				fmt.Fprintf(&b, "- %s (%s)\n", c.Name, c.URL)
			} else {
				fmt.Fprintf(&b, "- %s\n", c.Name)
			}
		}
	}
	if d.Mergeable == "CONFLICTING" {
		fmt.Fprintf(&b, "\nThe branch has merge conflicts with `%s`; resolve them too.\n", d.Base)
	}
	b.WriteString("\nWork through each point, run the relevant tests, and commit the fixes on the PR branch. Reply with a short summary of what changed per point, and say so if you disagree with a comment instead of applying it.\n")
	return b.String()
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}
