package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
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
	projSel := r.URL.Query().Get("p")
	theme := s.theme(r)
	sidebar := s.sidebarMode(r)
	dense := s.denseMode(r)
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
	c := &conn{s: s, sse: sse, view: view, threadID: threadID, theme: theme, sidebar: sidebar, dense: dense, rowDirty: map[string]bool{}, projSel: projSel, usageDays: usageDays, usageMetric: usageMetric, provSel: provSel, provTab: provTab, dirty: map[string]bool{}, tails: map[string]*tail{}}
	// The page sends its signals with the request; the side panel's open
	// detail is the one worth keeping across a reconnect, and whether the
	// reader had loaded the whole transcript.
	var sig struct {
		GitPath  string `json:"gitPath"`
		GitEdit  bool   `json:"gitEdit"`
		PanelTab string `json:"panelTab"`
		Full     bool   `json:"full"`
		MainV    string `json:"mainv"`
		SideV    string `json:"sidev"`
	}
	datastar.ReadSignals(r, &sig)
	c.gitPath, c.gitEdit, c.panelTab, c.full = sig.GitPath, sig.GitEdit, sig.PanelTab, sig.Full
	c.mainV, c.sideV = sig.MainV, sig.SideV
	c.pairURL = s.pairURL(r)

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
				s.Log.Info("sse fell behind the bus, redrawing", "view", view, "thread", threadID)
				err = c.resync(ctx)
			case domain.Event:
				err = c.handle(ctx, m)
			case domain.GitChanged:
				// Only the thread page has a git panel, and only its own
				// project's tree matters; an empty id means any project.
				if c.view == "thread" && (m.ProjectID == "" || m.ProjectID == c.projectID) {
					c.gitDirty = true
				}
			case domain.ProvidersChanged:
				c.sideDirty, c.sideFull = true, true
				if view == "providers" {
					err = c.renderProviders(ctx)
				}
			case domain.BinaryUpdated:
				err = c.renderUpdateBanner(ctx)
			case domain.SeenChanged:
				c.sideDirty, c.sideFull = true, true
				c.homeDirty = view == "home"
			case domain.LimitsChanged:
				if view == "usage" {
					err = c.renderAll(ctx)
				} else if view == "thread" {
					c.ctxDirty = true
				}
			case domain.PRStateChanged:
				c.sideDirty, c.sideFull = true, true
				c.homeDirty = view == "home"
				if view == "thread" {
					err = c.renderHead(ctx, false)
				} else if view == "prs" {
					err = c.renderPRs(ctx)
				}
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
	sidebar  string
	dense    bool
	// rowDirty names the threads whose sidebar row changed since the last
	// flush; sideFull says something else did (projects, seen marks, PR
	// states). With only rows dirty and the order as it was, the rows are
	// patched one by one instead of the whole list; lastOrder is that order.
	rowDirty    map[string]bool
	sideFull    bool
	lastOrder   string
	usageDays   int
	usageMetric string
	provSel     string
	provTab     string
	projSel     string
	// dirty items get re-rendered on the next flush tick so a burst of
	// changes costs one morph instead of one per event.
	dirty map[string]bool
	// tails is the streamed text per running item since its last full
	// patch; see flushTails.
	tails     map[string]*tail
	gitDirty  bool
	sideDirty bool
	homeDirty bool
	ctxDirty  bool
	projectID string
	lastGit   time.Time
	// gitPath, gitEdit and panelTab are the side panel's open detail and
	// tab as the page reported them on connect.
	gitPath  string
	gitEdit  bool
	panelTab string
	// lastSide is the sidebar as last sent, to skip a redraw that would
	// send the same thing; lastGit the same for the panel's header and
	// file list.
	lastSide    string
	lastGitHTML string
	// mainV and sideV are hashes of the page and sidebar markup as the
	// page last got them whole, sent back as signals when the stream
	// reconnects: a phone coming back from the background with nothing
	// changed skips the redraw, which was the whole page every time.
	mainV string
	sideV string
	// pairURL is the settings page's sign-in link, from the request that
	// opened the stream, so the redraw on connect keeps the QR code.
	pairURL string
	// full is whether the page has the whole transcript (the reader
	// pressed "earlier"); a redraw then keeps it instead of cutting again.
	full bool
	// hasItems is whether the transcript had an item at the last full
	// render; the first one locks the agent chip, so the composer is
	// redrawn once when it lands.
	hasItems bool
	// work is the id of the first item of the open "worked for" block, or
	// empty when the last transcript row is not a work block. Work items
	// are appended into it; anything else closes it.
	work string
}

