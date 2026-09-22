package web

// The composer's slash menu. GET /api/threads/{id}/commands renders the
// commands a thread's agent takes as typed text, so the page can offer
// them when the reader types "/" (static/slash.js opens the menu, fetches
// this and filters the rows as the reader goes on typing). Built-ins are
// a fixed list per driver; skills and custom commands are read from the
// instance's config dir and the project's .claude directory. The list is
// cached a minute per thread and config dir, since a scan touches a few
// dozen files and the menu opens on every "/".

import (
	"bufio"
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"starcode/internal/store"
	"starcode/internal/web/views"
)

// claudeCommands are the built-in commands Claude Code takes as a user
// message over stream-json, one line each.
var claudeCommands = []views.SlashItem{
	// Checked over stream-json: these answer. /status and /release-notes
	// say "isn't available in this environment", /model would change the
	// model behind the composer's chip, and /clear empties the CLI's
	// conversation while the transcript here stays, so they are left out.
	{Cmd: "/compact", Desc: "Fold the conversation so far into a summary and go on from that"},
	{Cmd: "/cost", Desc: "Show the plan's usage so far, the way /usage does"},
	{Cmd: "/context", Desc: "Show what fills the context window"},
	{Cmd: "/review", Desc: "Review the working tree's changes"},
	{Cmd: "/init", Desc: "Write a CLAUDE.md for this project"},
	{Cmd: "/pr-comments", Desc: "Read the comments on the branch's pull request"},
}

// Codex has no built-ins here: its app-server takes turn/start text as
// a prompt and leaves slash commands to the TUI, so "/review" typed
// into a Codex thread would reach the model as three words. Compacting
// goes through the context ring, which calls thread/compact/start.

// slashRoutes registers the menu's endpoint. server.go calls it.
func (s *Server) slashRoutes() {
	s.mux.HandleFunc("GET /api/threads/{id}/commands", s.slashCommands)
	s.mux.HandleFunc("GET /api/threads/{id}/prpick", s.prPick)
}

// slashCommands answers with the menu's rows as plain HTML; the page
// puts them into its .slash-menu box itself.
func (s *Server) slashCommands(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.App.Store.Thread(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.App.Store.Project(ctx, t.ProjectID)
	if err != nil && err != store.ErrNotFound {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	driver, configDir := t.Agent, ""
	if s.Providers != nil {
		if in, ok := s.Providers.Get(t.Agent); ok {
			driver, configDir = in.Driver, in.ConfigDir
		}
	}
	configDir = driverConfigDir(driver, configDir)
	items := slashLists.get(t.ID+"\x00"+configDir, func() []views.SlashItem {
		return slashItems(driver, configDir, t.Dir(p))
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	views.SlashMenu(items).Render(ctx, w)
}

// driverConfigDir is the directory the driver reads its settings from:
// the instance's own, else the variable in starcode's environment, else
// the CLI's default under the home directory (the same rule the usage
// scanner applies).
func driverConfigDir(driver, configDir string) string {
	if configDir != "" {
		return configDir
	}
	home, _ := os.UserHomeDir()
	switch driver {
	case "claude":
		if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
			return d
		}
		return filepath.Join(home, ".claude")
	case "codex":
		if d := strings.TrimSpace(os.Getenv("CODEX_HOME")); d != "" {
			return d
		}
		return filepath.Join(home, ".codex")
	}
	return ""
}

// slashItems is the menu for a driver: its built-ins, then the skills
// and custom commands under the config dir and the project, by name.
func slashItems(driver, configDir, project string) []views.SlashItem {
	if driver != "claude" {
		return nil
	}
	items := append([]views.SlashItem(nil), claudeCommands...)
	roots := []string{configDir}
	if project != "" {
		roots = append(roots, filepath.Join(project, ".claude"))
	}
	return append(items, slashScan(roots...)...)
}

// slashScan reads <root>/skills/<name>/SKILL.md and <root>/commands/<name>.md
// under each root. A name found twice keeps the first root's entry, so
// the config dir wins over the project, as in Claude Code itself.
func slashScan(roots ...string) []views.SlashItem {
	var out []views.SlashItem
	seen := map[string]bool{}
	add := func(name, path string, skill bool) {
		if name == "" || seen[name] {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		seen[name] = true
		out = append(out, views.SlashItem{Cmd: "/" + name, Desc: skillDesc(data), Skill: skill})
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		dirs, _ := os.ReadDir(filepath.Join(root, "skills"))
		for _, d := range dirs {
			if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
				add(d.Name(), filepath.Join(root, "skills", d.Name(), "SKILL.md"), true)
			}
		}
		files, _ := os.ReadDir(filepath.Join(root, "commands"))
		for _, f := range files {
			name, ok := strings.CutSuffix(f.Name(), ".md")
			if ok && !f.IsDir() && !strings.HasPrefix(name, ".") {
				add(name, filepath.Join(root, "commands", f.Name()), false)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Cmd < out[j].Cmd })
	return out
}

// skillDesc is the one-line description of a SKILL.md or command file:
// the front matter's description when it has one, else the first
// non-empty line of the body without its heading marks.
func skillDesc(data []byte) string {
	body := data
	if rest, ok := bytes.CutPrefix(data, []byte("---\n")); ok {
		if end := bytes.Index(rest, []byte("\n---")); end >= 0 {
			front, after := rest[:end], rest[end+4:]
			body = after
			lines := strings.Split(string(front), "\n")
			for i, line := range lines {
				v, ok := strings.CutPrefix(line, "description:")
				if !ok {
					continue
				}
				// A folded or literal scalar (">" or "|") has its text on
				// the indented lines that follow.
				if d := cleanDesc(v); d != "" {
					return d
				}
				var parts []string
				for _, l := range lines[i+1:] {
					if !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
						break
					}
					parts = append(parts, strings.TrimSpace(l))
				}
				if d := cleanDesc(strings.Join(parts, " ")); d != "" {
					return d
				}
			}
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		if d := cleanDesc(strings.TrimLeft(sc.Text(), "# ")); d != "" {
			return d
		}
	}
	return ""
}

// cleanDesc trims a front matter value: quotes, a folded-scalar mark and
// anything past the first sentence-length stretch.
func cleanDesc(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimLeft(v, ">|")
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	if r := []rune(v); len(r) > 160 {
		v = string(r[:159]) + "…"
	}
	return v
}

// slashCache holds scanned menus for a minute, keyed by thread and
// config dir.
type slashCache struct {
	mu sync.Mutex
	m  map[string]slashEntry
}

type slashEntry struct {
	items []views.SlashItem
	at    time.Time
}

const slashTTL = time.Minute

var slashLists slashCache

func (c *slashCache) get(key string, build func() []views.SlashItem) []views.SlashItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && time.Since(e.at) < slashTTL {
		return e.items
	}
	if c.m == nil {
		c.m = map[string]slashEntry{}
	}
	items := build()
	c.m[key] = slashEntry{items: items, at: time.Now()}
	return items
}

// prPick answers the composer's "#" menu: the open pull requests of the
// thread's repository (the panel's list, cached a minute), as rows the
// page filters by number and title while the reader types. A pick puts a
// link to the PR in the prompt (static/slash.js).
func (s *Server) prPick(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.App.Store.Thread(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.App.Store.Project(ctx, t.ProjectID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p.Path = t.Dir(p)
	e := s.prList(r, p)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	views.PRPickMenu(e.list).Render(ctx, w)
}
