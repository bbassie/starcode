package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/push"
	"starcode/internal/store"
)

// Web Push: a browser that turns notifications on subscribes with this
// instance's key, and the server pushes to it when a thread finishes a
// turn, waits on the reader, or its agent calls notify. That reaches a
// phone with starcode closed. A thread on screen in a page the reader is
// looking at gets no push; the page says when it is in view (presence).

func (s *Server) pushRoutes() {
	s.read("/api/push/key", s.pushKey)
	s.mux.HandleFunc("POST /api/push/subscribe", s.pushSubscribe)
	s.mux.HandleFunc("POST /api/push/unsubscribe", s.pushUnsubscribe)
	s.mux.HandleFunc("POST /api/presence", s.setPresence)
	s.mux.HandleFunc("POST /api/threads/{id}/notify", s.setNotifyLevel)
}

func (s *Server) pushKey(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		http.Error(w, "push is off", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]string{"key": s.Push.PublicKey()})
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	var sub push.Subscription
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&sub); err != nil || !strings.HasPrefix(sub.Endpoint, "https://") || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}
	if err := s.App.Store.SavePushSub(r.Context(), store.PushSub{Endpoint: sub.Endpoint, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth, Agent: r.UserAgent()}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var sub push.Subscription
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&sub)
	if sub.Endpoint != "" {
		s.App.Store.DeletePushSub(r.Context(), sub.Endpoint)
	}
	w.WriteHeader(http.StatusNoContent)
}

// setNotifyLevel is the header menu's choice of what the thread pushes.
func (s *Server) setNotifyLevel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	level := r.URL.Query().Get("level")
	if level != store.NotifyAll && level != store.NotifyInput && level != store.NotifyOff {
		s.fail(w, r, errors.New("level is all, input or off"))
		return
	}
	if _, err := s.App.Store.Thread(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.App.Store.SetSetting(r.Context(), "notify:"+id, level); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.NotifyLevelChanged{ThreadID: id})
	s.ok(w, r)
}

// presence is which pages are in view, keyed by the page id the layout
// makes on load. A page's stream adds it and takes it away when it ends;
// the page reports every change of visibility in between.
type presence struct {
	mu    sync.Mutex
	pages map[string]*page
}

type page struct {
	thread  string
	visible bool
	streams int
}

func (p *presence) open(pid, thread string, visible bool) {
	if pid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pages == nil {
		p.pages = map[string]*page{}
	}
	pg := p.pages[pid]
	if pg == nil {
		pg = &page{}
		p.pages[pid] = pg
	}
	pg.thread, pg.visible = thread, visible
	pg.streams++
}

func (p *presence) close(pid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pg := p.pages[pid]; pg != nil {
		if pg.streams--; pg.streams <= 0 {
			delete(p.pages, pid)
		}
	}
}

func (p *presence) set(pid string, visible bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pg := p.pages[pid]; pg != nil {
		pg.visible = visible
	}
}

// watching is whether a page in view shows thread id.
func (p *presence) watching(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pg := range p.pages {
		if pg.visible && pg.thread == id {
			return true
		}
	}
	return false
}

func (s *Server) setPresence(w http.ResponseWriter, r *http.Request) {
	s.presence.set(r.URL.Query().Get("pid"), r.URL.Query().Get("visible") == "1")
	s.noteZone(r.Context(), r.URL.Query().Get("tz"))
	w.WriteHeader(http.StatusNoContent)
}

// watchPush turns thread events into pushes until ctx ends.
func (s *Server) watchPush(ctx context.Context) {
	ch := s.App.Bus.Subscribe(ctx)
	// last is each thread's status as of the last event, so a change to
	// idle can tell a turn that ended from a session that closed.
	last := map[string]string{}
	if ts, err := s.App.Store.Threads(ctx); err == nil {
		for _, t := range ts {
			last[t.ID] = t.Status
		}
	}
	for msg := range ch {
		switch m := msg.(type) {
		case bus.Resync:
			if ts, err := s.App.Store.Threads(ctx); err == nil {
				for _, t := range ts {
					last[t.ID] = t.Status
				}
			}
		case domain.AgentNotified:
			s.pushThread(ctx, m.ThreadID, m.Text, true)
		case domain.Event:
			p, ok := m.Payload.(domain.ThreadStatusChanged)
			if !ok {
				continue
			}
			before := last[m.ThreadID]
			last[m.ThreadID] = p.Status
			switch {
			case p.Status == domain.StatusAwaitingApproval && before != p.Status:
				s.pushThread(ctx, m.ThreadID, s.waitingWhat(ctx, m.ThreadID), true)
			case (p.Status == domain.StatusIdle || p.Status == domain.StatusError) && (before == domain.StatusRunning || before == domain.StatusAwaitingApproval):
				body := "finished"
				if p.Status == domain.StatusError {
					body = "stopped with an error"
					if p.Detail != "" {
						body += ": " + p.Detail
					}
				} else if reply := s.App.LastReply(ctx, m.ThreadID); reply != "" {
					body = reply
				}
				s.pushThread(ctx, m.ThreadID, body, false)
			}
		}
	}
}

// waitingWhat says what a thread waiting on the reader asks for.
func (s *Server) waitingWhat(ctx context.Context, id string) string {
	aps, err := s.App.Store.PendingApprovals(ctx, id)
	if err != nil || len(aps) == 0 {
		return "needs your approval"
	}
	ap := aps[len(aps)-1]
	if qs := domain.Questions(ap.ToolName, ap.Input); len(qs) > 0 {
		return "asks: " + qs[0].Question
	}
	what := ap.ToolName
	if ap.Description != "" {
		what += ": " + ap.Description
	}
	return "needs your approval for " + what
}

// pushThread sends body about thread id to every subscribed device, as
// far as the thread's notify level lets it. input is something that waits
// on the reader (an approval, a question, the agent's notify), which even
// the "input" level sends.
func (s *Server) pushThread(ctx context.Context, id, body string, input bool) {
	if s.Push == nil {
		return
	}
	t, err := s.App.Store.Thread(ctx, id)
	if err != nil {
		return
	}
	switch s.App.Store.NotifyLevel(ctx, id) {
	case store.NotifyOff:
		return
	case store.NotifyInput:
		if !input {
			return
		}
	}
	// A child that reports to its parent is the parent's business, not
	// the reader's, until it needs the reader.
	if !input && t.Parent != "" && t.WakeParent {
		return
	}
	if s.presence.watching(id) {
		return
	}
	subs, err := s.App.Store.PushSubs(ctx)
	if err != nil || len(subs) == 0 {
		return
	}
	msg := push.Message{Title: t.Title, Body: clipRunes(strings.Join(strings.Fields(body), " "), 180), URL: "/threads/" + id, Tag: "thread-" + id}
	for _, sub := range subs {
		go func(sub store.PushSub) {
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var ps push.Subscription
			ps.Endpoint = sub.Endpoint
			ps.Keys.P256dh, ps.Keys.Auth = sub.P256dh, sub.Auth
			switch err := s.Push.Send(sctx, ps, msg, input); {
			case errors.Is(err, push.ErrGone):
				s.App.Store.DeletePushSub(sctx, sub.Endpoint)
			case err != nil:
				s.Log.Warn("push", "endpoint", hostOf(sub.Endpoint), "err", err)
			}
		}(sub)
	}
}

func hostOf(u string) string {
	u = strings.TrimPrefix(u, "https://")
	h, _, _ := strings.Cut(u, "/")
	return h
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
