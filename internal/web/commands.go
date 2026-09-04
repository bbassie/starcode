package web

import (
	"net/http"
	"strings"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/app"
	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/web/views"
)

// Command handlers. Each one reads the signals Datastar posted, runs the
// command, and answers with either 204 (the SSE stream will show the
// result) or a small SSE response of its own for things that are local to
// the caller: clearing the composer, showing an error, redirecting.

type signals struct {
	Path    string `json:"path"`
	Prompt  string `json:"prompt"`
	Agent   string `json:"agent"`
	Model   string `json:"model"`
	Project string `json:"project"`
	Theme   string `json:"theme"`
	Effort  string `json:"effort"`
	Mode    string `json:"mode"`
	File    string `json:"file"`
}

func (s *Server) readSignals(r *http.Request) signals {
	var sig signals
	datastar.ReadSignals(r, &sig)
	return sig
}

// ok answers a command that has nothing to say: an empty event stream, which
// Datastar closes cleanly (a bare 204 makes it abort the fetch).
func (s *Server) ok(w http.ResponseWriter, r *http.Request) {
	datastar.NewSSE(w, r)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Warn("command failed", "path", r.URL.Path, "err", err)
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.Toast(err.Error()))
}

func (s *Server) addProject(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	if _, err := s.App.AddProject(r.Context(), sig.Path); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"path": ""})
}

func (s *Server) removeProject(w http.ResponseWriter, r *http.Request) {
	if err := s.App.RemoveProject(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) projectFiles(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dir, entries, err := listProjectDir(p.Path, r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files := make([]views.ProjectFile, len(entries))
	for i, entry := range entries {
		files[i] = views.ProjectFile{Name: entry.Name, Path: entry.Path, IsDir: entry.IsDir}
	}
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.FileExplorer(views.ExplorerData{Project: p, Dir: dir, Files: files}))
}

func (s *Server) newThreadForProject(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	s.createThread(w, r, r.PathValue("id"), sig.Agent, sig.Model, sig.Effort, sig.Mode, "")
}

func (s *Server) newThread(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	s.createThread(w, r, sig.Project, sig.Agent, sig.Model, sig.Effort, sig.Mode, sig.Prompt)
}

// createThread makes a thread, applies settings, and when the home
// composer carried a prompt, sends it as the first turn before redirecting.
func (s *Server) createThread(w http.ResponseWriter, r *http.Request, projectID, agent, model, effort, mode, prompt string) {
	if agent == "" {
		agent = "claude"
		if _, ok := s.App.Agents[agent]; !ok {
			agent = s.agentNames()[0]
		}
	}
	id, err := s.App.CreateThread(r.Context(), projectID, agent, model)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if effort != "" || mode != "" {
		if err := s.App.SetThreadSettings(r.Context(), id, app.ThreadSettings{Agent: agent, Model: model, Effort: effort, PermissionMode: mode}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if strings.TrimSpace(prompt) != "" {
		if err := s.App.SendPrompt(r.Context(), id, prompt); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	sse := datastar.NewSSE(w, r)
	sse.Redirect("/threads/" + id)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	if strings.TrimSpace(sig.Prompt) == "" {
		s.ok(w, r)
		return
	}
	if err := s.App.SendPrompt(r.Context(), r.PathValue("id"), sig.Prompt); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": ""})
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.App.Interrupt(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) setThreadSettings(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	st := app.ThreadSettings{Agent: sig.Agent, Model: sig.Model, Effort: sig.Effort, PermissionMode: sig.Mode}
	if err := s.App.SetThreadSettings(r.Context(), r.PathValue("id"), st); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) deleteThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DeleteThread(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.Redirect("/")
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	if err := s.App.ResolveApproval(r.Context(), r.PathValue("id"), r.PathValue("aid"), r.PathValue("decision")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) setTheme(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	for _, t := range Themes {
		if t == sig.Theme {
			http.SetCookie(w, &http.Cookie{Name: "theme", Value: t, Path: "/", SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 365})
			s.ok(w, r)
			return
		}
	}
	w.WriteHeader(http.StatusBadRequest)
}

func (s *Server) gitRefresh(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.GitHeader(views.GitData{Project: p, Status: gitx.Read(r.Context(), p.Path)}))
	sse.PatchElementTempl(views.GitFiles(views.GitData{Project: p, Status: gitx.Read(r.Context(), p.Path)}))
}

func (s *Server) gitDiff(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := r.URL.Query().Get("path")
	if strings.Contains(path, "..") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"gitPath": path, "gitEdit": false, "file": ""})
	sse.PatchElementTempl(views.GitDetail(views.GitData{
		Project:  p,
		Selected: path,
		Diff:     gitx.Diff(r.Context(), p.Path, path),
	}))
}

func (s *Server) gitFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := r.URL.Query().Get("path")
	content, err := readProjectFile(p.Path, path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"gitPath": path, "gitEdit": true, "file": content})
	sse.PatchElementTempl(views.GitDetail(views.GitData{Project: p, Selected: path, Diff: gitx.Diff(r.Context(), p.Path, path)}))
}

func (s *Server) saveGitFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := r.URL.Query().Get("path")
	if err := writeProjectFile(p.Path, path, s.readSignals(r).File); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.GitChanged{})
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"gitPath": path, "gitEdit": false, "file": ""})
	sse.PatchElementTempl(views.GitDetail(views.GitData{Project: p, Selected: path, Diff: gitx.Diff(r.Context(), p.Path, path)}))
}
