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
	"sort"
	"strconv"
	"strings"
	"sync"
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

// PRList is the repository's open PRs with the one for the current
// branch first, or why none could be read. The composer's "#" menu
// lists them.
type PRList struct {
	Repo   string // "owner/name"
	Branch string // the working tree's branch
	PRs    []PR
	Err    string
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
		// A cancelled or expired context kills gh; say so instead of
		// "signal: killed", and let callers tell the two apart.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: gh %s", ctx.Err(), strings.Join(args[:min(2, len(args))], " "))
		}
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return nil, errors.New(firstLine(msg))
		}
		return nil, err
	}
	return out.Bytes(), nil
}

// ghGraphQL runs a GraphQL query through gh. An answer with data comes
// back even when gh exits with an error: GitHub answers a batch where one
// alias failed (a deleted repository) with the rest of the data and an
// error naming that alias, and gh fails the whole call over it.
func ghGraphQL(ctx context.Context, dir, query string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("the GitHub CLI (gh) is not installed")
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w: gh api graphql", ctx.Err())
	}
	var partial struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(out.Bytes(), &partial) == nil && len(partial.Data) > 0 && string(partial.Data) != "null" {
		return out.Bytes(), nil
	}
	if msg := strings.TrimSpace(errb.String()); msg != "" {
		return nil, errors.New(firstLine(msg))
	}
	return nil, err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// repoNames keeps the repository each folder belongs to, which gh repo
// view would otherwise ask GitHub for on every list.
var repoNames sync.Map

// PullRequests lists the open PRs of the repository dir belongs to. It
// needs gh on PATH and signed in; anything else lands in Err.
func PullRequests(ctx context.Context, dir string) PRList {
	l := PRList{Branch: Branch(ctx, dir)}
	if v, ok := repoNames.Load(dir); ok {
		l.Repo = v.(string)
	} else {
		repo, err := gh(ctx, dir, "repo", "view", "--json", "nameWithOwner")
		if err != nil {
			l.Err = err.Error()
			return l
		}
		var rv struct {
			NameWithOwner string `json:"nameWithOwner"`
		}
		if err := json.Unmarshal(repo, &rv); err != nil {
			l.Err = "gh repo view: " + err.Error()
			return l
		}
		l.Repo = rv.NameWithOwner
		repoNames.Store(dir, l.Repo)
	}
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

// PRDetail is one PR read in full, with the review state broken out, for
// the watch that wakes an agent when it changes (app.PRWatch).
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
	// HeadSHA is the head commit; Viewer the login gh reads as. Remarks
	// is every comment and submitted review, oldest first, for the watch
	// that wakes an agent on new ones.
	HeadSHA string
	Viewer  string
	Remarks []Remark
	// Activity is the newest of the PR's update time, its head commit,
	// a check finishing and a remark: how lively it is, which sets how
	// often the watch reads it.
	Activity time.Time
}

