package gitx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PR is one pull request as the GitHub CLI reports it.
type PR struct {
	Number int
	Title  string
	URL    string
	Head   string // branch the PR merges from
	Base   string
	Author string
	Draft  bool
	// Review is GitHub's decision, with one word of its own:
	// "rereview_requested" is changes requested where every reviewer who
	// asked has since been asked to review again, so the PR waits on them
	// and not on its author.
	Review    string // "approved" | "changes_requested" | "rereview_requested" | "review_required" | ""
	Checks    string // "success" | "failure" | "pending" | ""
	UpdatedAt time.Time
	// Requested are the reviewers asked for a review who have not
	// answered yet; Blocking those whose latest review asks for changes.
	Requested []string
	Blocking  []string
	// Comments is how many comments the PR has had, conversation and
	// review comments together, as GitHub's own list counts them. Only
	// the GraphQL readers fill it.
	Comments int
}

// reviewWord folds GitHub's decision and the two reviewer lists into
// PR.Review. GitHub keeps CHANGES_REQUESTED until the reviewer reviews
// again or is dismissed; a pending request for every blocking reviewer
// means that is what everyone is waiting for.
func reviewWord(decision string, blocking, requested []string) string {
	w := strings.ToLower(decision)
	if w != "changes_requested" || len(requested) == 0 {
		return w
	}
	for _, b := range blocking {
		if !slices.Contains(requested, b) {
			return w
		}
	}
	return "rereview_requested"
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
	raw, err := gh(ctx, dir, "pr", "list", "--limit", "40", "--json", "number,title,url,headRefName,baseRefName,author,isDraft,reviewDecision,statusCheckRollup,updatedAt,latestReviews,reviewRequests")
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
		// latestReviews leaves out a reviewer with a pending re-request,
		// which is what makes the re-review word work from this list.
		LatestReviews []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			State string `json:"state"`
		} `json:"latestReviews"`
		ReviewRequests []struct {
			Login string `json:"login"`
		} `json:"reviewRequests"`
		Checks []struct {
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
		p := PR{Number: r.Number, Title: r.Title, URL: r.URL, Head: r.Head, Base: r.Base, Author: r.Author.Login, Draft: r.Draft}
		p.UpdatedAt, _ = time.Parse(time.RFC3339, r.UpdatedAt)
		for _, rv := range r.LatestReviews {
			if rv.State == "CHANGES_REQUESTED" {
				p.Blocking = append(p.Blocking, rv.Author.Login)
			}
		}
		for _, rr := range r.ReviewRequests {
			if rr.Login != "" {
				p.Requested = append(p.Requested, rr.Login)
			}
		}
		p.Review = reviewWord(r.Review, p.Blocking, p.Requested)
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
	State        string // "open" | "merged" | "closed"
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

// NeedsWork is whether anything stands in the way of merging that an
// agent could act on: requested changes, open comments, failing checks or
// conflicts.
func (d PRDetail) NeedsWork() bool {
	return len(d.ChangesRequested()) > 0 || len(d.Unresolved()) > 0 || len(d.Failing()) > 0 || d.Mergeable == "CONFLICTING"
}

// ChangesRequested lists the reviewers whose latest review asks for
// changes and who have not been asked to review again: the ones whose
// feedback still waits on the author.
func (d PRDetail) ChangesRequested() []Review {
	var out []Review
	for _, r := range d.Reviews {
		if r.State == "CHANGES_REQUESTED" && !slices.Contains(d.Requested, r.Author) {
			out = append(out, r)
		}
	}
	return out
}

// AwaitingReReview lists the reviewers who asked for changes and have
// since been asked to review again.
func (d PRDetail) AwaitingReReview() []Review {
	var out []Review
	for _, r := range d.Reviews {
		if r.State == "CHANGES_REQUESTED" && slices.Contains(d.Requested, r.Author) {
			out = append(out, r)
		}
	}
	return out
}

const prDetailQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      number title url headRefName baseRefName isDraft reviewDecision updatedAt body state
      additions deletions changedFiles mergeable mergeStateStatus
      author { login }
      labels(first: 20) { nodes { name } }
      reviewRequests(first: 20) { nodes { requestedReviewer { ... on User { login } } } }
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
					State        string `json:"state"`
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
					ReviewRequests struct {
						Nodes []struct {
							RequestedReviewer struct {
								Login string `json:"login"`
							} `json:"requestedReviewer"`
						} `json:"nodes"`
					} `json:"reviewRequests"`
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
		PR:        PR{Number: pr.Number, Title: pr.Title, URL: pr.URL, Head: pr.Head, Base: pr.Base, Author: pr.Author.Login, Draft: pr.Draft},
		State:     strings.ToLower(pr.State),
		Body:      pr.Body,
		Additions: pr.Additions, Deletions: pr.Deletions, ChangedFiles: pr.ChangedFiles,
		Mergeable: pr.Mergeable, MergeState: pr.MergeState,
	}
	d.UpdatedAt, _ = time.Parse(time.RFC3339, pr.UpdatedAt)
	for _, l := range pr.Labels.Nodes {
		d.Labels = append(d.Labels, l.Name)
	}
	for _, rr := range pr.ReviewRequests.Nodes {
		if rr.RequestedReviewer.Login != "" {
			d.Requested = append(d.Requested, rr.RequestedReviewer.Login)
		}
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
	for _, r := range d.Reviews {
		if r.State == "CHANGES_REQUESTED" {
			d.Blocking = append(d.Blocking, r.Author)
		}
	}
	d.Review = reviewWord(pr.Review, d.Blocking, d.Requested)
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
	if !d.NeedsWork() {
		// Nothing to address: the prompt only sets the scene and leaves
		// the ask to the reader, who is about to edit it anyway.
		fmt.Fprintf(&b, "Work on pull request #%d \"%s\" (%s).\n", d.Number, d.Title, d.URL)
		fmt.Fprintf(&b, "The PR branch is `%s`, merging into `%s`. Make sure that branch is checked out before changing anything.\n", d.Head, d.Base)
		return b.String()
	}
	fmt.Fprintf(&b, "Address the review feedback on pull request #%d \"%s\" (%s).\n", d.Number, d.Title, d.URL)
	fmt.Fprintf(&b, "The PR branch is `%s`, merging into `%s`. Make sure that branch is checked out before changing anything.\n", d.Head, d.Base)
	if waiting := d.AwaitingReReview(); len(waiting) > 0 {
		names := make([]string, len(waiting))
		for i, r := range waiting {
			names[i] = "@" + r.Author
		}
		fmt.Fprintf(&b, "%s asked for changes earlier and has been asked to review again; leave that feedback alone unless a point below repeats it.\n", strings.Join(names, ", "))
	}
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

// FocusPrompt is the prompt for one point of the PR, named by the URL of
// a review or of a comment in a review thread: the same header as
// FixPrompt, then only that review or that thread. An unknown URL gives
// the whole FixPrompt.
func (d PRDetail) FocusPrompt(url string) string {
	var b strings.Builder
	head := func() {
		fmt.Fprintf(&b, "Address one review comment on pull request #%d \"%s\" (%s).\n", d.Number, d.Title, d.URL)
		fmt.Fprintf(&b, "The PR branch is `%s`, merging into `%s`. Make sure that branch is checked out before changing anything.\n", d.Head, d.Base)
	}
	for _, r := range d.Reviews {
		if r.URL != url || url == "" {
			continue
		}
		head()
		fmt.Fprintf(&b, "\n@%s %s (%s):\n", r.Author, strings.ToLower(strings.ReplaceAll(r.State, "_", " ")), r.URL)
		if body := strings.TrimSpace(r.Body); body != "" {
			b.WriteString(indent(body) + "\n")
		}
		b.WriteString("\nDo what the review asks, run the relevant tests, and commit on the PR branch. Reply with a short summary, and say so if you disagree with the review instead of applying it.\n")
		return b.String()
	}
	for _, t := range d.Threads {
		hit := false
		for _, c := range t.Comments {
			if c.URL == url && url != "" {
				hit = true
			}
		}
		if !hit {
			continue
		}
		head()
		where := t.Path
		if t.Line > 0 {
			where = fmt.Sprintf("%s:%d", t.Path, t.Line)
		}
		if t.Outdated {
			where += " (on an older revision)"
		}
		fmt.Fprintf(&b, "\nComment thread on %s:\n", where)
		for _, c := range t.Comments {
			fmt.Fprintf(&b, "   @%s: %s\n", c.Author, strings.TrimSpace(strings.ReplaceAll(c.Body, "\n", "\n   ")))
		}
		if t.Resolved {
			b.WriteString("\n(The thread is marked resolved on GitHub.)\n")
		}
		b.WriteString("\nDo what the comment asks, run the relevant tests, and commit on the PR branch. Reply with a short summary, and say so if you disagree with the comment instead of applying it.\n")
		return b.String()
	}
	return d.FixPrompt()
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

// Open is whether the PR can still be reviewed and merged.
func (d PRDetail) Open() bool {
	return d.State == "" || d.State == "open"
}

// MergeStateNote says in a few words why GitHub will not merge the PR
// as it stands; empty when it would (CLEAN) or when gh has no word for
// it yet.
func (d PRDetail) MergeStateNote() string {
	switch d.MergeState {
	case "BLOCKED":
		return "blocked by branch rules or reviews"
	case "BEHIND":
		return "behind the base branch"
	case "DIRTY":
		return "has conflicts"
	case "UNSTABLE":
		return "checks failing"
	case "DRAFT":
		return "still a draft"
	}
	return ""
}

// ---- acting on a pull request ----

// The actions the console can take on a pull request through gh: a
// review verdict, a comment, a merge, or a change of its state. The
// names are the ones the routes use.
const (
	ActionApprove        = "approve"
	ActionRequestChanges = "request-changes"
	ActionComment        = "comment"
	ActionMerge          = "merge"
	ActionUpdateBranch   = "update-branch"
	ActionReady          = "ready"
	ActionClose          = "close"
)

// prActionArgs is gh's command line for action on repo#number. body is
// the review or comment text; method ("squash", "merge", "rebase") and
// auto only matter for a merge. It refuses what gh would refuse (a
// request for changes without a word of why) before a process starts.
func prActionArgs(action, repo string, number int, body, method string, auto bool) ([]string, error) {
	n := strconv.Itoa(number)
	body = strings.TrimSpace(body)
	switch action {
	case ActionApprove:
		args := []string{"pr", "review", "-R", repo, n, "--approve"}
		if body != "" {
			args = append(args, "-b", body)
		}
		return args, nil
	case ActionRequestChanges:
		if body == "" {
			return nil, errors.New("say what should change")
		}
		return []string{"pr", "review", "-R", repo, n, "--request-changes", "-b", body}, nil
	case ActionComment:
		if body == "" {
			return nil, errors.New("the comment is empty")
		}
		return []string{"pr", "comment", "-R", repo, n, "-b", body}, nil
	case ActionMerge:
		if method == "" {
			method = "squash"
		}
		if method != "squash" && method != "merge" && method != "rebase" {
			return nil, fmt.Errorf("unknown merge method %q", method)
		}
		args := []string{"pr", "merge", "-R", repo, n, "--" + method}
		if auto {
			args = append(args, "--auto")
		}
		return args, nil
	case ActionUpdateBranch:
		return []string{"pr", "update-branch", "-R", repo, n}, nil
	case ActionReady:
		return []string{"pr", "ready", "-R", repo, n}, nil
	case ActionClose:
		return []string{"pr", "close", "-R", repo, n}, nil
	}
	return nil, fmt.Errorf("unknown pull request action %q", action)
}

// DoPRAction runs action on repo#number with gh in dir. The error is
// gh's own first line of stderr, which names the reason (not signed in,
// no permission, branch protection).
func DoPRAction(ctx context.Context, dir, repo string, number int, action, body, method string, auto bool) error {
	args, err := prActionArgs(action, repo, number, body, method, auto)
	if err != nil {
		return err
	}
	_, err = gh(ctx, dir, args...)
	return err
}

// Approve submits an approving review, with body as its text when set.
func Approve(ctx context.Context, dir, repo string, number int, body string) error {
	return DoPRAction(ctx, dir, repo, number, ActionApprove, body, "", false)
}

// RequestChanges submits a review asking for changes; body must say what.
func RequestChanges(ctx context.Context, dir, repo string, number int, body string) error {
	return DoPRAction(ctx, dir, repo, number, ActionRequestChanges, body, "", false)
}

// Comment adds body to the PR's conversation.
func Comment(ctx context.Context, dir, repo string, number int, body string) error {
	return DoPRAction(ctx, dir, repo, number, ActionComment, body, "", false)
}

// Merge merges the PR with method ("squash" by default, "merge" or
// "rebase"); with auto set GitHub merges once the checks pass instead.
// The head branch is left alone.
func Merge(ctx context.Context, dir, repo string, number int, method string, auto bool) error {
	return DoPRAction(ctx, dir, repo, number, ActionMerge, "", method, auto)
}

// UpdateBranch merges the base branch into the PR's branch on GitHub.
func UpdateBranch(ctx context.Context, dir, repo string, number int) error {
	return DoPRAction(ctx, dir, repo, number, ActionUpdateBranch, "", "", false)
}

// ReadyForReview takes the PR out of draft.
func ReadyForReview(ctx context.Context, dir, repo string, number int) error {
	return DoPRAction(ctx, dir, repo, number, ActionReady, "", "", false)
}

// Close closes the PR without merging; the branch stays.
func Close(ctx context.Context, dir, repo string, number int) error {
	return DoPRAction(ctx, dir, repo, number, ActionClose, "", "", false)
}

// pullURL matches a pull request page on GitHub, wherever it appears in
// text: the URL gh prints after creating one, or one pasted by hand.
var pullURL = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+)`)

// FindPullURL returns the first pull request URL in text with its
// repository ("owner/name") and number.
func FindPullURL(text string) (repo string, number int, url string, ok bool) {
	m := pullURL.FindStringSubmatch(text)
	if m == nil {
		return "", 0, "", false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return "", 0, "", false
	}
	return m[1], n, m[0], true
}

// PullURL is the page of a pull request on github.com.
func PullURL(repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}

// Remotes lists the GitHub repositories ("owner/name") dir's remotes
// point at, read from the git config so it costs no network call. A
// clone of a fork has two: the fork as origin and the upstream, and a
// pull request lives in either.
func Remotes(ctx context.Context, dir string) []string {
	out, err := run(ctx, dir, "remote", "-v")
	if err != nil {
		return nil
	}
	return reposFromRemoteList(out)
}

// reposFromRemoteList parses `git remote -v` output into unique repos.
func reposFromRemoteList(out string) []string {
	var repos []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if repo := RepoFromRemote(f[1]); repo != "" && !slices.Contains(repos, repo) {
			repos = append(repos, repo)
		}
	}
	return repos
}

// RepoFromRemote parses "owner/name" out of an https, ssh or scp-style
// remote URL on github.com.
func RepoFromRemote(remote string) string {
	m := regexp.MustCompile(`github\.com[:/]([\w.-]+/[\w.-]+?)(?:\.git)?/?$`).FindStringSubmatch(remote)
	if m == nil {
		return ""
	}
	return m[1]
}

// PRState is what a poll reports for a linked pull request.
type PRState struct {
	Repo   string
	Number int
	Title  string
	URL    string
	State  string // "open" | "merged" | "closed"
	Review string // as PR.Review
	Checks string // as PR.Checks
	Draft  bool
	Head   string
}

// PullRequestStates reads the state of each (repo, number) pair in one
// GraphQL call. A PR that cannot be read (deleted, no access) is left out
// of the result rather than failing the batch.
func PullRequestStates(ctx context.Context, dir string, refs []PRRef) ([]PRState, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	var q strings.Builder
	q.WriteString("query {")
	for i, r := range refs {
		owner, name, ok := strings.Cut(r.Repo, "/")
		if !ok {
			continue
		}
		fmt.Fprintf(&q, ` pr%d: repository(owner: %q, name: %q) { pullRequest(number: %d) { %s } }`, i, owner, name, r.Number, prStateFields)
	}
	q.WriteString(" }")
	raw, err := gh(ctx, dir, "api", "graphql", "-f", "query="+q.String())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data map[string]struct {
			PullRequest *prNode `json:"pullRequest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	var out []PRState
	for i, r := range refs {
		if n, ok := resp.Data[fmt.Sprintf("pr%d", i)]; ok && n.PullRequest != nil {
			st := n.PullRequest.state()
			st.Repo, st.Number = r.Repo, r.Number
			out = append(out, st)
		}
	}
	return out, nil
}