// renderAll redraws the whole page from the store: on connect, which is a
// no-op morph over what the page handler already served, or puts back the
// detail the page had open when the stream reconnects.
func (c *conn) renderAll(ctx context.Context) error {
	return c.render(ctx, true)
}

// resync redraws after the bus dropped messages for this page. The side
// panel's detail (an open diff or file) is left alone: the connection has
// no fresh copy of what the reader opened since, and a dropped GitChanged
// only affects the header and the file lists, which renderGit redraws.
func (c *conn) resync(ctx context.Context) error {
	return c.render(ctx, false)
}

func (c *conn) render(ctx context.Context, withDetail bool) error {
	pt, err := c.s.parts(ctx, c.page())
	if err == store.ErrNotFound {
		return c.sse.Redirect("/")
	}
	if err != nil {
		return err
	}
	c.sideDirty, c.homeDirty = false, false
	if pt.Thread != nil {
		c.s.App.MarkSeen(ctx, c.threadID)
		c.projectID = pt.Thread.Project.ID
		c.hasItems = len(pt.Thread.Items) > 0
		c.tails = map[string]*tail{}
		c.work = ""
		if blocks := views.GroupItems(pt.Thread.Items); len(blocks) > 0 && blocks[len(blocks)-1].Item == nil {
			c.work = blocks[len(blocks)-1].Work[0].ID
		}
		c.gitDirty = false
		c.lastGit = time.Now()
	}
	side, err := renderString(ctx, pt.Sidebar)
	if err != nil {
		return err
	}
	main, err := renderString(ctx, pt.Main)
	if err != nil {
		return err
	}
	c.lastSide = side
	if sd, err := c.s.sidebarData(ctx, c.threadID, c.view, c.sidebar, c.dense); err == nil {
		c.lastOrder = views.SidebarOrder(sd)
	}
	sideV, mainV := hashHTML(side), hashHTML(main)
	if sideV != c.sideV {
		if err := c.sse.PatchElements(side); err != nil {
			return err
		}
	}
	if mainV != c.mainV {
		if err := c.sse.PatchElements(main); err != nil {
			return err
		}
	}
	if sideV != c.sideV || mainV != c.mainV {
		c.sideV, c.mainV = sideV, mainV
		if err := c.sse.MarshalAndPatchSignals(map[string]any{"sidev": sideV, "mainv": mainV}); err != nil {
			return err
		}
	}
	if withDetail {
		return c.sse.PatchElementTempl(pt.Git)
	}
	return c.renderGit(ctx)
}

func renderString(ctx context.Context, c templ.Component) (string, error) {
	var buf strings.Builder
	err := c.Render(ctx, &buf)
	return buf.String(), err
}

func hashHTML(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func (c *conn) page() views.Page {
	return views.Page{View: c.view, ThreadID: c.threadID, Theme: c.theme, UsageDays: c.usageDays, UsageMetric: c.usageMetric, ProviderSel: c.provSel, ProviderTab: c.provTab, ProjectSel: c.projSel, GitPath: c.gitPath, GitEdit: c.gitEdit, PanelTab: c.panelTab, Full: c.full, PairURL: c.pairURL, Sidebar: c.sidebar, Dense: c.dense}
}

// renderHome redraws the projects overview. The composer in it keeps what
// the reader typed: the morph leaves the textarea alone and the prompt
// lives in a signal anyway.
func (c *conn) renderHome(ctx context.Context) error {
	c.homeDirty = false
	side, err := c.s.sidebarData(ctx, c.threadID, c.view, c.sidebar, c.dense)
	if err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.Home(c.s.homeData(ctx, side)))
}

func (c *conn) renderPRs(ctx context.Context) error {
	d, err := c.s.prsPageData(ctx, false)
	if err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.PRsList(d))
}

func (c *conn) renderProviders(ctx context.Context) error {
	return c.sse.PatchElementTempl(views.ProvidersPage(c.s.providersData(ctx, c.provSel, c.provTab, false)))
}

func (c *conn) renderUpdateBanner(ctx context.Context) error {
	return c.sse.PatchElementTempl(views.UpdateBanner(c.s.bannerData(ctx, c.threadID)))
}

