package web

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strconv"

	"github.com/a-h/templ"
	qrcode "github.com/skip2/go-qrcode"
	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/store"
	"starcode/internal/web/views"
)

// parts builds the three page regions for a view. The page handlers put
// them in the initial HTML, so a navigation paints the real page at once;
// the stream then renders the same parts on connect (a no-op morph) and
// patches fragments from there. Thread is set for the thread view so the
// stream can seat its bookkeeping.
func (s *Server) parts(ctx context.Context, p views.Page) (views.Parts, error) {
	var pt views.Parts
	side, err := s.sidebarData(ctx, p.ThreadID, p.View, p.Sidebar, p.Dense)
	if err != nil {
		return pt, err
	}
	pt.Sidebar = views.Sidebar(side)
	pt.Projects = store.Named(side.Projects)
	pt.Update = views.UpdateBanner(s.bannerData(ctx))
	switch p.View {
	case "thread":
		d, err := s.threadData(ctx, p.ThreadID)
		if err != nil {
			return pt, err
		}
		d.Full, d.NewSince = p.Full, p.NewSince
		pt.Thread = &d
		pt.Main = views.Thread(d)
	case "schedules":
		pt.Main = views.SchedulesPage(s.schedulesData(ctx, side))
	case "appearance":
		pt.Main = views.AppearancePage(views.SettingsPageData{Theme: p.Theme, Themes: Themes, Appearance: p.Appearance, Appearances: Appearances})
	case "settings":
		st := s.App.Store
		pt.Main = views.SettingsPage(views.SettingsPageData{Theme: p.Theme, Themes: Themes, PairURL: p.PairURL, PairQR: pairQR(p.PairURL), Sidebar: p.Sidebar, Dense: p.Dense, SettleMerged: st.SettleMerged(ctx), SettleDays: st.SettleIdleDays(ctx), SettleKeepPR: st.SettleKeepPR(ctx),
			Resume: st.ResumeAfterRestart(ctx), ResumeLimit: st.ResumeAfterLimit(ctx), AgentTools: st.AgentTools(ctx), WatchPRs: st.WatchLinkedPRs(ctx)})
	case "projects":
		pt.Main = views.ProjectsPage(s.projectsData(ctx, store.Named(side.Projects), p.ProjectSel))
	case "providers":
		pt.Main = views.ProvidersPage(s.providersData(ctx, p.ProviderSel, p.ProviderTab, false))
	case "keys":
		pt.Main = views.KeysPage(s.keysData())
	case "usage":
		d, err := s.usageData(ctx, p.UsageDays, p.UsageMetric)
		if err != nil {
			return pt, err
		}
		pt.Main = views.UsagePage(d)
	default:
		pt.Main = views.Home(s.homeData(ctx, side))
	}
	return pt, nil
}

// pairQR is the sign-in link as a QR code PNG data URL, empty for no link.
func pairQR(link string) string {
	if link == "" {
		return ""
	}
	png, err := qrcode.Encode(link, qrcode.Medium, 360)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// homeData is the new-thread page built from the sidebar's reads, which
// list the same projects.
func (s *Server) homeData(ctx context.Context, side views.SidebarData) views.HomeData {
	caps, capsErrs := s.capabilities(ctx)
	set := s.homeSettings(caps, capsErrs)
	// The composer starts on the first project; its defaults, when it
	// has them, are what the chips show (picking another project swaps
	// them, see views.projectPicked).
	named := store.Named(side.Projects)
	if len(named) > 0 {
		if p := named[0]; p.HasDefaults() {
			if _, ok := s.App.Agent(p.Agent); ok {
				set.Agent, set.Model, set.Effort, set.Mode = p.Agent, p.Model, p.Effort, p.Mode
			}
		}
	}
	return views.HomeData{Projects: named, Settings: set, Stashed: s.App.Store.StashCount(ctx)}
}

// earlierItems answers the "earlier" button of a cut transcript: the
// rows before seq, prepended as one element, and the button removed.
func (s *Server) earlierItems(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r = s.withThreadDir(r, id)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	items, err := s.App.Store.Items(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	aps, err := s.App.Store.Approvals(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n := 0
	for n < len(items) && items[n].Seq < before {
		n++
	}
	sse := datastar.NewSSE(w, r)
	if err := sse.PatchElementTempl(views.Earlier(items[:n], aps), datastar.WithSelector("#items .items-inner"), datastar.WithMode(datastar.ElementPatchModePrepend)); err != nil {
		return
	}
	sse.RemoveElementByID("load-earlier")
}

// page renders a full document for p, or the shell alone when the data
// cannot be read (the stream reports the error on connect).
func (s *Server) page(ctx context.Context, title string, p views.Page) templ.Component {
	pt, err := s.parts(ctx, p)
	if err != nil && err != store.ErrNotFound {
		s.Log.Warn("page render", "view", p.View, "err", err)
		pt = views.Parts{}
	}
	if pt.Thread == nil {
		return views.Layout(title, p, pt)
	}
	dir := pt.Thread.Project.Path
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return views.Layout(title, p, pt).Render(s.threadTitles(views.WithDir(ctx, dir)), w)
	})
}

// withThreadDir is r with the thread's folder in its context, so the
// views show the paths in its tool calls relative to it.
func (s *Server) withThreadDir(r *http.Request, threadID string) *http.Request {
	t, err := s.App.Store.Thread(r.Context(), threadID)
	if err != nil {
		return r
	}
	p, err := s.App.Store.Project(r.Context(), t.ProjectID)
	if err != nil {
		return r
	}
	return r.WithContext(s.threadTitles(views.WithDir(r.Context(), p.Path)))
}

// threadTitles is ctx with a lookup of thread titles for the views, so
// a tool call or a message that names a thread shows it by title. No
// cache: a stream's context lives as long as the page, and a title can
// change in that time.
func (s *Server) threadTitles(ctx context.Context) context.Context {
	return views.WithThreadTitles(ctx, func(id string) string {
		t, err := s.App.Store.Thread(context.WithoutCancel(ctx), id)
		if err != nil {
			return ""
		}
		return t.Title
	})
}
