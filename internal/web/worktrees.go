package web

// The home composer's "Start in" choice (views.ProjectPick). Picking "New
// worktree" there fetches GET /api/projects/{id}/branches, which fills the
// base branch select under the row with the project's local and remote
// branches, the default one first and picked.

import (
	"net/http"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/gitx"
	"starcode/internal/web/views"
)

// worktreeRoutes registers the branch list. server.go calls it.
func (s *Server) worktreeRoutes() {
	s.mux.HandleFunc("GET /api/projects/{id}/branches", s.projectBranches)
}

// projectBranches patches the #wt-base select and sets the wtbase signal
// to the default branch, so a thread started without touching the select
// gets the same base the project's default would.
func (s *Server) projectBranches(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.App.Store.Project(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	branches := gitx.Branches(ctx, p.Path)
	def := ""
	if len(branches) > 0 {
		def = branches[0]
	}
	sse := datastar.NewSSE(w, r)
	if err := sse.PatchElementTempl(views.BranchOptions(branches, def)); err != nil {
		return
	}
	sse.MarshalAndPatchSignals(map[string]any{"wtbase": def})
}
