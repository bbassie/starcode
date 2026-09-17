package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/app"
	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
	"starcode/internal/web/views"
)

// prCache keeps each project's pull request list for a minute: gh takes
// a second per call and the tab is reopened far more often than PRs
// change. ?refresh=1 skips it.
type prCache struct {
	mu      sync.Mutex
	items   map[string]prEntry
	details map[string]prDetailEntry // "project/number"
	mine    myPRsEntry
	repos   map[string][]string // project path to its remotes, "owner/name"
	// refresh asks watchPRs for a full poll of the linked PRs; one asks
	// for a read of a single PR a few seconds from now. watchPRs makes
	// one, so it is nil until Watch has run (a send then just drops).
	refresh chan struct{}
	one     chan store.PR
}

type prEntry struct {
	at   time.Time
	list gitx.PRList
}

type prDetailEntry struct {
	at     time.Time
	detail gitx.PRDetail
	repo   string // the list's repository at the time, for the panel's links
	err    string
}

const prTTL = time.Minute

// prDetailKeep is how long a detail entry stays in the map after it went
// stale. Entries are keyed by PR, so without a sweep every PR ever opened
// would sit there for the life of the process.
const prDetailKeep = 10 * time.Minute

// putDetail stores e under key and drops every entry older than
// prDetailKeep while it holds the lock.
func (s *Server) putDetail(key string, e prDetailEntry) {
	s.prs.mu.Lock()
	defer s.prs.mu.Unlock()
	if s.prs.details == nil {
		s.prs.details = map[string]prDetailEntry{}
	}
	for k, old := range s.prs.details {
		if time.Since(old.at) > prDetailKeep {
			delete(s.prs.details, k)
		}
	}
	s.prs.details[key] = e
}

func (s *Server) pullRequests(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	e := s.prList(r, p)
	tid, linked := s.panelThread(r)
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.PullRequests(views.PRData{Project: p, List: e.list, Loaded: true, ThreadID: tid, Linked: linked}))
}

// panelThread is the thread the side panel belongs to (?thread=) and
// the PR it is linked to, for the link buttons in the PR views.
func (s *Server) panelThread(r *http.Request) (string, store.PR) {
	tid := r.URL.Query().Get("thread")
	if tid == "" {
		return "", store.PR{}
	}
	t, err := s.App.Store.Thread(r.Context(), tid)
	if err != nil {
		return "", store.PR{}
	}
	pr, _ := t.Linked()
	return tid, pr
}

// prList is the cached list for p, refreshed when stale or on ?refresh=1.
func (s *Server) prList(r *http.Request, p store.Project) prEntry {
	s.prs.mu.Lock()
	if s.prs.items == nil {
		s.prs.items = map[string]prEntry{}
	}
	// Keyed by the checkout, not the project: a thread's worktree has a
	// current branch of its own.
	e, ok := s.prs.items[p.Path]
	s.prs.mu.Unlock()
	refresh := r.URL.Query().Get("refresh") != ""
	if !ok || time.Since(e.at) > prTTL || refresh {
		// Detached from the request: a tab switch mid-fetch would kill gh
		// and leave "signal: killed" in the cache for a minute.
		e = prEntry{at: time.Now(), list: gitx.PullRequests(context.WithoutCancel(r.Context()), p.Path)}
		if e.list.Err != "" {
			// A failed read is worth retrying on the next open.
			e.at = e.at.Add(-prTTL + 5*time.Second)
		}
		s.prs.mu.Lock()
		s.prs.items[p.Path] = e
		if refresh {
			// The reader asked for a fresh look; the remotes may have
			// changed too (a fork added, upstream renamed), so reread
			// them next time they are needed.
			delete(s.prs.repos, p.Path)
		}
		s.prs.mu.Unlock()
	}
	return e
}

// prDetail reads one PR, from the cache unless stale or ?refresh=1, and
// returns the repository it lives in. The repository name comes from the
// list, which is fetched first if needed, and is cached with the detail
// so a cached read costs no list call.
func (s *Server) prDetail(r *http.Request, p store.Project) (gitx.PRDetail, string, error) {
	n, err := prNumber(r)
	if err != nil {
		return gitx.PRDetail{}, "", err
	}
	return s.prDetailN(r, p, n, r.URL.Query().Get("refresh") != "")
}

