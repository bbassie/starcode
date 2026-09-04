package web

import (
	"context"
	"net/http"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/store"
	"starcode/internal/web/views"
)

// events is the read side: one long-lived SSE stream per open page. It
// renders the page from the store on connect, then patches fragments as
// events arrive on the bus.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	threadID := r.URL.Query().Get("id")
	theme := s.theme(r)
	ctx := r.Context()

	// Subscribe before the initial render so nothing slips between them.
	ch := s.App.Bus.Subscribe(ctx)
	sse := datastar.NewSSE(w, r)
	c := &conn{s: s, sse: sse, view: view, threadID: threadID, theme: theme, dirty: map[string]bool{}}

	if err := c.renderAll(ctx); err != nil {
		s.Log.Warn("initial render", "err", err)
		return
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	flush := time.NewTicker(60 * time.Millisecond)
	defer flush.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := sse.MarshalAndPatchSignals(map[string]any{"_hb": time.Now().Unix()}); err != nil {
				return
			}
		case <-flush.C:
			if err := c.flushDirty(ctx); err != nil {
				return
			}
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var err error
			switch m := msg.(type) {
			case bus.Resync:
				err = c.renderAll(ctx)
			case domain.Event:
				err = c.handle(ctx, m)
			case domain.GitChanged:
				c.gitDirty = true
			}
			if err != nil {
				s.Log.Debug("sse", "err", err)
				return
			}
		}
	}
}

type conn struct {
	s        *Server
	sse      *datastar.ServerSentEventGenerator
	view     string
	threadID string
	theme    string
	// dirty items get re-rendered on the next flush tick so a burst of
	// deltas costs one morph instead of one per token.
	dirty     map[string]bool
	gitDirty  bool
	sideDirty bool
	projectID string
	lastGit   time.Time
	// work is the id of the first item of the open "worked for" block, or
	// empty when the last transcript row is not a work block. Work items
	// are appended into it; anything else closes it.
	work string
}

func (c *conn) renderAll(ctx context.Context) error {
	if err := c.renderSidebar(ctx); err != nil {
		return err
	}
	switch c.view {
	case "thread":
		d, err := c.s.threadData(ctx, c.threadID)
		if err == store.ErrNotFound {
			return c.sse.Redirect("/")
		}
		if err != nil {
			return err
		}
		c.projectID = d.Project.ID
		c.work = ""
		if blocks := views.GroupItems(d.Items); len(blocks) > 0 && blocks[len(blocks)-1].Item == nil {
			c.work = blocks[len(blocks)-1].Work[0].ID
		}
		if err := c.sse.PatchElementTempl(views.Thread(d)); err != nil {
			return err
		}
		return c.renderGitPanel(ctx)
	default:
		ps, err := c.s.App.Store.Projects(ctx)
		if err != nil {
			return err
		}
		ts, err := c.s.App.Store.Threads(ctx)
		if err != nil {
			return err
		}
		caps, capsErrs := c.s.capabilities(ctx)
		home := views.HomeData{Projects: ps, Threads: ts, Settings: views.SettingsData{Agents: c.s.agentNames(), Caps: caps, CapsErrs: capsErrs, Agent: views.FirstOr(c.s.agentNames(), "claude")}}
		if err := c.sse.PatchElementTempl(views.Home(home)); err != nil {
			return err
		}
		return c.sse.PatchElementTempl(views.GitPanel(views.GitData{}))
	}
}

func (c *conn) renderSidebar(ctx context.Context) error {
	d, err := c.s.sidebarData(ctx, c.threadID, c.theme)
	if err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.Sidebar(d))
}

// renderHead refreshes the breadcrumb row and the composer, which both
// depend on the thread's status and settings.
func (c *conn) renderHead(ctx context.Context) error {
	d, err := c.s.threadData(ctx, c.threadID)
	if err != nil {
		return err
	}
	if err := c.sse.PatchElementTempl(views.MainHead(d)); err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.Composer(d))
}

func (c *conn) renderGit(ctx context.Context) error {
	c.gitDirty = false
	c.lastGit = time.Now()
	if c.projectID == "" {
		return nil
	}
	p, err := c.s.App.Store.Project(ctx, c.projectID)
	if err != nil {
		return nil
	}
	d := views.GitData{Project: p, Status: gitx.Read(ctx, p.Path), ThreadID: c.threadID}
	if err := c.sse.PatchElementTempl(views.GitHeader(d)); err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.GitFiles(d))
}

// renderGitPanel mounts the panel on the first thread-page render. Later
// refreshes use renderGit so the selected diff or editor is not replaced.
func (c *conn) renderGitPanel(ctx context.Context) error {
	c.gitDirty = false
	c.lastGit = time.Now()
	if c.projectID == "" {
		return nil
	}
	p, err := c.s.App.Store.Project(ctx, c.projectID)
	if err != nil {
		return nil
	}
	return c.sse.PatchElementTempl(views.GitPanel(views.GitData{Project: p, Status: gitx.Read(ctx, p.Path), ThreadID: c.threadID}))
}

