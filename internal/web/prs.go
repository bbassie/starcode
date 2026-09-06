package web

import (
	"net/http"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/gitx"
	"starcode/internal/web/views"
)

// prCache keeps each project's pull request list for a minute: gh takes
// a second per call and the tab is reopened far more often than PRs
// change. ?refresh=1 skips it.
type prCache struct {
	mu    sync.Mutex
	items map[string]prEntry
}

type prEntry struct {
	at   time.Time
	list gitx.PRList
}

const prTTL = time.Minute

func (s *Server) pullRequests(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
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
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.PullRequests(views.PRData{Project: p, List: e.list, Loaded: true}))
}
