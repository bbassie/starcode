package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The terminal endpoints speak plain SSE and raw bodies instead of Datastar
// signals: output is a byte stream for xterm.js, not a page fragment. A
// thread can have several shells side by side; ?pane=N names one (1 when
// absent) and the session id is "<thread>/<pane>".
// GET  /api/term/{id}/stream  base64 chunks as "out" events, "exit" at the end
// POST /api/term/{id}/input   raw keystrokes
// POST /api/term/{id}/resize  ?cols=&rows=
// POST /api/term/{id}/kill    end the shell so the next stream starts fresh
// GET  /api/term/{id}/panes   JSON list of the panes with a live shell

// termID is the session id for the request's thread and pane.
func termID(r *http.Request) string {
	pane := r.URL.Query().Get("pane")
	if n, err := strconv.Atoi(pane); err != nil || n < 1 || n > 16 {
		pane = "1"
	}
	return r.PathValue("id") + "/" + pane
}

// termPrefix is what every pane id of a thread starts with.
func termPrefix(threadID string) string { return threadID + "/" }

func (s *Server) termPanes(w http.ResponseWriter, r *http.Request) {
	prefix := termPrefix(r.PathValue("id"))
	panes := []int{}
	for _, id := range s.Term.LiveIDs(prefix) {
		if n, err := strconv.Atoi(strings.TrimPrefix(id, prefix)); err == nil {
			panes = append(panes, n)
		}
	}
	sort.Ints(panes)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(panes)
}

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
	sess, err := s.Term.Session(termID(r), dir, cols, rows)
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
	sess := s.Term.Live(termID(r))
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
	sess := s.Term.Live(termID(r))
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
	s.Term.Kill(termID(r))
	w.WriteHeader(http.StatusNoContent)
}