// Remark is one comment on a pull request: on the conversation, on a
// line of the diff (Path set), or a submitted review (State set).
type Remark struct {
	ID        string
	Author    string
	Body      string
	URL       string
	Path      string
	State     string
	CreatedAt time.Time
	// Role is the author's tie to the repository as GitHub reports it
	// (authorAssociation): OWNER, MEMBER and COLLABORATOR can push.
	Role string
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

// prDetailFields is what the watch reads of a PR. It is lean, since
// every watched PR is read often: the last 30 review threads with their
// last 10 comments, no description, labels or diff size. Five PRs in
// one query cost 2 points of GitHub's 5,000 an hour.
const prDetailFields = `number title url headRefName headRefOid baseRefName isDraft reviewDecision updatedAt state mergeable
      author { login }
      reviewRequests(first: 20) { nodes { requestedReviewer { ... on User { login } } } }
      reviews(last: 30) { nodes { id author { login } authorAssociation state body url submittedAt } }
      comments(last: 30) { nodes { id author { login } authorAssociation body url createdAt } }
      reviewThreads(last: 30) {
        nodes {
          isResolved isOutdated path line originalLine
          comments(last: 10) { nodes { id author { login } authorAssociation body url createdAt } }
        }
      }
      commits(last: 1) {
        nodes { commit { committedDate statusCheckRollup { contexts(first: 100) { nodes {
          __typename
          ... on CheckRun { name status conclusion detailsUrl completedAt }
          ... on StatusContext { context state targetUrl createdAt }
        } } } } }
      }`

// prDetailBatch is how many PRs one query reads. Each asks for about
// 1,000 nodes at most, far under GitHub's 500,000.
const prDetailBatch = 25

// PRRead is one PR of a batch read: its detail, or why it could not be
// read.
type PRRead struct {
	PRRef
	Detail PRDetail
	Err    error
}

// PullRequestDetail reads one PR of repo ("owner/name") in full.
func PullRequestDetail(ctx context.Context, dir, repo string, number int) (PRDetail, error) {
	r := PullRequestDetails(ctx, dir, []PRRef{{Repo: repo, Number: number}})
	return r[0].Detail, r[0].Err
}

// PullRequestDetails reads refs in one GraphQL query per prDetailBatch,
// in the order given. A PR that cannot be read (deleted, no access) gets
// its own error; the others are still read.
func PullRequestDetails(ctx context.Context, dir string, refs []PRRef) []PRRead {
	out := make([]PRRead, len(refs))
	for i, r := range refs {
		out[i].PRRef = r
	}
	for lo := 0; lo < len(refs); lo += prDetailBatch {
		hi := min(lo+prDetailBatch, len(refs))
		readDetails(ctx, dir, out[lo:hi])
	}
	return out
}

func readDetails(ctx context.Context, dir string, rs []PRRead) {
	var q strings.Builder
	q.WriteString("query { viewer { login }")
	for i, r := range rs {
		owner, name, ok := strings.Cut(r.Repo, "/")
		if !ok {
			rs[i].Err = errors.New("unknown repository")
			continue
		}
		fmt.Fprintf(&q, " pr%d: repository(owner: %q, name: %q) { pullRequest(number: %d) { %s } }", i, owner, name, r.Number, prDetailFields)
	}
	q.WriteString(" }")
	raw, err := ghGraphQL(ctx, dir, q.String())
	if err == nil {
		err = decodeDetails(raw, rs)
	}
	if err != nil {
		for i := range rs {
			if rs[i].Err == nil {
				rs[i].Err = err
			}
		}
	}
}

// decodeDetails fills rs from a batch answer; an alias GitHub reported an
// error for gets that error.
func decodeDetails(raw []byte, rs []PRRead) error {
	var resp struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Path    []any  `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if resp.Data == nil {
		if len(resp.Errors) > 0 {
			return errors.New(resp.Errors[0].Message)
		}
		return errors.New("no data in the answer")
	}
	failed := map[string]string{}
	for _, e := range resp.Errors {
		if len(e.Path) > 0 {
			if alias, ok := e.Path[0].(string); ok && failed[alias] == "" {
				failed[alias] = e.Message
			}
		}
	}
	var viewer struct {
		Login string `json:"login"`
	}
	json.Unmarshal(resp.Data["viewer"], &viewer)
	for i := range rs {
		if rs[i].Err != nil {
			continue
		}
		alias := fmt.Sprintf("pr%d", i)
		if msg := failed[alias]; msg != "" {
			rs[i].Err = errors.New(msg)
			continue
		}
		var n struct {
			PullRequest *prDetailNode `json:"pullRequest"`
		}
		if err := json.Unmarshal(resp.Data[alias], &n); err != nil || n.PullRequest == nil {
			rs[i].Err = errors.New("pull request not found")
			continue
		}
		rs[i].Detail = n.PullRequest.detail(viewer.Login)
	}
	return nil
}

func decodePRDetail(raw []byte) (PRDetail, error) {
	var resp struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Repository struct {
				PullRequest *prDetailNode `json:"pullRequest"`
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
	return pr.detail(resp.Data.Viewer.Login), nil
}

// prDetailNode is the GraphQL shape prDetailFields selects.
type prDetailNode struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	Head         string `json:"headRefName"`
	HeadSHA      string `json:"headRefOid"`
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
	Comments struct {
		Nodes []struct {
			ID     string `json:"id"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Role      string `json:"authorAssociation"`
			Body      string `json:"body"`
			URL       string `json:"url"`
			CreatedAt string `json:"createdAt"`
		} `json:"nodes"`
	} `json:"comments"`
	Reviews struct {
		Nodes []struct {
			ID     string `json:"id"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Role        string `json:"authorAssociation"`
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
					ID     string `json:"id"`
					Author struct {
						Login string `json:"login"`
					} `json:"author"`
					Role      string `json:"authorAssociation"`
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
				CommittedAt string `json:"committedDate"`
				Rollup      *struct {
					Contexts struct {
						Nodes []struct {
							Type        string `json:"__typename"`
							Name        string `json:"name"`
							Status      string `json:"status"`
							Conclusion  string `json:"conclusion"`
							DetailsURL  string `json:"detailsUrl"`
							Context     string `json:"context"`
							State       string `json:"state"`
							TargetURL   string `json:"targetUrl"`
							CompletedAt string `json:"completedAt"`
							CreatedAt   string `json:"createdAt"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// detail turns the node into a PRDetail; viewer is the login gh reads as.
func (pr *prDetailNode) detail(viewer string) PRDetail {
	d := PRDetail{
		PR:        PR{Number: pr.Number, Title: pr.Title, URL: pr.URL, Head: pr.Head, Base: pr.Base, Author: pr.Author.Login, Draft: pr.Draft},
		State:     strings.ToLower(pr.State),
		Body:      pr.Body,
		Additions: pr.Additions, Deletions: pr.Deletions, ChangedFiles: pr.ChangedFiles,
		Mergeable: pr.Mergeable, MergeState: pr.MergeState,
		HeadSHA: pr.HeadSHA, Viewer: viewer,
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
	for _, r := range pr.Reviews.Nodes {
		// A pending review is a draft only its author sees; a plain
		// "commented" review without words is the wrapper of line
		// comments, which count on their own below.
		if r.State == "PENDING" || r.State == "COMMENTED" && strings.TrimSpace(r.Body) == "" {
			continue
		}
		rm := Remark{ID: r.ID, Author: r.Author.Login, Body: r.Body, URL: r.URL, State: r.State, Role: r.Role}
		rm.CreatedAt, _ = time.Parse(time.RFC3339, r.SubmittedAt)
		d.Remarks = append(d.Remarks, rm)
	}
	for _, c := range pr.Comments.Nodes {
		rm := Remark{ID: c.ID, Author: c.Author.Login, Body: c.Body, URL: c.URL, Role: c.Role}
		rm.CreatedAt, _ = time.Parse(time.RFC3339, c.CreatedAt)
		d.Remarks = append(d.Remarks, rm)
	}
	for _, t := range pr.ReviewThreads.Nodes {
		for _, c := range t.Comments.Nodes {
			rm := Remark{ID: c.ID, Author: c.Author.Login, Body: c.Body, URL: c.URL, Path: t.Path, Role: c.Role}
			rm.CreatedAt, _ = time.Parse(time.RFC3339, c.CreatedAt)
			d.Remarks = append(d.Remarks, rm)
		}
	}
	sort.SliceStable(d.Remarks, func(i, j int) bool { return d.Remarks[i].CreatedAt.Before(d.Remarks[j].CreatedAt) })
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
			done := c.CompletedAt
			if c.Type == "StatusContext" && ch.Status != "pending" {
				done = c.CreatedAt
			}
			d.active(done)
			if ch.Status != "skipped" {
				d.PR.Checks = worseCheck(d.PR.Checks, ch.Status)
			}
		}
	}
	d.active(pr.UpdatedAt)
	if len(pr.Commits.Nodes) > 0 {
		d.active(pr.Commits.Nodes[0].Commit.CommittedAt)
	}
	for _, r := range d.Remarks {
		if r.CreatedAt.After(d.Activity) {
			d.Activity = r.CreatedAt
		}
	}
	return d
}

// active moves d.Activity up to the time at, an RFC 3339 stamp.
func (d *PRDetail) active(at string) {
	if t, err := time.Parse(time.RFC3339, at); err == nil && t.After(d.Activity) {
		d.Activity = t
	}
}

// Open is whether the PR can still be reviewed and merged.
func (d PRDetail) Open() bool {
	return d.State == "" || d.State == "open"
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
	raw, err := ghGraphQL(ctx, dir, q.String())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data map[string]*struct {
			PullRequest *prNode `json:"pullRequest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	var out []PRState
	for i, r := range refs {
		if n := resp.Data[fmt.Sprintf("pr%d", i)]; n != nil && n.PullRequest != nil {
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

const prStateFields = `number title url state isDraft reviewDecision headRefName repository { nameWithOwner } latestOpinionatedReviews(first: 20) { nodes { author { login } state } } reviewRequests(first: 20) { nodes { requestedReviewer { ... on User { login } } } } commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }`

// prNode is the GraphQL shape prStateFields selects.
type prNode struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	State      string `json:"state"` // OPEN, CLOSED, MERGED
	Draft      bool   `json:"isDraft"`
	Review     string `json:"reviewDecision"`
	Head       string `json:"headRefName"`
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
