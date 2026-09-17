package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/a-h/templ"
	qrcode "github.com/skip2/go-qrcode"
	"github.com/starfederation/datastar-go/datastar"

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
	side, err := s.sidebarData(ctx, p.ThreadID, p.View, p.Sidebar)
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
		d.Full = p.Full
		pt.Thread = &d
		pt.Main = views.Thread(d)
		if d.Project.ID != "" {
			// The panel shows the thread's checkout, which may be a worktree.
			pp := d.Project
			pp.Path = d.Thread.Dir(pp)
			g := views.GitData{Project: pp, Status: gitx.Read(ctx, pp.Path), ThreadID: d.Thread.ID}
			// A stream that reconnects says which detail the page had
			// open; the editor's text is still in the page's signals.
			if p.GitPath != "" && insideProject(p.GitPath) {
				g.Selected, g.Editing = p.GitPath, p.GitEdit
				g.Diff = gitx.Diff(ctx, pp.Path, p.GitPath)
			}
			if p.PanelTab == "files" {
				g.TreeOpen = true
				if _, entries, err := listProjectDir(pp.Path, ""); err == nil {
					for _, e := range entries {
						g.Tree = append(g.Tree, views.ProjectFile{Name: e.Name, Path: e.Path, IsDir: e.IsDir})
					}
				}
			}
			pt.Git = views.GitPanel(g)
		}
	case "appearance":
		pt.Main = views.AppearancePage(views.SettingsPageData{Theme: p.Theme, Themes: Themes})
	case "settings":
		pt.Main = views.SettingsPage(views.SettingsPageData{Theme: p.Theme, Themes: Themes, PairURL: p.PairURL, PairQR: pairQR(p.PairURL), Sidebar: p.Sidebar, SettleMerged: s.App.Store.SettleMerged(ctx), SettleDays: s.App.Store.SettleIdleDays(ctx)})
	case "providers":
		pt.Main = views.ProvidersPage(s.providersData(ctx, p.ProviderSel, p.ProviderTab, false))
	case "keys":
		pt.Main = views.KeysPage(s.keysData())
	case "prs":
		d, err := s.prsPageData(ctx, false)
		if err != nil {
			return pt, err
		}
		pt.Main = views.PRsPage(d)
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

// homeData is the projects overview built from the sidebar's reads,
// which list the same projects and threads.
func (s *Server) homeData(ctx context.Context, side views.SidebarData) views.HomeData {
	caps, capsErrs := s.capabilities(ctx)
	return views.HomeData{Projects: side.Projects, Threads: side.Threads, Looks: s.agentLooks(), Seen: side.Seen, PRs: side.PRs, Settings: s.homeSettings(caps, capsErrs)}
}

// earlierItems answers the "earlier" button of a cut transcript: the
// rows before seq, prepended as one element, and the button removed.
func (s *Server) earlierItems(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	return views.Layout(title, p, pt)
}
