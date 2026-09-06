package web

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/gitx"
	"starcode/internal/store"
	"starcode/internal/web/views"
)

const searchLimit = 12

// search answers the palette: commands that fit the page, threads by
// title and messages by text, all filtered by q.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	view, threadID := r.URL.Query().Get("view"), r.URL.Query().Get("id")
	ps, err := s.App.Store.Projects(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byID := map[string]store.Project{}
	for _, p := range ps {
		byID[p.ID] = p
	}
	threads, err := s.App.Store.SearchThreads(ctx, q, searchLimit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hits, err := s.App.Store.SearchItems(ctx, q, searchLimit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := views.SearchData{Query: q, Threads: threads, Hits: hits, Projects: byID, Looks: s.agentLooks()}
	// On a thread, file names of its project are searchable too; a hit
	// opens the file in the side panel's editor.
	if view == "thread" && q != "" {
		if t, err := s.App.Store.Thread(ctx, threadID); err == nil {
			if p, ok := byID[t.ProjectID]; ok {
				d.Files = gitx.MatchFiles(s.projectFileList(ctx, p), q, 8)
				d.FileProject = p
			}
		}
	}
	for _, c := range s.commands(ctx, view, threadID, ps) {
		if q == "" || strings.Contains(strings.ToLower(c.Label), strings.ToLower(q)) {
			d.Commands = append(d.Commands, c)
		}
	}
	if q == "" && len(d.Commands) > 6 {
		d.Commands = d.Commands[:6]
	}
	sse := datastar.NewSSE(w, r)
	sse.PatchElementTempl(views.PaletteResults(d))
}

// commands lists what the palette can do on this page. The current
// thread's actions and its project's "new thread" come first.
func (s *Server) commands(ctx context.Context, view, threadID string, ps []store.Project) []views.Command {
	var out []views.Command
	var t store.Thread
	if view == "thread" {
		t, _ = s.App.Store.Thread(ctx, threadID)
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].ID == t.ProjectID && ps[j].ID != t.ProjectID })
	for _, p := range ps {
		c := views.Command{Label: "New thread in " + p.Name, Icon: "plus", Action: "@post('/api/projects/" + p.ID + "/threads')"}
		if p.ID == t.ProjectID {
			c.Key = "mod+shift+o"
		}
		out = append(out, c)
	}
	if t.ID != "" {
		out = append(out,
			views.Command{Label: "Rename thread", Icon: "pencil", Action: "$title = " + jsq(t.Title) + "; $rename = true; renameFocus()"},
		)
		if t.Archived {
			out = append(out, views.Command{Label: "Unarchive thread", Icon: "archive-restore", Action: "@post('/api/threads/" + t.ID + "/unarchive')"})
		} else {
			out = append(out, views.Command{Label: "Archive thread", Icon: "archive", Action: "@post('/api/threads/" + t.ID + "/archive')"})
		}
		out = append(out,
			views.Command{Label: "Toggle terminal", Icon: "terminal", Action: "$term = !$term", Key: "mod+`"},
			views.Command{Label: "Toggle changes panel", Icon: "git-compare-arrows", Action: "$git = !$git", Key: "mod+shift+g"},
			views.Command{Label: "Delete thread", Icon: "trash-2", Action: "confirm('Delete this thread?') && @post('/api/threads/" + t.ID + "/delete')"},
		)
	}
	out = append(out,
		views.Command{Label: "Projects overview", Icon: "star", Href: "/"},
		views.Command{Label: "Settings", Icon: "settings", Href: "/settings", Key: "mod+,"},
		views.Command{Label: "Settings: usage", Icon: "chart-no-axes-combined", Href: "/settings/usage"},
		views.Command{Label: "Settings: providers", Icon: "plug", Href: "/settings/providers"},
		views.Command{Label: "Keyboard shortcuts", Icon: "keyboard", Action: "$help = true", Key: "mod+/"},
	)
	for _, th := range Themes {
		out = append(out, views.Command{Label: "Theme: " + views.ThemeName(th), Icon: "palette", Action: "$theme = " + jsq(th) + "; @post('/api/theme')"})
	}
	return out
}

func jsq(s string) string { return views.JSQ(s) }

// projectFileList is gitx.ListFiles behind a short cache: the palette
// asks on every keystroke.
func (s *Server) projectFileList(ctx context.Context, p store.Project) []string {
	s.files.mu.Lock()
	if s.files.items == nil {
		s.files.items = map[string]fileListEntry{}
	}
	e, ok := s.files.items[p.ID]
	s.files.mu.Unlock()
	if ok && time.Since(e.at) < 30*time.Second {
		return e.files
	}
	// Listing runs outside the lock so one slow tree does not hold up
	// the palette for other projects.
	e = fileListEntry{at: time.Now(), files: gitx.ListFiles(ctx, p.Path)}
	s.files.mu.Lock()
	s.files.items[p.ID] = e
	s.files.mu.Unlock()
	return e.files
}

type fileListCache struct {
	mu    sync.Mutex
	items map[string]fileListEntry
}

type fileListEntry struct {
	at    time.Time
	files []string
}
