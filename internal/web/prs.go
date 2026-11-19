package web

import (
	"context"
	"net/http"
	"sync"
	"time"

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

func (s *Server) unwatchPR(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StopPRWatch(r.Context(), r.PathValue("id"), ""); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}