func (c *conn) renderSidebar(ctx context.Context) error {
	d, err := c.s.sidebarData(ctx, c.threadID, c.view, c.sidebar, c.dense)
	if err != nil {
		return err
	}
	// A thread's own change with every row where it was needs that row
	// only: a status flip then costs one row, not the list.
	rows, full := c.rowDirty, c.sideFull
	c.rowDirty, c.sideFull = map[string]bool{}, false
	order := views.SidebarOrder(d)
	same := order == c.lastOrder
	c.lastOrder = order
	if same && !full && len(rows) > 0 && len(rows) <= 4 && !c.homeDirty {
		for id := range rows {
			if row := views.SidebarRow(d, id); row != nil {
				if err := c.sse.PatchElementTempl(row); err != nil {
					return err
				}
			}
		}
		c.lastSide = "" // the list as last sent whole is stale now
		return nil
	}
	// Many events mark the sidebar dirty without changing what it shows
	// (a seen stamp, a status this page already drew); the redraw is a
	// good part of a turn's bytes on a phone, so an identical one is skipped.
	var buf strings.Builder
	if err := views.Sidebar(d).Render(ctx, &buf); err != nil {
		return err
	}
	if buf.String() != c.lastSide {
		c.lastSide = buf.String()
		if err := c.sse.PatchElements(buf.String()); err != nil {
			return err
		}
	}
	// The home page draws the same threads; one read serves both.
	if c.homeDirty {
		c.homeDirty = false
		return c.sse.PatchElementTempl(views.Home(c.s.homeData(ctx, d)))
	}
	return nil
}

// renderHead refreshes the breadcrumb row, the busy bar, the tab title
// and what the composer shows for the thread's status: its rules, its
// buttons and the locked and placeholder signals. The composer itself,
// with the model lists of every agent in it, is only redrawn when
// composer is set: a settings change, or the first item, which locks
// the agent chip.
func (c *conn) renderHead(ctx context.Context, composer bool) error {
	d, err := c.s.threadData(ctx, c.threadID)
	if err != nil {
		return err
	}
	c.hasItems = len(d.Items) > 0
	if err := c.sse.PatchElementTempl(views.MainHead(d)); err != nil {
		return err
	}
	if composer {
		if err := c.sse.PatchElementTempl(views.Composer(d)); err != nil {
			return err
		}
	} else {
		if err := c.sse.PatchElementTempl(views.SessionRules(d)); err != nil {
			return err
		}
		if err := c.sse.PatchElementTempl(views.ComposerActions(d)); err != nil {
			return err
		}
		if err := c.sse.MarshalAndPatchSignals(views.ComposerSignals(d)); err != nil {
			return err
		}
	}
	if err := c.sse.PatchElementTempl(views.BusyBar(d)); err != nil {
		return err
	}
	return c.sse.PatchElementTempl(views.PageTitle(views.TabTitle(d.Thread)))
}

// renderContextMeter redraws the window gauge in the composer. It moves
// several times a turn, so it is patched on its own rather than with the
// whole composer.
func (c *conn) renderContextMeter(ctx context.Context) error {
	d, err := c.s.contextData(ctx, c.threadID)
	if err != nil {
		// A stale meter is not worth dropping the stream over.
		c.s.Log.Debug("context meter", "thread", c.threadID, "err", err)
		return nil
	}
	return c.sse.PatchElementTempl(views.ContextMeter(d))
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
	if t, err := c.s.App.Store.Thread(ctx, c.threadID); err == nil {
		p.Path = t.Dir(p)
	}
	d := views.GitData{Project: p, Status: gitx.Read(ctx, p.Path), ThreadID: c.threadID}
	head, err := renderString(ctx, views.GitHeader(d))
	if err != nil {
		return err
	}
	files, err := renderString(ctx, views.GitFiles(d))
	if err != nil {
		return err
	}
	// Every tool call marks the tree dirty; most leave it as it was.
	if head+files == c.lastGitHTML {
		return nil
	}
	c.lastGitHTML = head + files
	if err := c.sse.PatchElements(head); err != nil {
		return err
	}
	return c.sse.PatchElements(files)
}

// tail is what one running item has streamed since the page last got a
// full patch of it. A flush appends the new text into the element (one
// span, see views.Tail) instead of re-sending the whole item, which for
// a long reply cost its full size on every tick; a full patch every
// tailFullEvery puts the markdown and the summary line right, and the
// completion sends the last one. Tool output only appends: its element
// is plain text, and past the clip in views.Tool nothing more is sent
// until the item completes.
type tail struct {
	kind    string
	field   string
	pending strings.Builder
	// sent is how much of the field the page has, appended or rendered;
	// zero means the element has no child to append into yet.
	sent     int
	lastFull time.Time
	capped   bool
	// trail is the whitespace at the end of the last full patch, which
	// the markdown drops from the end of a paragraph; the next append
	// puts it back so the words do not run together.
	trail string
}

const (
	tailFullEvery = 500 * time.Millisecond
	tailCap       = 20000 // the clip in views.Tool
)

