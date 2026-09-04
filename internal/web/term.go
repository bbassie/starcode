package web

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The terminal endpoints speak plain SSE and raw bodies instead of Datastar
// signals: output is a byte stream for xterm.js, not a page fragment.
// GET  /api/term/{id}/stream  base64 chunks as "out" events, "exit" at the end
// POST /api/term/{id}/input   raw keystrokes
// POST /api/term/{id}/resize  ?cols=&rows=
// POST /api/term/{id}/kill    end the shell so the next stream starts fresh

func (s *Server) termProjectDir(r *http.Request) (string, error) {
	t, err := s.App.Store.Thread(r.Context(), r.PathValue("id"))
	if err != nil {
		return "", err
	}
	p, err := s.App.Store.Project(r.Context(), t.ProjectID)
	if err != nil {
		return "", err
	}
	return p.Path, nil
}

func (s *Server) termStream(w http.ResponseWriter, r *http.Request) {
	dir, err := s.termProjectDir(r)
	if err != nil {
		http.Error(w, "unknown thread", http.StatusNotFound)
		return
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	sess, err := s.Term.Session(r.PathValue("id"), dir, cols, rows)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")

	replay, ch, cancel := sess.Subscribe()
	defer cancel()
	send := func(event string, data []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, base64.StdEncoding.EncodeToString(data))
		flusher.Flush()
	}
	send("out", replay)

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": hb\n\n")
			flusher.Flush()
		case chunk, ok := <-ch:
			if !ok {
				// Shell exited, or this subscriber lagged and lost its
				// channel; either way the client reconnects (or stops, on
				// "exit") and the replay puts it back in sync.
				if sess.Exited() {
					send("exit", nil)
				}
				return
			}
			send("out", chunk)
		}
	}
}

func (s *Server) termInput(w http.ResponseWriter, r *http.Request) {
	sess := s.Term.Live(r.PathValue("id"))
	if sess == nil {
		http.Error(w, "no terminal session", http.StatusConflict)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := sess.Write(data); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) termResize(w http.ResponseWriter, r *http.Request) {
	sess := s.Term.Live(r.PathValue("id"))
	if sess == nil {
		http.Error(w, "no terminal session", http.StatusConflict)
		return
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	if err := sess.Resize(cols, rows); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) termKill(w http.ResponseWriter, r *http.Request) {
	s.Term.Kill(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}