// prNumber is the {n} of the route.
func prNumber(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		return 0, errors.New("bad pull request number")
	}
	return n, nil
}

// prDetailN is prDetail for PR n of p's repository, fresh when refresh
// is set. r is only for the list's cache rules and the context.
func (s *Server) prDetailN(r *http.Request, p store.Project, n int, refresh bool) (gitx.PRDetail, string, error) {
	key := p.ID + "/" + strconv.Itoa(n)
	s.prs.mu.Lock()
	e, ok := s.prs.details[key]
	s.prs.mu.Unlock()
	if ok && time.Since(e.at) < prTTL && !refresh {
		if e.err != "" {
			return gitx.PRDetail{}, e.repo, errors.New(e.err)
		}
		return e.detail, e.repo, nil
	}
	list := s.prList(r, p)
	if list.list.Repo == "" {
		return gitx.PRDetail{}, "", errors.New(list.list.Err)
	}
	// Detached from the request for the same reason as the list.
	d, err := gitx.PullRequestDetail(context.WithoutCancel(r.Context()), p.Path, list.list.Repo, n)
	e = prDetailEntry{at: time.Now(), detail: d, repo: list.list.Repo}
	if err != nil {
		e.err = err.Error()
	}
	s.putDetail(key, e)
	return d, e.repo, err
}

// prDetailRepo reads one PR of any repository the signed-in user can
// see, for the page; cached like the panel's under "repo#number".
func (s *Server) prDetailRepo(ctx context.Context, repo string, n int, refresh bool) (gitx.PRDetail, error) {
	key := repo + "#" + strconv.Itoa(n)
	s.prs.mu.Lock()
	e, ok := s.prs.details[key]
	s.prs.mu.Unlock()
	if ok && time.Since(e.at) < prTTL && !refresh {
		if e.err != "" {
			return gitx.PRDetail{}, errors.New(e.err)
		}
		return e.detail, nil
	}
	d, err := gitx.PullRequestDetail(ctx, s.prDir(ctx), repo, n)
	e = prDetailEntry{at: time.Now(), detail: d, repo: repo}
	if err != nil {
		e.err = err.Error()
	}
	s.putDetail(key, e)
	return d, err
}

// pageRepo is the repository and number a page PR route names.
func pageRepo(r *http.Request) (string, int, error) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		return "", 0, errors.New("bad pull request number")
	}
	return r.PathValue("owner") + "/" + r.PathValue("name"), n, nil
}

// pullRequestPageDetail renders one PR into the page's detail region.
func (s *Server) pullRequestPageDetail(w http.ResponseWriter, r *http.Request) {
	repo, n, err := pageRepo(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.patchPageDetail(datastar.NewSSE(w, r), r, repo, n, r.URL.Query().Get("refresh") != "")
}

// patchPageDetail reads the PR (fresh when refresh is set) and patches
// the page's detail region with it, or with the read error in its place.
func (s *Server) patchPageDetail(sse *datastar.ServerSentEventGenerator, r *http.Request, repo string, n int, refresh bool) {
	d, err := s.prDetailRepo(r.Context(), repo, n, refresh)
	data, derr := s.pageDetailData(r.Context(), repo, n, d)
	if derr != nil {
		sse.PatchElementTempl(views.Toast(derr.Error()))
		return
	}
	if err != nil {
		data.Err = err.Error()
	}
	sse.MarshalAndPatchSignals(map[string]any{"prsel": n})
	sse.PatchElementTempl(views.PRPageDetail(data))
}

// pageDetailData is the page's detail: the project the repository is
// (if any), every project for the picker when it is none, and the threads
// linked to the PR.
func (s *Server) pageDetailData(ctx context.Context, repo string, n int, d gitx.PRDetail) (views.PRDetailData, error) {
	ps, err := s.App.Store.Projects(ctx)
	if err != nil {
		return views.PRDetailData{}, err
	}
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return views.PRDetailData{}, err
	}
	data := views.PRDetailData{Detail: d, Loaded: true, Page: true, Repo: repo, Base: "/api/prs/" + repo + "/" + strconv.Itoa(n), Projects: ps}
	if p, ok := s.projectRepos(ctx, ps)[repo]; ok {
		data.Project, data.ProjectID = p, p.ID
	}
	for _, t := range ts {
		if pr, ok := t.Linked(); ok && pr.Repo == repo && pr.Number == n {
			data.Threads = append(data.Threads, t)
		}
	}
	return data, nil
}

