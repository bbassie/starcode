package web

import (
	"context"
	"net/http"
	"time"

	"github.com/a-h/templ"
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
	usageDays, usageMetric := usageParams(r)
	provSel, provTab := providerParams(r)
	theme := s.theme(r)
	ctx := r.Context()

	// Subscribe before the initial render so nothing slips between them.
	ch := s.App.Bus.Subscribe(ctx)
	sse := datastar.NewSSE(w, r)
	// A page served by an older build of starcode has stale CSS and
	// scripts; only a reload can fix that.
	if v := r.URL.Query().Get("v"); v != "" && v != s.assets {
		sse.ExecuteScript("location.reload()")
		return
	}
	c := &conn{s: s, sse: sse, view: view, threadID: threadID, theme: theme, usageDays: usageDays, usageMetric: usageMetric, provSel: provSel, provTab: provTab, dirty: map[string]bool{}}

	if err := c.renderAll(ctx); err != nil {
		s.Log.Warn("initial render", "err", err)
		return
	}
	if err := c.renderUpdateBanner(ctx); err != nil {
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
			case domain.ProvidersChanged:
				c.sideDirty = true
				if view == "providers" {
					err = c.renderProviders(ctx)
				}
			case domain.BinaryUpdated:
				err = c.renderUpdateBanner(ctx)
			}
			if err != nil {
				s.Log.Debug("sse", "err", err)
				return
			}
		}
	}
}

type conn struct {
	s           *Server
	sse         *datastar.ServerSentEventGenerator
	view        string
	threadID    string
	theme       string
	usageDays   int
	usageMetric string
	provSel     string
	provTab     string
	// dirty items get re-rendered on the next flush tick so a burst of
	// deltas costs one morph instead of one per token.
	dirty     map[string]bool
	gitDirty  bool
	sideDirty bool
	homeDirty bool
	projectID string
	lastGit   time.Time
	// work is the id of the first item of the open "worked for" block, or
	// empty when the last transcript row is not a work block. Work items
	// are appended into it; anything else closes it.
	work string
}

// renderAll redraws the whole page from the store: on connect (a no-op
// morph over what the page handler already served) and after a bus resync.
func (c *conn) renderAll(ctx context.Context) error {
	pt, err := c.s.parts(ctx, c.page())
	if err == store.ErrNotFound {
		return c.sse.Redirect("/")
	}
	if err != nil {
		return err
	}
	c.sideDirty, c.homeDirty = false, false
	if pt.Thread != nil {
		c.projectID = pt.Thread.Project.ID
		c.work = ""
		if blocks := views.GroupItems(pt.Thread.Items); len(blocks) > 0 && blocks[len(blocks)-1].Item == nil {
			c.work = blocks[len(blocks)-1].Work[0].ID
		}
		c.gitDirty = false
		c.lastGit = time.Now()
	}
	for _, part := range []templ.Component{pt.Sidebar, pt.Main, pt.Git} {
		if err := c.sse.PatchElementTempl(part); err != nil {
			return err
		}
	}
	return nil
}

func (c *conn) page() views.Page {
	return views.Page{View: c.view, ThreadID: c.threadID, Theme: c.theme, UsageDays: c.usageDays, UsageMetric: c.usageMetric, ProviderSel: c.provSel, ProviderTab: c.provTab}
}

// renderHome redraws the projects overview. The composer in it keeps what
// the reader typed: the morph leaves the textarea alone and the prompt
// lives in a signal anyway.
func (c *conn) renderHome(ctx context.Context) error {
	c.homeDirty = false
	ps, err := c.s.App.Store.Projects(ctx)
	if err != nil {
		return err
	}
	ts, err := c.s.App.Store.Threads(ctx)
	if err != nil {
		return err
	}
	caps, capsErrs := c.s.capabilities(ctx)
	home := views.HomeData{Projects: ps, Threads: ts, Looks: c.s.agentLooks(), Settings: views.SettingsData{Agents: c.s.agentNames(), Looks: c.s.agentLooks(), Caps: caps, CapsErrs: capsErrs, Agent: views.FirstOr(c.s.agentNames(), "claude")}}
	return c.sse.PatchElementTempl(views.Home(home))
}

func (c *conn) renderProviders(ctx context.Context) error {
	return c.sse.PatchElementTempl(views.ProvidersPage(c.s.providersData(ctx, c.provSel, c.provTab, false)))
}

func (c *conn) renderUpdateBanner(ctx context.Context) error {
	show := c.s.Update.Changed()
	running := 0
	if show {
		running = c.s.runningThreads(ctx)
	}
	return c.sse.PatchElementTempl(views.UpdateBanner(show, running))
}

func (c *conn) renderSidebar(ctx context.Context) error {
	d, err := c.s.sidebarData(ctx, c.threadID, c.view == "settings" || c.view == "usage" || c.view == "providers" || c.view == "keys")
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
	if err := c.sse.PatchElementTempl(views.Composer(d)); err != nil {
		return err
	}
	if err := c.sse.PatchElementTempl(views.BusyBar(d)); err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.PageTitle(views.TabTitle(d.Thread)))
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
	if c.homeDirty {
		if err := c.renderHome(ctx); err != nil {
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

func (c *conn) renderPromptQueue(ctx context.Context) error {
	t, err := c.s.App.Store.Thread(ctx, c.threadID)
	if err != nil {
		return err
	}
	queued, err := c.s.App.Store.QueuedPrompts(ctx, c.threadID)
	if err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.PromptQueue(views.ThreadData{Thread: t, Queued: queued}))
}

func (c *conn) renderMessageRail(ctx context.Context) error {
	items, err := c.s.App.Store.Items(ctx, c.threadID)
	if err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.MessageRail(items))
}

func (c *conn) handle(ctx context.Context, ev domain.Event) error {
	mine := c.view == "thread" && ev.ThreadID == c.threadID
	switch p := ev.Payload.(type) {
	case domain.ProjectAdded, domain.ProjectRemoved, domain.ThreadCreated:
		c.sideDirty = true
		if c.view == "home" {
			return c.renderAll(ctx)
		}
	case domain.ThreadRenamed, domain.ThreadStatusChanged, domain.ThreadSettingsChanged, domain.AgentSessionBound, domain.ThreadArchived, domain.ThreadUnarchived:
		c.sideDirty = true
		// The project cards on the home page show the same glyphs and
		// titles as the sidebar; a burst of changes costs one redraw.
		c.homeDirty = c.view == "home"
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
		c.homeDirty = c.view == "home"
		if mine {
			return c.sse.Redirect("/")
		}
	case domain.PromptQueued, domain.PromptDequeued:
		if mine {
			return c.renderPromptQueue(ctx)
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
			if err := c.sse.PatchElementTempl(views.Item(it), datastar.WithSelector("#items .items-inner"), datastar.WithModeAppend()); err != nil {
				return err
			}
			if it.Kind == domain.KindUser {
				return c.renderMessageRail(ctx)
			}
			return nil
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
		if c.view == "usage" {
			return c.renderAll(ctx)
		}
		c.gitDirty = true
	}
	return nil
}
