package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/domain"
	"starcode/internal/store"
	"starcode/internal/web/views"
)

func (s *Server) scheduleRoutes() {
	s.mux.HandleFunc("POST /api/threads/{id}/later", s.sendLater)
	s.mux.HandleFunc("POST /api/schedules", s.addSchedule)
	s.mux.HandleFunc("POST /api/schedules/{id}/remove", s.scheduleCmd(func(ctx context.Context, id string) error { return s.App.RemoveSchedule(ctx, id) }))
	s.mux.HandleFunc("POST /api/schedules/{id}/pause", s.scheduleCmd(func(ctx context.Context, id string) error { return s.App.PauseSchedule(ctx, id, true) }))
	s.mux.HandleFunc("POST /api/schedules/{id}/resume", s.scheduleCmd(func(ctx context.Context, id string) error { return s.App.PauseSchedule(ctx, id, false) }))
	s.mux.HandleFunc("POST /api/schedules/{id}/run", s.scheduleCmd(func(ctx context.Context, id string) error { return s.App.RunSchedule(ctx, id) }))
}

func (s *Server) schedulesPage(w http.ResponseWriter, r *http.Request) {
	s.page(r.Context(), "schedules", views.Page{View: "schedules", Theme: s.theme(r), Sidebar: s.sidebarMode(r), Dense: s.denseMode(r)}).Render(r.Context(), w)
}

func (s *Server) schedulesData(ctx context.Context, side views.SidebarData) views.SchedulesData {
	scs, err := s.App.Store.Schedules(ctx, "")
	if err != nil {
		s.Log.Warn("read schedules", "err", err)
	}
	threads := map[string]store.Thread{}
	for _, t := range side.Threads {
		threads[t.ID] = t
	}
	return views.SchedulesData{Schedules: scs, Projects: store.Named(side.Projects), Threads: threads, Agents: s.agentNames(), Looks: s.agentLooks()}
}

func (s *Server) scheduleCmd(run func(ctx context.Context, id string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := run(r.Context(), r.PathValue("id")); err != nil {
			s.fail(w, r, err)
			return
		}
		s.ok(w, r)
	}
}

// sendLater schedules the composer's prompt, attachments and all, for the
// time the browser worked out, and empties the composer.
func (s *Server) sendLater(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	id := r.PathValue("id")
	at, err := time.Parse(time.RFC3339, r.URL.Query().Get("at"))
	if err != nil {
		s.fail(w, r, errors.New("the time did not come through"))
		return
	}
	sig := s.readSignals(r)
	if strings.TrimSpace(sig.Prompt) == "" && len(sig.Attach) == 0 {
		s.fail(w, r, errors.New("write the message first"))
		return
	}
	prompt, err := s.attachToPrompt(id, sig.Prompt, sig.Attach)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sid, err := s.App.SetSchedule(r.Context(), domain.ScheduleSet{ThreadID: id, Prompt: prompt, At: at})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.SaveDraft(r.Context(), id, "")
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"prompt": "", "_attach": []any{}})
	sse.PatchElementTempl(views.UndoToast("Sends", at, "@post('/api/schedules/"+sid+"/remove')"))
}

// addSchedule is the Schedules page's form: a recurring prompt that opens
// a thread in a project on every run. The thread runs in the agent's mode
// that never asks, as a new thread from the composer would, since nobody
// is there to answer when it starts.
func (s *Server) addSchedule(w http.ResponseWriter, r *http.Request) {
	var sig struct {
		Project string `json:"schedProject"`
		Prompt  string `json:"schedPrompt"`
		Cron    string `json:"schedCron"`
		Title   string `json:"schedTitle"`
		Agent   string `json:"schedAgent"`
	}
	datastar.ReadSignals(r, &sig)
	s.noteZone(r.Context(), r.URL.Query().Get("tz"))
	sc := domain.ScheduleSet{ProjectID: sig.Project, Prompt: sig.Prompt, Cron: sig.Cron, TZ: r.URL.Query().Get("tz"), Title: strings.TrimSpace(sig.Title), Agent: sig.Agent}
	if sc.Cron == "" {
		s.fail(w, r, errors.New("say when, as a cron"))
		return
	}
	if sc.Agent != "" {
		sc.Mode = s.defaultMode(r.Context(), sc.Agent)
	}
	if _, err := s.App.SetSchedule(r.Context(), sc); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]any{"schedPrompt": "", "schedTitle": ""})
}

// noteZone records the reader's time zone as a browser names it, for the
// schedules agents make (userZone).
func (s *Server) noteZone(ctx context.Context, tz string) {
	if tz == "" || len(tz) > 64 || tz == s.userZone(ctx) {
		return
	}
	if _, err := time.LoadLocation(tz); err == nil {
		s.App.Store.SetSetting(ctx, "user_tz", tz)
	}
}
