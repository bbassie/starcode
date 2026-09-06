package web

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
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
	Path    string       `json:"path"`
	Prompt  string       `json:"prompt"`
	Agent   string       `json:"agent"`
	Model   string       `json:"model"`
	Project string       `json:"project"`
	Theme   string       `json:"theme"`
	Effort  string       `json:"effort"`
	Mode    string       `json:"mode"`
	File    string       `json:"file"`
	Title   string       `json:"title"`
	Attach  []UploadFile `json:"attach"`
}

func (s *Server) readSignals(r *http.Request) signals {
	var sig signals
	datastar.ReadSignals(r, &sig)
	return sig
}

func newSSE(w http.ResponseWriter, r *http.Request) *datastar.ServerSentEventGenerator {
	return datastar.NewSSE(w, r)
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

// uploadFiles saves browser-picked files (base64 in the "upload" signal, put
// there by data-bind on the file input) into the explorer's current
// directory. The signal is cleared in every outcome, so a morphed explorer
// re-running the effect cannot post the same files again.
func (s *Server) uploadFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // decoded cap is checked in saveUploads
	p, err := s.App.Store.Project(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var sig struct {
		Upload []UploadFile `json:"upload"`
	}
	datastar.ReadSignals(r, &sig)
	if len(sig.Upload) == 0 {
		s.ok(w, r)
		return
	}
	saveErr := saveUploads(p.Path, r.URL.Query().Get("path"), sig.Upload)
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"upload": []any{}})
	if saveErr != nil {
		s.Log.Warn("upload failed", "project", p.ID, "err", saveErr)
		sse.PatchElementTempl(views.Toast(saveErr.Error()))
		return
	}
	s.App.Bus.Publish(domain.GitChanged{})
	dir, entries, err := listProjectDir(p.Path, r.URL.Query().Get("path"))
	if err != nil {
		return
	}
	files := make([]views.ProjectFile, len(entries))
	for i, entry := range entries {
		files[i] = views.ProjectFile{Name: entry.Name, Path: entry.Path, IsDir: entry.IsDir}
	}
	sse.PatchElementTempl(views.FileExplorer(views.ExplorerData{Project: p, Dir: dir, Files: files}))
}

func (s *Server) newThreadForProject(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	s.createThread(w, r, r.PathValue("id"), sig.Agent, sig.Model, sig.Effort, sig.Mode, "", nil)
}

func (s *Server) newThread(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // attachments ride the signals as base64
	sig := s.readSignals(r)
	s.createThread(w, r, sig.Project, sig.Agent, sig.Model, sig.Effort, sig.Mode, sig.Prompt, sig.Attach)
}

// createThread makes a thread, applies settings, and when the home
// composer carried a prompt, sends it as the first turn before redirecting.
func (s *Server) createThread(w http.ResponseWriter, r *http.Request, projectID, agent, model, effort, mode, prompt string, attach []UploadFile) {
	if agent == "" {
		agent = views.FirstOr(s.agentNames(), "")
		if agent == "" {
			s.fail(w, r, errors.New("no provider is enabled (see Settings > Providers)"))
			return
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
	if strings.TrimSpace(prompt) != "" || len(attach) > 0 {
		prompt, err := s.attachToPrompt(id, prompt, attach)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.App.SendPrompt(r.Context(), id, prompt); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	sse := datastar.NewSSE(w, r)
	sse.Redirect("/threads/" + id)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // attachments ride the signals as base64
	sig := s.readSignals(r)
	if strings.TrimSpace(sig.Prompt) == "" && len(sig.Attach) == 0 {
		s.ok(w, r)
		return
	}
	prompt, err := s.attachToPrompt(r.PathValue("id"), sig.Prompt, sig.Attach)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.App.SendPrompt(r.Context(), r.PathValue("id"), prompt); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": "", "attach": []any{}})
}

// attachmentFile serves a stored attachment for transcript previews. Both
// path segments are flattened to base names, so the lookup cannot leave the
// attachments directory.
func (s *Server) attachmentFile(w http.ResponseWriter, r *http.Request) {
	tid := filepath.Base(r.PathValue("id"))
	name := filepath.Base(r.PathValue("name"))
	if s.AttachDir == "" || tid == "." || tid == ".." || name == "." || name == ".." {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.AttachDir, tid, name))
}

// attachToPrompt saves the composer's attachments for threadID and appends
// their paths to the prompt text. No attachments returns the text as is.
func (s *Server) attachToPrompt(threadID, text string, files []UploadFile) (string, error) {
	if len(files) == 0 {
		return text, nil
	}
	if s.AttachDir == "" {
		return "", errors.New("attachments are not configured")
	}
	paths, err := saveAttachments(filepath.Join(s.AttachDir, threadID), files)
	if err != nil {
		return "", err
	}
	return promptWithAttachments(text, files, paths), nil
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.App.Interrupt(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) removeQueuedPrompt(w http.ResponseWriter, r *http.Request) {
	if err := s.App.CancelQueuedPrompt(r.Context(), r.PathValue("id"), r.PathValue("qid")); err != nil {
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
	s.Term.KillPrefix(termPrefix(r.PathValue("id")))
	if s.AttachDir != "" {
		os.RemoveAll(filepath.Join(s.AttachDir, r.PathValue("id")))
	}
	sse := datastar.NewSSE(w, r)
	sse.Redirect("/")
}

func (s *Server) archiveThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.ArchiveThread(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) unarchiveThread(w http.ResponseWriter, r *http.Request) {
	if err := s.App.UnarchiveThread(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) renameThread(w http.ResponseWriter, r *http.Request) {
	sig := s.readSignals(r)
	if err := s.App.RenameThread(r.Context(), r.PathValue("id"), sig.Title); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"rename": false})
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
	if !insideProject(path) {
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
	sse.PatchElementTempl(views.GitDetail(views.GitData{Project: p, Selected: path, Diff: gitx.Diff(r.Context(), p.Path, path), Editing: true}))
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

// insideProject rejects paths that could name a file outside the project:
// absolute ones and ones that climb out with "..". git confines tracked
// paths on its own, but the untracked fallback in gitx.Diff runs git diff
// --no-index, which would happily print any file on the machine.
func insideProject(path string) bool {
	clean := filepath.Clean(filepath.FromSlash(path))
	if path == "" || clean == "." || filepath.IsAbs(clean) {
		return false
	}
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