// tailSelector is where a kind's streamed text goes: the last element
// of the markdown, the thinking text, the tool's output block.
func tailSelector(kind, id string) string {
	switch kind {
	case domain.KindAssistant:
		return "#it-" + id + " > :last-child"
	case domain.KindThinking:
		return "#it-" + id + " > pre"
	case domain.KindTool:
		return "#it-" + id + " > .block.output"
	}
	return ""
}

func (c *conn) flushTails(ctx context.Context) error {
	now := time.Now()
	for id, t := range c.tails {
		if t.pending.Len() == 0 {
			continue
		}
		text := t.pending.String()
		t.pending.Reset()
		if t.capped {
			continue
		}
		sel := tailSelector(t.kind, id)
		full := sel == "" || t.sent == 0 || (t.field != "output" && now.Sub(t.lastFull) > tailFullEvery)
		if !full {
			if err := c.sse.PatchElementTempl(views.Tail(t.trail+text), datastar.WithSelector(sel), datastar.WithModeAppend()); err != nil {
				return err
			}
			t.trail = ""
			t.sent += len(text)
			t.capped = t.field == "output" && t.sent > tailCap
			continue
		}
		it, err := c.s.App.Store.Item(ctx, id)
		if err != nil {
			continue
		}
		if err := c.patchItem(ctx, it); err != nil {
			return err
		}
		field := t.field
		*t = *newTail(it)
		t.field = field
		if field == "output" {
			t.sent = len(it.Output)
		}
		t.capped = field == "output" && t.sent > tailCap
	}
	return nil
}

// newTail is the bookkeeping for an item the page just got whole.
func newTail(it store.Item) *tail {
	t := &tail{kind: it.Kind, sent: len(it.Body), lastFull: time.Now()}
	if it.Kind == domain.KindAssistant {
		t.trail = it.Body[len(strings.TrimRight(it.Body, " \t\n")):]
	}
	return t
}

// patchItem redraws one item whole. A Task carries its subagent's items
// inside it, so those are read along, or the morph would drop them.
func (c *conn) patchItem(ctx context.Context, it store.Item) error {
	if !views.IsAgentTool(it) {
		return c.sse.PatchElementTempl(views.Item(it))
	}
	kids, err := c.s.App.Store.ItemsByParent(ctx, c.threadID, it.ID)
	if err != nil {
		return err
	}
	_, nested := views.Nest(kids)
	nested[it.ID] = kids
	return c.sse.PatchElementTempl(views.ItemWith(it, nested))
}

