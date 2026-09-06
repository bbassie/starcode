package web

import (
	"context"

	"github.com/a-h/templ"

	"starcode/internal/gitx"
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
	side, err := s.sidebarData(ctx, p.ThreadID, p.View == "settings" || p.View == "usage" || p.View == "providers" || p.View == "keys")
	if err != nil {
		return pt, err
	}
	pt.Sidebar = views.Sidebar(side)
	pt.Projects = side.Projects
	pt.Git = views.GitPanel(views.GitData{})
	pt.Update = views.UpdateBanner(s.Update.Changed(), s.runningThreads(ctx))
	switch p.View {
	case "thread":
		d, err := s.threadData(ctx, p.ThreadID)
		if err != nil {
			return pt, err
		}
		pt.Thread = &d
		pt.Main = views.Thread(d)
		if d.Project.ID != "" {
			pt.Git = views.GitPanel(views.GitData{Project: d.Project, Status: gitx.Read(ctx, d.Project.Path), ThreadID: d.Thread.ID})
		}
	case "settings":
		pt.Main = views.SettingsPage(views.SettingsPageData{Theme: p.Theme, Themes: Themes})
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
		ps, err := s.App.Store.Projects(ctx)
		if err != nil {
			return pt, err
		}
		ts, err := s.App.Store.Threads(ctx)
		if err != nil {
			return pt, err
		}
		caps, capsErrs := s.capabilities(ctx)
		pt.Main = views.Home(views.HomeData{Projects: ps, Threads: ts, Looks: s.agentLooks(), Seen: side.Seen, Settings: views.SettingsData{Agents: s.agentNames(), Looks: s.agentLooks(), Caps: caps, CapsErrs: capsErrs, Agent: views.FirstOr(s.agentNames(), "claude")}})
	}
	return pt, nil
}

// page renders a full document for p, or the shell alone when the data
// cannot be read (the stream reports the error on connect).
func (s *Server) page(ctx context.Context, title string, p views.Page) templ.Component {
	pt, err := s.parts(ctx, p)
	if err != nil && err != store.ErrNotFound {
		s.Log.Warn("page render", "view", p.View, "err", err)
		pt = views.Parts{}
	}
	return views.Layout(title, p, pt)
}