// pullRequestPageThread opens a thread on the PR from the page, in the
// project named by ?project= (the repository's own when it is one).
func (s *Server) pullRequestPageThread(w http.ResponseWriter, r *http.Request) {
	repo, n, err := pageRepo(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.App.Store.Project(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		s.fail(w, r, errors.New("pick a project for the thread"))
		return
	}
	d, err := s.prDetailRepo(r.Context(), repo, n, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.openPRThread(w, r, p, repo, d)
}

func (s *Server) pullRequest(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, err := prNumber(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.patchPanelDetail(datastar.NewSSE(w, r), r, p, n, r.URL.Query().Get("refresh") != "")
}

// patchPanelDetail reads PR n of p (fresh when refresh is set) and
// patches the panel's detail with it, or with the read error.
func (s *Server) patchPanelDetail(sse *datastar.ServerSentEventGenerator, r *http.Request, p store.Project, n int, refresh bool) {
	d, repo, err := s.prDetailN(r, p, n, refresh)
	tid, linked := s.panelThread(r)
	data := views.PRDetailData{Project: p, ProjectID: p.ID, Detail: d, Loaded: true, ThreadID: tid, Linked: linked, Repo: repo, Base: "/api/projects/" + p.ID + "/prs/" + strconv.Itoa(n)}
	if err != nil {
		data.Err = err.Error()
	}
	sse.MarshalAndPatchSignals(map[string]any{"pr": n})
	sse.PatchElementTempl(views.PRDetailView(data))
}

// pullRequestDraft puts the fix prompt for the PR into the page's
// composer, for the reader to edit and send from the thread they are on.
func (s *Server) pullRequestDraft(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d, _, err := s.prDetail(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": prPrompt(r, d), "git": false})
	sse.ExecuteScript("promptFocus()")
}

// prPrompt is the fix prompt, narrowed to one review or comment when the
// request names it (?focus=url).
func prPrompt(r *http.Request, d gitx.PRDetail) string {
	if focus := r.URL.Query().Get("focus"); focus != "" {
		return d.FocusPrompt(focus)
	}
	return d.FixPrompt()
}

// pullRequestThread opens a new thread on the project with the fix prompt
// waiting in its composer, saved as the thread's draft, so the reader can
// adjust the agent or the text before sending.
func (s *Server) pullRequestThread(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d, repo, err := s.prDetail(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.openPRThread(w, r, p, repo, d)
}

// openPRThread makes the thread for pullRequestThread and the page's
// variant: named after the PR, linked to it, the prompt (whole or focused
// on one comment) saved as its draft, then a redirect to it.
func (s *Server) openPRThread(w http.ResponseWriter, r *http.Request, p store.Project, repo string, d gitx.PRDetail) {
	sig := s.readSignals(r)
	agent := sig.Agent
	if agent == "" {
		agent = views.FirstOr(s.agentNames(), "")
	}
	id, err := s.App.CreateThread(r.Context(), p.ID, agent, sig.Model, app.ThreadStart{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	mode := sig.modeOr(s.defaultMode(r.Context(), agent))
	if sig.Effort != "" || mode != "" {
		if err := s.App.SetThreadSettings(r.Context(), id, app.ThreadSettings{Agent: agent, Model: sig.Model, Effort: sig.Effort, PermissionMode: mode}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.App.RenameThread(r.Context(), id, fmt.Sprintf("PR #%d: %s", d.Number, d.Title)); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.App.SaveDraft(r.Context(), id, prPrompt(r, d)); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.App.LinkPR(r.Context(), id, repo, d.Number, d.URL); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.Redirect("/threads/" + id)
}

// ---- linked pull requests ----

// prPollInterval is how often the linked PRs are read from GitHub. A
// link being made and a turn ending on a linked thread poll that PR at
// once, so the interval only has to catch reviews and merges done
// elsewhere.
const prPollInterval = 5 * time.Minute

// watchPRs keeps pr_state current for every thread's linked pull
// request: a full poll at startup and every prPollInterval, one PR when
// it gets linked or when a turn on its thread ends (a few seconds later,
// so a push has reached GitHub), and a full one on demand through
// refreshPRs.
func (s *Server) watchPRs(ctx context.Context) {
	ch := s.App.Bus.Subscribe(ctx)
	one := make(chan store.PR, 16)
	s.prs.mu.Lock()
	s.prs.one = one
	s.prs.mu.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case pr := <-one:
				time.Sleep(4 * time.Second)
				s.pollPRs(ctx, []store.PR{pr})
			}
		}
	}()
	s.pollPRs(ctx, nil)
	tick := time.NewTicker(prPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.pollPRs(ctx, nil)
		case <-s.prs.refresh:
			s.pollPRs(ctx, nil)
		case msg, ok := <-ch:
			if !ok {
				return
			}
			ev, isEvent := msg.(domain.Event)
			if !isEvent {
				continue
			}
			var pr store.PR
			switch p := ev.Payload.(type) {
			case domain.ThreadPRLinked:
				pr = store.PR{Repo: p.Repo, Number: p.Number}
			case domain.TurnCompleted:
				t, err := s.App.Store.Thread(ctx, ev.ThreadID)
				if err != nil {
					continue
				}
				var linked bool
				if pr, linked = t.Linked(); !linked {
					// A thread on a branch of its own may have a PR opened
					// on GitHub by hand; find it by the branch.
					go s.linkByBranch(ctx, t)
					continue
				}
			default:
				continue
			}
			select {
			case one <- pr:
			default:
			}
		}
	}
}

// pollPRs reads the state of refs (or of every linked PR when refs is
// nil) and saves it; a change is announced on the bus so sidebars redraw.
func (s *Server) pollPRs(ctx context.Context, refs []store.PR) {
	if refs == nil {
		ts, err := s.App.Store.Threads(ctx)
		if err != nil {
			return
		}
		seen := map[store.PR]bool{}
		for _, t := range ts {
			if pr, ok := t.Linked(); ok && !seen[pr] {
				seen[pr] = true
				refs = append(refs, pr)
			} else if !ok && t.WorktreeBranch != "" && !t.Archived && time.Since(t.UpdatedAt) < 14*24*time.Hour {
				// Recent worktree threads without a link: one gh call each,
				// so a PR opened on GitHub by hand still finds its thread.
				if s.linkByBranch(ctx, t) {
					if pr, ok := t.Linked(); ok {
						refs = append(refs, pr)
					}
				}
			}
		}
	}
	if len(refs) == 0 {
		return
	}
	before, err := s.App.Store.PRStates(ctx)
	if err != nil {
		return
	}
	var states []store.PRState
	// One call per hundred keeps the query well under GitHub's size limit.
	for len(refs) > 0 {
		batch := refs
		if len(batch) > 100 {
			batch = refs[:100]
		}
		refs = refs[len(batch):]
		gr := make([]gitx.PRRef, len(batch))
		for i, r := range batch {
			gr[i] = gitx.PRRef{Repo: r.Repo, Number: r.Number}
		}
		got, err := gitx.PullRequestStates(ctx, s.prDir(ctx), gr)
		if err != nil {
			s.Log.Debug("poll pull requests", "err", err)
			return
		}
		for _, g := range got {
			states = append(states, store.PRState{PR: store.PR{Repo: g.Repo, Number: g.Number}, Title: g.Title, URL: g.URL, State: g.State, Review: g.Review, Checks: g.Checks, Draft: g.Draft, Head: g.Head, CheckedAt: time.Now()})
		}
	}
	if err := s.App.Store.SavePRStates(ctx, states); err != nil {
		s.Log.Warn("save pull request states", "err", err)
		return
	}
	changed := false
	for _, st := range states {
		old, had := before[st.PR]
		if !had || old.State != st.State || old.Review != st.Review || old.Checks != st.Checks || old.Draft != st.Draft || old.Title != st.Title {
			changed = true
		}
		if had && old.State != "merged" && st.State == "merged" {
			s.settleMerged(ctx, st.PR)
		}
	}
	if changed {
		s.App.Bus.Publish(domain.PRStateChanged{})
	}
}

// linkByBranch links t to the pull request whose head is its worktree
// branch, when GitHub has one. The lookup runs in the project's checkout;
// the worktree may be gone by now. True when a link was made.
func (s *Server) linkByBranch(ctx context.Context, t store.Thread) bool {
	if t.WorktreeBranch == "" || t.PRNumber > 0 {
		return false
	}
	p, err := s.App.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return false
	}
	repo, n, url, ok := gitx.PullRequestForBranch(ctx, p.Path, t.WorktreeBranch)
	if !ok {
		return false
	}
	if err := s.App.LinkPR(ctx, t.ID, repo, n, url); err != nil {
		s.Log.Warn("link pull request by branch", "thread", t.ID, "err", err)
		return false
	}
	s.Log.Info("linked pull request by branch", "thread", t.ID, "branch", t.WorktreeBranch, "pr", n)
	return true
}

// settleMerged archives the idle threads linked to a pull request the
// poll just saw merged: the work is done, and the sidebar is the phone's
// home screen, so a list that trims itself is worth having. Archive is
// reversible and a reply brings a thread back. The settle_merged setting
// turns this off.
func (s *Server) settleMerged(ctx context.Context, pr store.PR) {
	if !s.App.Store.SettleMerged(ctx) {
		return
	}
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return
	}
	for _, t := range ts {
		if ref, ok := t.Linked(); !ok || ref != pr || t.Archived || t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval {
			continue
		}
		if err := s.App.ArchiveThread(ctx, t.ID); err != nil {
			s.Log.Warn("archive merged thread", "thread", t.ID, "err", err)
			continue
		}
		s.Term.KillPrefix(termPrefix(t.ID))
	}
}

// prDir is where gh runs for calls that need no repository: the first
// project, so gh picks the same host it does in the panel.
func (s *Server) prDir(ctx context.Context) string {
	ps, err := s.App.Store.Projects(ctx)
	if err != nil || len(ps) == 0 {
		return ""
	}
	return ps[0].Path
}

// ---- the pull requests page ----

type myPRsEntry struct {
	at   time.Time
	list gitx.MyPullRequests
	err  string
}

// myPRs is the signed-in user's PR search, cached for a minute unless
// refresh is set.
func (s *Server) myPRs(ctx context.Context, refresh bool) myPRsEntry {
	s.prs.mu.Lock()
	e := s.prs.mine
	s.prs.mu.Unlock()
	if e.at.IsZero() || time.Since(e.at) > prTTL || refresh {
		list, err := gitx.FetchMyPullRequests(ctx, s.prDir(ctx))
		e = myPRsEntry{at: time.Now(), list: list}
		if err != nil {
			e.err = err.Error()
		}
		s.prs.mu.Lock()
		s.prs.mine = e
		s.prs.mu.Unlock()
	}
	return e
}

// projectRemotes is every GitHub repository ("owner/name") p's remotes
// point at, read from the git config (no network) and cached: remotes
// rarely change.
func (s *Server) projectRemotes(ctx context.Context, p store.Project) []string {
	s.prs.mu.Lock()
	if s.prs.repos == nil {
		s.prs.repos = map[string][]string{}
	}
	repos, ok := s.prs.repos[p.Path]
	s.prs.mu.Unlock()
	if !ok {
		repos = gitx.Remotes(ctx, p.Path)
		s.prs.mu.Lock()
		s.prs.repos[p.Path] = repos
		s.prs.mu.Unlock()
	}
	return repos
}

// projectRepos maps each GitHub repository a project has as a remote to
// the project. A fork's clone answers for the upstream too, which is
// where its pull requests are.
func (s *Server) projectRepos(ctx context.Context, ps []store.Project) map[string]store.Project {
	out := map[string]store.Project{}
	for _, p := range ps {
		for _, repo := range s.projectRemotes(ctx, p) {
			if _, taken := out[repo]; !taken {
				out[repo] = p
			}
		}
	}
	return out
}

// prsPageData gathers the page: the search, the projects each repository
// belongs to, and the threads linked to each PR.
func (s *Server) prsPageData(ctx context.Context, refresh bool) (views.PRsPageData, error) {
	ps, err := s.App.Store.Projects(ctx)
	if err != nil {
		return views.PRsPageData{}, err
	}
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return views.PRsPageData{}, err
	}
	e := s.myPRs(ctx, refresh)
	d := views.PRsPageData{Mine: e.list, Err: e.err, At: e.at, Loaded: true, Repos: s.projectRepos(ctx, ps), Threads: map[store.PR][]store.Thread{}}
	for _, t := range ts {
		if pr, ok := t.Linked(); ok {
			d.Threads[pr] = append(d.Threads[pr], t)
		}
	}
	return d, nil
}

func (s *Server) prsPage(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "pull requests", views.Page{View: "prs", Theme: s.theme(r), Sidebar: s.sidebarMode(r)}).Render(r.Context(), w)
}

// refreshPRs rereads the search and every linked PR's state.
func (s *Server) refreshPRs(w http.ResponseWriter, r *http.Request) {
	select {
	case s.prs.refresh <- struct{}{}:
	default:
	}
	d, err := s.prsPageData(r.Context(), true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	datastar.NewSSE(w, r).PatchElementTempl(views.PRsList(d))
}

// linkPR ties the thread to the PR named in the query (repo and n), from
// the panel's PR detail or the page.
func (s *Server) linkPR(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil {
		s.fail(w, r, errors.New("bad pull request number"))
		return
	}
	if err := s.App.LinkPR(r.Context(), r.PathValue("id"), r.URL.Query().Get("repo"), n, ""); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) unlinkPR(w http.ResponseWriter, r *http.Request) {
	if err := s.App.UnlinkPR(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// ---- acting on a pull request ----

// prActionSignals are the review card's signals: the text typed in it,
// the merge method picked and whether to merge when the checks pass.
type prActionSignals struct {
	Body   string `json:"prBody"`
	Method string `json:"prMethod"`
	Auto   bool   `json:"prAuto"`
}

// prActionRoutes registers the actions on a pull request: a review
// verdict, a comment, a merge, an update of its branch, taking it out of
// draft, closing it. The literal routes next to them (/thread, /draft)
// win over {action}, as the mux prefers the more specific pattern.
func (s *Server) prActionRoutes() {
	s.mux.HandleFunc("POST /api/prs/{owner}/{name}/{n}/{action}", s.pullRequestPageAction)
	s.mux.HandleFunc("POST /api/projects/{id}/prs/{n}/{action}", s.pullRequestAction)
}

// pullRequestAction runs {action} on PR {n} of the project's repository
// from the panel, then redraws the detail from a fresh read.
func (s *Server) pullRequestAction(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, err := prNumber(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list := s.prList(r, p).list
	if list.Repo == "" {
		s.fail(w, r, errors.New(list.Err))
		return
	}
	if err := s.runPRAction(r, p.Path, list.Repo, n); err != nil {
		s.fail(w, r, err)
		return
	}
	s.forgetPR(p, list.Repo, n)
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prBody": ""})
	s.patchPanelDetail(sse, r, p, n, true)
}

// pullRequestPageAction is pullRequestAction for the page, where the
// repository is in the route. gh runs in the project the repository is
// a remote of when there is one, so it talks to the right host.
func (s *Server) pullRequestPageAction(w http.ResponseWriter, r *http.Request) {
	repo, n, err := pageRepo(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ps, err := s.App.Store.Projects(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, own := s.projectRepos(r.Context(), ps)[repo]
	dir := s.prDir(r.Context())
	if own {
		dir = p.Path
	}
	if err := s.runPRAction(r, dir, repo, n); err != nil {
		s.fail(w, r, err)
		return
	}
	s.forgetPR(p, repo, n)
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prBody": ""})
	s.patchPageDetail(sse, r, repo, n, true)
}

// runPRAction runs the route's {action} with the card's signals through
// gh in dir. A closed or merged PR takes no review or merge, whatever the
// page showed when the button was pressed.
func (s *Server) runPRAction(r *http.Request, dir, repo string, n int) error {
	var sig prActionSignals
	datastar.ReadSignals(r, &sig)
	return gitx.DoPRAction(r.Context(), dir, repo, n, r.PathValue("action"), sig.Body, sig.Method, sig.Auto)
}

// forgetPR drops what is cached about the PR after an action changed it:
// both detail keys (the panel's by project, the page's by repository),
// the project's list and the page's search, so the next read is fresh.
// It also asks for a poll of the PR a few seconds from now, so the chips
// on linked threads follow.
func (s *Server) forgetPR(p store.Project, repo string, n int) {
	s.prs.mu.Lock()
	delete(s.prs.details, repo+"#"+strconv.Itoa(n))
	if p.ID != "" {
		delete(s.prs.details, p.ID+"/"+strconv.Itoa(n))
		delete(s.prs.items, p.ID)
	}
	s.prs.mine = myPRsEntry{}
	one := s.prs.one
	s.prs.mu.Unlock()
	select {
	case one <- store.PR{Repo: repo, Number: n}:
	default:
	}
}
