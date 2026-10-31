package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"starcode/internal/app"
	"starcode/internal/domain"
	"starcode/internal/store"
	"starcode/internal/web/views"
)

// The command line (`starcode thread`, internal/cli) reaches the running
// instance on the agent socket, under /cli/. It needs no thread key:
// the socket is the user's own (mode 0600 in the data dir), and whoever
// can open it can read the database next to it anyway.

// CLIThread is a thread as the command line lists it.
type CLIThread struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	State   string    `json:"state"`
	Project string    `json:"project"`
	Agent   string    `json:"agent"`
	Updated time.Time `json:"updated"`
}

// CLIMessage is a prompt or a reply.
type CLIMessage struct {
	Role string    `json:"role"` // "prompt", "agent" or "error"
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// CLIShow is a thread with its latest messages, and the last reply when
// it is done.
type CLIShow struct {
	CLIThread
	Detail   string       `json:"detail,omitempty"`
	Messages []CLIMessage `json:"messages"`
}

// CLIStart is what `starcode thread start` sends. Project is an id, a
// name or a path inside the project's folder; "none" starts a thread
// with a folder of its own.
type CLIStart struct {
	Project string `json:"project"`
	Prompt  string `json:"prompt"`
	Title   string `json:"title,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// CLISend is a message for a thread; Steer hands it to a running turn
// instead of queueing it after.
type CLISend struct {
	Prompt string `json:"prompt"`
	Steer  bool   `json:"steer,omitempty"`
}

func (s *Server) cliRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /cli/threads", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.cliList(r.Context(), r.URL.Query().Get("project"))
		cliReply(w, v, err)
	})
	mux.HandleFunc("POST /cli/threads", func(w http.ResponseWriter, r *http.Request) {
		var a CLIStart
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&a); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v, err := s.cliStart(r.Context(), a)
		cliReply(w, v, err)
	})
	mux.HandleFunc("POST /cli/threads/{id}/send", func(w http.ResponseWriter, r *http.Request) {
		var a CLISend
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&a); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := threadIDOf(r.PathValue("id"))
		send := s.App.SendPrompt
		if a.Steer {
			send = s.App.SteerPrompt
		}
		if err := send(r.Context(), id, a.Prompt); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"id": id})
	})
	mux.HandleFunc("GET /cli/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("messages"))
		v, err := s.cliShow(r.Context(), threadIDOf(r.PathValue("id")), n)
		cliReply(w, v, err)
	})
	// wait blocks until the thread stops running, up to ?timeout=
	// seconds (default 600); the answer says whether it did.
	mux.HandleFunc("POST /cli/threads/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		secs, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
		if secs <= 0 || secs > 3600 {
			secs = 600
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(secs)*time.Second)
		defer cancel()
		id := threadIDOf(r.PathValue("id"))
		_, err := s.App.AwaitThread(ctx, id)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]bool{"done": err == nil})
	})
}

// cliReply writes v, or the error as a 400.
func cliReply[T any](w http.ResponseWriter, v T, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, v)
}

// cliState is a thread's state in one word: settled, snoozed, or its
// status.
func cliState(t store.Thread) string {
	switch {
	case t.Archived:
		return "settled"
	case t.Snoozed():
		return "snoozed"
	}
	return t.Status
}

// cliProject finds the project ref names: an id, a name (any case), or
// a path inside a project's folder, the deepest one winning.
func (s *Server) cliProject(ctx context.Context, ref string) (store.Project, error) {
	ps, err := s.App.Store.Projects(ctx)
	if err != nil {
		return store.Project{}, err
	}
	for _, p := range ps {
		if p.ID == ref {
			return p, nil
		}
	}
	for _, p := range store.Named(ps) {
		if strings.EqualFold(p.Name, ref) {
			return p, nil
		}
	}
	var best store.Project
	if filepath.IsAbs(ref) {
		for _, p := range ps {
			if (ref == p.Path || strings.HasPrefix(ref, p.Path+string(filepath.Separator))) && len(p.Path) > len(best.Path) {
				best = p
			}
		}
	}
	if best.ID == "" {
		return best, fmt.Errorf("no project %q (an id, a name, or a path inside a project's folder)", ref)
	}
	return best, nil
}

func (s *Server) cliList(ctx context.Context, project string) ([]CLIThread, error) {
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := s.App.Store.Projects(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, p := range ps {
		names[p.ID] = p.Name
	}
	only := ""
	if project != "" {
		p, err := s.cliProject(ctx, project)
		if err != nil {
			return nil, err
		}
		only = p.ID
	}
	out := []CLIThread{}
	for _, t := range ts {
		if only != "" && t.ProjectID != only {
			continue
		}
		out = append(out, cliThread(t, names[t.ProjectID]))
	}
	return out, nil
}

func cliThread(t store.Thread, project string) CLIThread {
	return CLIThread{ID: t.ID, Title: t.Title, State: cliState(t), Project: project, Agent: t.Agent, Updated: t.UpdatedAt}
}

// cliStart makes a thread the way the new-thread page does: the
// project's own defaults when it names them, else the first agent in
// its default permission mode; then it sends the prompt.
func (s *Server) cliStart(ctx context.Context, a CLIStart) (CLIThread, error) {
	if strings.TrimSpace(a.Prompt) == "" {
		return CLIThread{}, errors.New("the message is empty")
	}
	var p store.Project
	if a.Project != "none" {
		var err error
		if p, err = s.cliProject(ctx, a.Project); err != nil {
			return CLIThread{}, err
		}
		if a.Agent == "" && p.HasDefaults() {
			if _, ok := s.App.Agent(p.Agent); ok {
				a.Agent, a.Model, a.Effort, a.Mode = p.Agent, p.Model, p.Effort, p.Mode
			}
		}
	}
	if a.Agent == "" {
		a.Agent = views.FirstOr(s.agentNames(), "")
		if a.Agent == "" {
			return CLIThread{}, errors.New("no provider is enabled (see Settings > Providers)")
		}
	}
	if _, ok := s.App.Agent(a.Agent); !ok {
		return CLIThread{}, fmt.Errorf("unknown agent %q; these are enabled: %s", a.Agent, strings.Join(s.agentNames(), ", "))
	}
	if a.Mode == "" {
		a.Mode = s.defaultMode(ctx, a.Agent)
	}
	var id string
	var err error
	if p.ID == "" {
		id, err = s.App.CreateScratchThread(ctx, a.Agent, a.Model, a.Prompt)
	} else {
		id, err = s.App.CreateThread(ctx, p.ID, a.Agent, a.Model)
	}
	if err != nil {
		return CLIThread{}, err
	}
	if a.Effort != "" || a.Mode != "" {
		if err := s.App.SetThreadSettings(ctx, id, app.ThreadSettings{Agent: a.Agent, Model: a.Model, Effort: a.Effort, PermissionMode: a.Mode}); err != nil {
			return CLIThread{}, err
		}
	}
	if title := clipTitle(a.Title); title != "" {
		if err := s.App.RenameThread(ctx, id, title); err != nil {
			return CLIThread{}, err
		}
	}
	if err := s.App.SendPrompt(ctx, id, a.Prompt); err != nil {
		return CLIThread{}, err
	}
	t, err := s.App.Store.Thread(ctx, id)
	if err != nil {
		return CLIThread{}, err
	}
	pp, _ := s.App.Store.Project(ctx, t.ProjectID)
	return cliThread(t, pp.Name), nil
}

// cliShow is the thread and its last n prompts and replies (default 5).
func (s *Server) cliShow(ctx context.Context, id string, n int) (CLIShow, error) {
	t, err := s.App.Store.Thread(ctx, id)
	if err != nil {
		return CLIShow{}, fmt.Errorf("no thread %q", id)
	}
	if n <= 0 {
		n = 5
	}
	p, _ := s.App.Store.Project(ctx, t.ProjectID)
	items, err := s.App.Store.Items(ctx, id)
	if err != nil {
		return CLIShow{}, err
	}
	out := CLIShow{CLIThread: cliThread(t, p.Name), Messages: []CLIMessage{}}
	if t.Status == domain.StatusError || t.Status == domain.StatusAwaitingApproval {
		out.Detail = s.App.ThreadState(t)
	}
	for _, it := range items {
		body := strings.TrimSpace(it.Body)
		if body == "" || views.ParentOf(it) != "" {
			continue
		}
		role := ""
		switch it.Kind {
		case domain.KindUser:
			role = "prompt"
		case domain.KindAssistant:
			role = "agent"
		case domain.KindError:
			role = "error"
		default:
			continue
		}
		out.Messages = append(out.Messages, CLIMessage{Role: role, Text: body, At: it.CreatedAt})
	}
	if len(out.Messages) > n {
		out.Messages = out.Messages[len(out.Messages)-n:]
	}
	return out, nil
}