func (c *conn) flushDirty(ctx context.Context) error {
	if err := c.flushTails(ctx); err != nil {
		return err
	}
	touchedWork := false
	parents := map[string]bool{}
	for id := range c.dirty {
		delete(c.dirty, id)
		it, err := c.s.App.Store.Item(ctx, id)
		if err != nil {
			continue
		}
		if err := c.patchItem(ctx, it); err != nil {
			return err
		}
		if views.IsWork(it.Kind) {
			touchedWork = true
		}
		// A subagent's step landing changes its Task's summary line.
		if p := views.ParentOf(it); p != "" {
			parents[p] = true
		}
	}
	for p := range parents {
		if it, err := c.s.App.Store.Item(ctx, p); err == nil {
			if err := c.patchItem(ctx, it); err != nil {
				return err
			}
		}
	}
	if touchedWork && c.work != "" {
		if err := c.renderWorkSummary(ctx, c.work); err != nil {
			return err
		}
	}
	if c.ctxDirty {
		c.ctxDirty = false
		if err := c.renderContextMeter(ctx); err != nil {
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
	// The block starts at firstID, so nothing before it is needed and the
	// first group is the one to draw.
	items, err := c.s.App.Store.ItemsFrom(ctx, c.threadID, firstID)
	if err != nil {
		return err
	}
	aps, err := c.s.App.Store.Approvals(ctx, c.threadID)
	if err != nil {
		return err
	}
	// The block's Tasks carry their subagents' items; the whole-block
	// morph needs them or it would drop them.
	_, kids := views.Nest(items)
	for _, b := range views.GroupItems(items) {
		if b.Item == nil && b.Work[0].ID == firstID {
			if running, _, _ := views.WorkState(b.Work, aps); running {
				return c.sse.PatchElementTempl(views.WorkSummary(b.Work, aps))
			}
			// Finished: morph the whole block so it also folds shut.
			return c.sse.PatchElementTempl(views.Work(b.Work, aps, kids))
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
	if _, delta := ev.Payload.(domain.ItemDelta); mine && !delta {
		// Whatever just happened on this thread, someone is looking at it.
		// A delta is skipped: there is one per token, and the thread is
		// running while they stream, so it cannot have gone unread.
		c.s.App.MarkSeen(ctx, c.threadID)
	}
	switch p := ev.Payload.(type) {
	case domain.ProjectAdded, domain.ProjectRemoved, domain.ProjectSettingsChanged, domain.ThreadCreated:
		c.sideDirty, c.sideFull = true, true
		if c.view == "home" {
			return c.renderAll(ctx)
		}
	case domain.ThreadRewound:
		c.sideDirty = true
		c.rowDirty[ev.ThreadID] = true
		c.homeDirty = c.view == "home"
		if mine {
			// Rows left the transcript: the page is drawn again, which is
			// rare enough to cost a full morph.
			return c.resync(ctx)
		}
	case domain.ThreadRenamed, domain.ThreadStatusChanged, domain.ThreadSettingsChanged, domain.AgentSessionBound, domain.ThreadArchived, domain.ThreadUnarchived, domain.ThreadSnoozed, domain.ThreadWoken, domain.ThreadPinned, domain.ThreadUnpinned, domain.ThreadWorktreeSet, domain.ThreadPRLinked, domain.ThreadPRUnlinked:
		c.sideDirty = true
		c.rowDirty[ev.ThreadID] = true
		// The project cards on the home page show the same glyphs and
		// titles as the sidebar; a burst of changes costs one redraw.
		c.homeDirty = c.view == "home"
		if c.view == "prs" {
			// The page lists the threads linked to each PR.
			switch p.(type) {
			case domain.ThreadPRLinked, domain.ThreadPRUnlinked, domain.ThreadRenamed:
				return c.renderPRs(ctx)
			}
		}
		if mine {
			if _, ok := p.(domain.ThreadStatusChanged); ok && c.work != "" {
				// Idle again: the open work block gets its final header.
				if err := c.renderWorkSummary(ctx, c.work); err != nil {
					return err
				}
			}
			// Settings and the bound session change what the chips show;
			// the rest only moves the status. A worktree change moves the
			// panel to the other checkout as well.
			switch p.(type) {
			case domain.ThreadSettingsChanged, domain.AgentSessionBound:
				return c.renderHead(ctx, true)
			case domain.ThreadWorktreeSet:
				c.gitDirty = true
			}
			return c.renderHead(ctx, false)
		}
	case domain.ThreadDeleted:
		c.sideDirty, c.sideFull = true, true
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
		// The page gets the item whole here; its deltas append from now on.
		c.tails[p.ID] = newTail(it)
		if parent := views.ParentOf(it); parent != "" {
			// A subagent's item goes under its Task, whatever block that
			// Task is in; the work block bookkeeping is the Task's, whose
			// summary line is redrawn on the next flush for the new step.
			c.dirty[parent] = true
			return c.sse.PatchElementTempl(views.Item(it), datastar.WithSelector("#it-"+parent+" .agent-body"), datastar.WithModeAppend())
		}
		if !c.hasItems {
			// The first item locks the agent chip.
			c.hasItems = true
			if err := c.renderHead(ctx, true); err != nil {
				return err
			}
		}
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
			return c.sse.PatchElementTempl(views.Work([]store.Item{it}, nil, nil), datastar.WithSelector("#items .items-inner"), datastar.WithModeAppend())
		}
		if err := c.sse.PatchElementTempl(views.Item(it), datastar.WithSelector("#work-"+c.work+" .work-body"), datastar.WithModeAppend()); err != nil {
			return err
		}
		return c.renderWorkSummary(ctx, c.work)
	case domain.ItemDelta:
		if mine {
			t := c.tails[p.ID]
			if t == nil {
				// Streaming began before this page connected; the first
				// flush sends the item whole and learns its kind.
				t = &tail{}
				c.tails[p.ID] = t
			}
			if p.Field == "output" && t.field != "output" {
				t.field, t.sent = "output", 0
			}
			t.pending.WriteString(p.Text)
		}
	case domain.ItemCompleted:
		if mine {
			delete(c.tails, p.ID)
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
			if err := c.sse.PatchElementTempl(views.ApprovalResolved(a)); err != nil {
				return err
			}
			if p.Decision == domain.DecisionAllowSession && !p.Auto {
				// The composer lists the session rules; a new one just landed.
				return c.renderHead(ctx, false)
			}
		}
	case domain.RuleRevoked:
		if mine {
			return c.renderHead(ctx, false)
		}
	case domain.ContextUsed:
		if mine {
			c.ctxDirty = true
		}
	case domain.TurnCompleted:
		// The git panel is redrawn by the GitChanged that pump publishes
		// right after this event, gated to the thread's project.
		if c.view == "usage" {
			return c.renderAll(ctx)
		}
	}
	return nil
}
