package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/app"
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
}

type prEntry struct {
	at   time.Time
	list gitx.PRList
}

type prDetailEntry struct {
	at     time.Time
	detail gitx.PRDetail
	err    string
}

const prTTL = time.Minute

func (s *Server) pullRequests(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	e := s.prList(r, p)
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.PullRequests(views.PRData{Project: p, List: e.list, Loaded: true}))
}

// prList is the cached list for p, refreshed when stale or on ?refresh=1.
func (s *Server) prList(r *http.Request, p store.Project) prEntry {
	s.prs.mu.Lock()
	if s.prs.items == nil {
		s.prs.items = map[string]prEntry{}
	}
	e, ok := s.prs.items[p.ID]
	s.prs.mu.Unlock()
	if !ok || time.Since(e.at) > prTTL || r.URL.Query().Get("refresh") != "" {
		e = prEntry{at: time.Now(), list: gitx.PullRequests(r.Context(), p.Path)}
		s.prs.mu.Lock()
		s.prs.items[p.ID] = e
		s.prs.mu.Unlock()
	}
	return e
}

// prDetail reads one PR, from the cache unless stale or ?refresh=1. The
// repository name comes from the list, which is fetched first if needed.
func (s *Server) prDetail(r *http.Request, p store.Project) (gitx.PRDetail, error) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		return gitx.PRDetail{}, errors.New("bad pull request number")
	}
	key := p.ID + "/" + strconv.Itoa(n)
	s.prs.mu.Lock()
	if s.prs.details == nil {
		s.prs.details = map[string]prDetailEntry{}
	}
	e, ok := s.prs.details[key]
	s.prs.mu.Unlock()
	if ok && time.Since(e.at) < prTTL && r.URL.Query().Get("refresh") == "" {
		if e.err != "" {
			return gitx.PRDetail{}, errors.New(e.err)
		}
		return e.detail, nil
	}
	list := s.prList(r, p)
	if list.list.Repo == "" {
		return gitx.PRDetail{}, errors.New(list.list.Err)
	}
	d, err := gitx.PullRequestDetail(r.Context(), p.Path, list.list.Repo, n)
	e = prDetailEntry{at: time.Now(), detail: d}
	if err != nil {
		e.err = err.Error()
	}
	s.prs.mu.Lock()
	s.prs.details[key] = e
	s.prs.mu.Unlock()
	return d, err
}

func (s *Server) pullRequest(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.prDetail(r, p)
	data := views.PRDetailData{Project: p, Detail: d, Loaded: true}
	if err != nil {
		data.Err = err.Error()
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"pr": d.Number})
	sse.PatchElementTempl(views.PRDetailView(data))
}

// pullRequestDraft puts the fix prompt for the PR into the page's
// composer, for the reader to edit and send from the thread they are on.
func (s *Server) pullRequestDraft(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.prDetail(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": d.FixPrompt(), "git": false})
	sse.ExecuteScript("promptFocus()")
}

// pullRequestThread opens a new thread on the project with the fix prompt
// waiting in its composer. The prompt travels as the thread's saved draft
// (see draftSync in the layout), so the page loads with it in place and
// the reader can adjust the agent or the text before sending.
func (s *Server) pullRequestThread(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.prDetail(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sig := s.readSignals(r)
	agent := sig.Agent
	if agent == "" {
		agent = views.FirstOr(s.agentNames(), "")
	}
	id, err := s.App.CreateThread(r.Context(), p.ID, agent, sig.Model)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if sig.Effort != "" || sig.Mode != "" {
		if err := s.App.SetThreadSettings(r.Context(), id, app.ThreadSettings{Agent: agent, Model: sig.Model, Effort: sig.Effort, PermissionMode: sig.Mode}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.App.RenameThread(r.Context(), id, fmt.Sprintf("PR #%d: %s", d.Number, d.Title)); err != nil {
		s.fail(w, r, err)
		return
	}
	draft, _ := json.Marshal(d.FixPrompt())
	sse := datastar.NewSSE(w, r)
	sse.ExecuteScript(fmt.Sprintf("draftPut(%q, %s); location.href = %q", id, draft, "/threads/"+id))
}
