package web

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/domain"
	"starcode/internal/web/views"
)

// The sidebar's select mode acts on several threads at once:
// POST /api/bulk/{action}?ids=a,b,c with action settle, snooze (with
// ?until=, RFC 3339), read or delete, and unsettle and wake for the undo
// of the first two. A thread with a turn running or an approval waiting
// is left out of settle, snooze and delete, as its own buttons are.
func (s *Server) bulkRoutes() {
	s.mux.HandleFunc("POST /api/bulk/{action}", s.bulk)
}

func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var ids []string
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		s.fail(w, r, errors.New("no threads selected"))
		return
	}
	action := r.PathValue("action")
	var until time.Time
	if action == "snooze" {
		var err error
		if until, err = time.Parse(time.RFC3339, r.URL.Query().Get("until")); err != nil {
			s.fail(w, r, errors.New("the snooze time did not come through"))
			return
		}
	}
	var done []string
	skipped := 0
	for _, id := range ids {
		t, err := s.App.Store.Thread(ctx, id)
		if err != nil {
			continue
		}
		busy := t.Status == domain.StatusRunning || t.Status == domain.StatusAwaitingApproval
		switch action {
		case "settle":
			if busy {
				skipped++
				continue
			}
			err = s.App.ArchiveThread(ctx, id)
			if err == nil {
				s.Term.KillPrefix(termPrefix(id))
			}
		case "unsettle":
			err = s.App.UnarchiveThread(ctx, id)
		case "snooze":
			if busy {
				skipped++
				continue
			}
			err = s.App.Snooze(ctx, id, until)
		case "wake":
			err = s.App.Wake(ctx, id)
		case "read":
			s.App.MarkSeen(ctx, id)
		case "delete":
			if busy {
				skipped++
				continue
			}
			err = s.App.DeleteThread(ctx, id)
			if err == nil {
				s.Term.KillPrefix(termPrefix(id))
				if s.AttachDir != "" {
					os.RemoveAll(filepath.Join(s.AttachDir, id))
				}
			}
		default:
			s.fail(w, r, fmt.Errorf("no bulk action %q", action))
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		done = append(done, id)
	}
	sse := datastar.NewSSE(w, r)
	if isUndo(r) {
		return
	}
	sse.MarshalAndPatchSignals(map[string]any{"sel": "", "selecting": false})
	n := fmt.Sprintf("%d threads", len(done))
	if len(done) == 1 {
		n = "1 thread"
	}
	note := ""
	if skipped > 0 {
		note = fmt.Sprintf(" (%d busy, left alone)", skipped)
	}
	undo := func(action string) string {
		return "@post('/api/bulk/" + action + "?undo=1&ids=" + strings.Join(done, ",") + "')"
	}
	switch {
	case len(done) == 0:
		sse.PatchElementTempl(views.Toast("Nothing changed" + note))
	case action == "settle":
		sse.PatchElementTempl(views.UndoToast("Settled "+n+note, time.Time{}, undo("unsettle")))
	case action == "snooze":
		sse.PatchElementTempl(views.UndoToast("Snoozed "+n+note+" until", until, undo("wake")))
	case action == "delete":
		sse.PatchElementTempl(views.Toast("Deleted " + n + note))
	}
}
