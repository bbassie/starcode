package web

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"starcode/internal/app"
	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
)

// prCache keeps each project's open pull requests for a minute (the
// composer's "#" menu; gh takes a second per call) and its GitHub
// remotes. one asks watchPRs for a read of a single PR a few seconds
// from now; watchPRs makes it, so it is nil until Watch has run (a send
// then just drops).
type prCache struct {
	mu    sync.Mutex
	items map[string]prEntry
	repos map[string][]string // project path to its remotes, "owner/name"
	one   chan store.PR
	// slowAt is the last full poll that took the slow PRs along.
	slowAt time.Time
}

type prEntry struct {
	at   time.Time
	list gitx.PRList
}

const prTTL = time.Minute

// prList is the cached list for p, read again once stale.
func (s *Server) prList(ctx context.Context, p store.Project) prEntry {
	s.prs.mu.Lock()
	if s.prs.items == nil {
		s.prs.items = map[string]prEntry{}
	}
	e, ok := s.prs.items[p.Path]
	s.prs.mu.Unlock()
	if !ok || time.Since(e.at) > prTTL {
		// Detached from the request: a menu closed mid-fetch would kill
		// gh and leave "signal: killed" in the cache for a minute.
		e = prEntry{at: time.Now(), list: gitx.PullRequests(context.WithoutCancel(ctx), p.Path)}
		if e.list.Err != "" {
			// A failed read is worth retrying on the next open.
			e.at = e.at.Add(-prTTL + 5*time.Second)
		}
		s.prs.mu.Lock()
		s.prs.items[p.Path] = e
		s.prs.mu.Unlock()
	}
	return e
}

// ---- linked pull requests ----

// prPollInterval is how often the linked PRs are read from GitHub. A
// link being made and a turn ending on a linked thread poll that PR at
// once, so the interval only has to catch reviews and merges done
// elsewhere.
const prPollInterval = 5 * time.Minute

// watchPRs keeps pr_state current for every thread's linked pull
// request: a full poll at startup and every prPollInterval, and one PR
// when it gets linked or when a turn on its thread ends (a few seconds
// later, so a push has reached GitHub).
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
				// A watched PR is the watch's to read; reading it now also
				// lets the watch see a push the turn just made.
				if id := s.watcherOf(ctx, pr); id != "" {
					s.App.CheckPRNow(ctx, id)
				} else {
					s.pollPRs(ctx, []store.PR{pr})
				}
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

// watcherOf is a thread that watches pr, or "".
func (s *Server) watcherOf(ctx context.Context, pr store.PR) string {
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return ""
	}
	for _, t := range ts {
		if l, ok := t.Linked(); ok && l == pr && app.Watching(t) {
			return t.ID
		}
	}
	return ""
}

// prSlowEvery is how often the poll reads the PRs nobody is likely to
// look at: closed ones (they can be reopened) and those of settled
// threads only.
const prSlowEvery = time.Hour

// pollRefs picks the linked PRs a full poll reads: not the watched ones
// nor merged ones, and closed ones and those of settled threads only
// when slow.
func pollRefs(ts []store.Thread, before map[store.PR]store.PRState, slow bool) []store.PR {
	watched, live := map[store.PR]bool{}, map[store.PR]bool{}
	var all, refs []store.PR
	for _, t := range ts {
		pr, ok := t.Linked()
		if !ok {
			continue
		}
		if !slices.Contains(all, pr) {
			all = append(all, pr)
		}
		watched[pr] = watched[pr] || app.Watching(t)
		live[pr] = live[pr] || !t.Archived
	}
	for _, pr := range all {
		st, known := before[pr]
		switch {
		case watched[pr], known && st.State == "merged":
		case known && st.State == "closed", !live[pr]:
			if slow {
				refs = append(refs, pr)
			}
		default:
			refs = append(refs, pr)
		}
	}
	return refs
}

// pollPRs reads the state of refs (or of the linked PRs that need it
// when refs is nil) and saves it; a change is announced on the bus so
// sidebars redraw. The full poll leaves out PRs a thread watches (the
// watch keeps their badge) and merged ones, which cannot change; closed
// PRs and those of settled threads only go in every prSlowEvery.
func (s *Server) pollPRs(ctx context.Context, refs []store.PR) {
	before, err := s.App.Store.PRStates(ctx)
	if err != nil {
		return
	}
	if refs == nil {
		ts, err := s.App.Store.Threads(ctx)
		if err != nil {
			return
		}
		s.prs.mu.Lock()
		slow := time.Since(s.prs.slowAt) >= prSlowEvery
		if slow {
			s.prs.slowAt = time.Now()
		}
		s.prs.mu.Unlock()
		refs = pollRefs(ts, before, slow)
	}
	if len(refs) == 0 {
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
			// A merge is what leaves the home checkout behind origin.
			s.Update.ScanHomeSoon()
		}
	}
	if changed {
		s.App.Bus.Publish(domain.PRStateChanged{})
	}
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
// project, so gh picks the host the projects use.
func (s *Server) prDir(ctx context.Context) string {
	ps, err := s.App.Store.Projects(ctx)
	ps = store.Named(ps)
	if err != nil || len(ps) == 0 {
		return ""
	}
	return ps[0].Path
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

func (s *Server) unlinkPR(w http.ResponseWriter, r *http.Request) {
	if err := s.App.UnlinkPR(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// watchPR starts the watch on the thread's linked pull request.
func (s *Server) watchPR(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StartPRWatch(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// checkPR reads the watched pull request now, out of the watch's pace.
func (s *Server) checkPR(w http.ResponseWriter, r *http.Request) {
	if err := s.App.CheckPRNow(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) unwatchPR(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StopPRWatch(r.Context(), r.PathValue("id"), ""); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}