func (c *conn) flushDirty(ctx context.Context) error {
	touchedWork := false
	for id := range c.dirty {
		delete(c.dirty, id)
		it, err := c.s.App.Store.Item(ctx, id)
		if err != nil {
			continue
		}
		if err := c.sse.PatchElementTempl(views.Item(it)); err != nil {
			return err
		}
		if views.IsWork(it.Kind) {
			touchedWork = true
		}
	}
	if touchedWork && c.work != "" {
		if err := c.renderWorkSummary(ctx, c.work); err != nil {
			return err
		}
	}
	if c.sideDirty {
		c.sideDirty = false
		if err := c.renderSidebar(ctx); err != nil {
			return err
		}
	}
	// Git status shells out, so rate-limit it.
	if c.gitDirty && time.Since(c.lastGit) > 750*time.Millisecond {
		return c.renderGit(ctx)
	}
	return nil
}

// renderWorkSummary re-renders the header of the work block starting at
// firstID ("Working…" / "Worked for 12s · 3 steps").
func (c *conn) renderWorkSummary(ctx context.Context, firstID string) error {
	items, err := c.s.App.Store.Items(ctx, c.threadID)
	if err != nil {
		return err
	}
	for _, b := range views.GroupItems(items) {
		if b.Item == nil && b.Work[0].ID == firstID {
			if running, _, _ := views.WorkState(b.Work); running {
				return c.sse.PatchElementTempl(views.WorkSummary(b.Work))
			}
			// Finished: morph the whole block so it also folds shut.
			return c.sse.PatchElementTempl(views.Work(b.Work))
		}
	}
	return nil
}

func (c *conn) handle(ctx context.Context, ev domain.Event) error {
	mine := c.view == "thread" && ev.ThreadID == c.threadID
	switch p := ev.Payload.(type) {
	case domain.ProjectAdded, domain.ProjectRemoved, domain.ThreadCreated:
		c.sideDirty = true
		if c.view == "home" {
			return c.renderAll(ctx)
		}
	case domain.ThreadRenamed, domain.ThreadStatusChanged, domain.ThreadSettingsChanged, domain.AgentSessionBound:
		c.sideDirty = true
		if mine {
			if _, ok := p.(domain.ThreadStatusChanged); ok && c.work != "" {
				// Idle again: the open work block gets its final header.
				if err := c.renderWorkSummary(ctx, c.work); err != nil {
					return err
				}
			}
			return c.renderHead(ctx)
		}
	case domain.ThreadDeleted:
		c.sideDirty = true
		if mine {
			return c.sse.Redirect("/")
		}
	case domain.ItemStarted:
		if !mine {
			return nil
		}
		// Keep order: earlier items must be current before a new one lands.
		if err := c.flushDirty(ctx); err != nil {
			return err
		}
		it, err := c.s.App.Store.Item(ctx, p.ID)
		if err != nil {
			return nil
		}
		delete(c.dirty, p.ID)
		if !views.IsWork(it.Kind) {
			// Closes any open work block; its header is refreshed with the
			// final duration.
			if c.work != "" {
				old := c.work
				c.work = ""
				if err := c.renderWorkSummary(ctx, old); err != nil {
					return err
				}
			}
			return c.sse.PatchElementTempl(views.Item(it), datastar.WithSelector("#items .items-inner"), datastar.WithModeAppend())
		}
		if c.work == "" {
			c.work = it.ID
			return c.sse.PatchElementTempl(views.Work([]store.Item{it}), datastar.WithSelector("#items .items-inner"), datastar.WithModeAppend())
		}
		if err := c.sse.PatchElementTempl(views.Item(it), datastar.WithSelector("#work-"+c.work+" .work-body"), datastar.WithModeAppend()); err != nil {
			return err
		}
		return c.renderWorkSummary(ctx, c.work)
	case domain.ItemDelta:
		if mine {
			c.dirty[p.ID] = true
		}
	case domain.ItemCompleted:
		if mine {
			c.dirty[p.ID] = true
		}
	case domain.ApprovalRequested:
		if mine {
			a, err := c.s.App.Store.Approval(ctx, c.threadID, p.ID)
			if err != nil {
				return nil
			}
			return c.sse.PatchElementTempl(views.Approval(a), datastar.WithSelector("#items .items-inner"), datastar.WithModeAppend())
		}
	case domain.ApprovalResolved:
		if mine {
			a, err := c.s.App.Store.Approval(ctx, c.threadID, p.ID)
			if err != nil {
				return nil
			}
			return c.sse.PatchElementTempl(views.ApprovalResolved(a))
		}
	case domain.TurnCompleted:
		c.gitDirty = true
	}
	return nil
}