// PRRef names a pull request for PullRequestStates.
type PRRef struct {
	Repo   string
	Number int
}

const prStateFields = `number title url state isDraft reviewDecision headRefName baseRefName updatedAt totalCommentsCount author { login } repository { nameWithOwner } latestOpinionatedReviews(first: 20) { nodes { author { login } state } } reviewRequests(first: 20) { nodes { requestedReviewer { ... on User { login } } } } commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }`

// prNode is the GraphQL shape prStateFields selects.
type prNode struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	State     string `json:"state"` // OPEN, CLOSED, MERGED
	Draft     bool   `json:"isDraft"`
	Review    string `json:"reviewDecision"`
	Head      string `json:"headRefName"`
	Base      string `json:"baseRefName"`
	UpdatedAt string `json:"updatedAt"`
	Comments  int    `json:"totalCommentsCount"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	// latestOpinionatedReviews keeps a reviewer with a pending re-request
	// (latestReviews would not), so the requests are read alongside.
	LatestReviews struct {
		Nodes []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			State string `json:"state"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer struct {
				Login string `json:"login"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Rollup *struct {
					State string `json:"state"` // SUCCESS, FAILURE, ERROR, PENDING, EXPECTED
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func (n prNode) reviewers() (blocking, requested []string) {
	for _, r := range n.LatestReviews.Nodes {
		if r.State == "CHANGES_REQUESTED" {
			blocking = append(blocking, r.Author.Login)
		}
	}
	for _, r := range n.ReviewRequests.Nodes {
		if r.RequestedReviewer.Login != "" {
			requested = append(requested, r.RequestedReviewer.Login)
		}
	}
	return blocking, requested
}

func (n prNode) state() PRState {
	blocking, requested := n.reviewers()
	st := PRState{Number: n.Number, Title: n.Title, URL: n.URL, State: strings.ToLower(n.State), Review: reviewWord(n.Review, blocking, requested), Draft: n.Draft, Head: n.Head, Repo: n.Repository.NameWithOwner}
	if len(n.Commits.Nodes) > 0 && n.Commits.Nodes[0].Commit.Rollup != nil {
		st.Checks = map[string]string{"SUCCESS": "success", "PENDING": "pending", "EXPECTED": "pending", "FAILURE": "failure", "ERROR": "failure"}[n.Commits.Nodes[0].Commit.Rollup.State]
	}
	return st
}

// pr turns the node into the list row shape.
func (n prNode) pr() PR {
	st := n.state()
	p := PR{Number: n.Number, Title: n.Title, URL: n.URL, Head: n.Head, Base: n.Base, Author: n.Author.Login, Draft: n.Draft, Review: st.Review, Checks: st.Checks, Comments: n.Comments}
	p.Blocking, p.Requested = n.reviewers()
	p.UpdatedAt, _ = time.Parse(time.RFC3339, n.UpdatedAt)
	return p
}

// MyPR is a pull request from a search across every repository the
// signed-in user can see.
type MyPR struct {
	PR
	Repo  string // "owner/name"
	State string // "open" | "merged" | "closed"
}

// MyPullRequests is the signed-in user's open pull requests in three
// groups: assigned to them, waiting for their review, and opened by
// them. One GraphQL call with three searches; Login is who "them" is.
type MyPullRequests struct {
	Login           string
	Assigned        []MyPR
	ReviewRequested []MyPR
	Authored        []MyPR
}

const myPRsQuery = `query {
  viewer { login }
  assigned: search(query: "is:pr is:open assignee:@me sort:updated-desc", type: ISSUE, first: 50) { nodes { ... on PullRequest { %[1]s } } }
  requested: search(query: "is:pr is:open review-requested:@me sort:updated-desc", type: ISSUE, first: 50) { nodes { ... on PullRequest { %[1]s } } }
  authored: search(query: "is:pr is:open author:@me sort:updated-desc", type: ISSUE, first: 50) { nodes { ... on PullRequest { %[1]s } } }
}`

// FetchMyPullRequests runs the search; dir only matters for which GitHub
// host gh talks to.
func FetchMyPullRequests(ctx context.Context, dir string) (MyPullRequests, error) {
	raw, err := gh(ctx, dir, "api", "graphql", "-f", "query="+fmt.Sprintf(myPRsQuery, prStateFields))
	if err != nil {
		return MyPullRequests{}, err
	}
	return decodeMyPullRequests(raw)
}

func decodeMyPullRequests(raw []byte) (MyPullRequests, error) {
	var resp struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Assigned  searchNodes `json:"assigned"`
			Requested searchNodes `json:"requested"`
			Authored  searchNodes `json:"authored"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return MyPullRequests{}, err
	}
	if len(resp.Errors) > 0 && resp.Data.Viewer.Login == "" {
		return MyPullRequests{}, errors.New(resp.Errors[0].Message)
	}
	return MyPullRequests{
		Login:           resp.Data.Viewer.Login,
		Assigned:        resp.Data.Assigned.prs(),
		ReviewRequested: resp.Data.Requested.prs(),
		Authored:        resp.Data.Authored.prs(),
	}, nil
}

type searchNodes struct {
	Nodes []prNode `json:"nodes"`
}

func (s searchNodes) prs() []MyPR {
	var out []MyPR
	for _, n := range s.Nodes {
		if n.Number == 0 {
			continue
		}
		out = append(out, MyPR{PR: n.pr(), Repo: n.Repository.NameWithOwner, State: strings.ToLower(n.State)})
	}
	return out
}
