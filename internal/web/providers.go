package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"starcode/internal/agent"
	"starcode/internal/domain"
	"starcode/internal/providers"
	"starcode/internal/web/views"
)

// Provider instances live in s.Providers (the on-disk list) and, when
// enabled, in the app's agent registry. This file keeps them in step, runs
// the checks behind the providers page (version, sign-in, latest release
// on npm) and runs the CLIs' own update commands on request. Checks are
// cached; a background loop repeats them on the configured interval and
// publishes ProvidersChanged when the number of available updates moves.

const (
	providerTTL  = 10 * time.Minute
	latestTTL    = time.Hour
	npmLatestURL = "https://registry.npmjs.org/%s/latest"
	updateLogMax = 4 << 10
)

type providerEntry struct {
	info agent.ProviderInfo
	err  error
	at   time.Time
}

// updateRun is one `claude update` / `codex update` and what it printed.
type updateRun struct {
	running  bool
	started  time.Time
	finished time.Time
	log      string
	err      string
}

// loginRun is one sign-in driven from the page (agent.Login) and, once
// it ends, how it went. It stays until the next sign-in or a page refresh
// with force, so the outcome is visible.
type loginRun struct {
	login    agent.Login
	started  time.Time
	finished time.Time
	err      string
}

type providerCache struct {
	mu        sync.Mutex
	infos     map[string]providerEntry
	latest    map[string]string // npm package -> latest version
	latestAt  time.Time
	updates   int // last computed count of CLIs with a newer release
	signedOut int // last computed count of enabled CLIs not signed in
	runs      map[string]*updateRun
	logins    map[string]*loginRun
}

func (c *providerCache) init() {
	if c.infos == nil {
		c.infos = map[string]providerEntry{}
		c.latest = map[string]string{}
		c.runs = map[string]*updateRun{}
		c.logins = map[string]*loginRun{}
	}
}

// providerRows gathers one row per configured instance, enabled or not.
// force ignores both caches.
func (s *Server) providerRows(ctx context.Context, force bool) []views.ProviderRow {
	s.providers.mu.Lock()
	s.providers.init()
	s.providers.mu.Unlock()

	var rows []views.ProviderRow
	for _, in := range s.Providers.Instances() {
		row := views.ProviderRow{Inst: in, Binary: s.Providers.Binary(in)}
		ag, err := s.Providers.Build(in)
		if err != nil {
			row.Err = err.Error()
		} else if _, ok := ag.(agent.SignInner); ok {
			row.CanSignIn = true
		}
		if pr, ok := ag.(agent.Prober); ok && err == nil {
			e := s.providerFor(ctx, in.Name, pr, force)
			row.Info = e.info
			if e.err != nil {
				row.Err = e.err.Error()
			}
			if e.info.Package != "" {
				row.Latest = s.latestVersion(ctx, e.info.Package, force)
				row.UpdateAvailable = row.Latest != "" && e.info.Version != "" && newerVersion(row.Latest, e.info.Version)
			}
		}
		s.providers.mu.Lock()
		if run := s.providers.runs[in.Name]; run != nil {
			row.Updating, row.UpdateLog, row.UpdateErr, row.UpdatedAt = run.running, run.log, run.err, run.finished
		}
		if lr := s.providers.logins[in.Name]; lr != nil {
			row.SigningIn = lr.finished.IsZero()
			row.LoginURL, row.LoginErr, row.LoginAt = lr.login.URL(), lr.err, lr.finished
			if !row.SigningIn && lr.err == "" {
				row.LoginOK = true
			}
		}
		s.providers.mu.Unlock()
		rows = append(rows, row)
	}
	updates, signedOut := 0, 0
	for _, r := range rows {
		if !r.Inst.Enabled {
			continue
		}
		if r.UpdateAvailable {
			updates++
		}
		if r.Info.LoggedIn != nil && !*r.Info.LoggedIn {
			signedOut++
		}
	}
	s.providers.mu.Lock()
	s.providers.updates = updates
	s.providers.signedOut = signedOut
	s.providers.mu.Unlock()
	return rows
}

func (s *Server) providerFor(ctx context.Context, name string, pr agent.Prober, force bool) providerEntry {
	s.providers.mu.Lock()
	e, ok := s.providers.infos[name]
	s.providers.mu.Unlock()
	if ok && !force && time.Since(e.at) < providerTTL {
		return e
	}
	info, err := pr.Provider(context.WithoutCancel(ctx))
	if err != nil {
		s.Log.Warn("provider check", "instance", name, "err", err)
	}
	e = providerEntry{info: info, err: err, at: time.Now()}
	s.providers.mu.Lock()
	s.providers.infos[name] = e
	s.providers.mu.Unlock()
	return e
}

// forgetProvider drops the cached check for an instance so the next render
// probes again (after a save, an update, or a removal).
func (s *Server) forgetProvider(name string) {
	s.providers.mu.Lock()
	delete(s.providers.infos, name)
	s.providers.mu.Unlock()
	s.cache.mu.Lock()
	delete(s.cache.entries, name)
	s.cache.mu.Unlock()
}

// latestVersion asks the npm registry, which is where both CLIs publish
// their releases. Failures are logged and leave the row without a
// comparison; nothing else depends on the network.
func (s *Server) latestVersion(ctx context.Context, pkg string, force bool) string {
	s.providers.mu.Lock()
	v, ok := s.providers.latest[pkg]
	fresh := time.Since(s.providers.latestAt) < latestTTL
	s.providers.mu.Unlock()
	if ok && fresh && !force {
		return v
	}
	reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.Replace(npmLatestURL, "%s", pkg, 1), nil)
	if err != nil {
		return v
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.Log.Warn("npm latest", "package", pkg, "err", err)
		return v
	}
	defer resp.Body.Close()
	var doc struct {
		Version string `json:"version"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&doc) != nil || doc.Version == "" {
		s.Log.Warn("npm latest", "package", pkg, "status", resp.Status)
		return v
	}
	s.providers.mu.Lock()
	s.providers.latest[pkg] = doc.Version
	s.providers.latestAt = time.Now()
	s.providers.mu.Unlock()
	return doc.Version
}

// updateCount is the cached number of enabled CLIs with a newer release;
// it never blocks, so the sidebar can show it on every render.
func (s *Server) updateCount() int {
	s.providers.mu.Lock()
	defer s.providers.mu.Unlock()
	return s.providers.updates
}

// signedOutCount is the cached number of enabled CLIs that report no
// sign-in, from the last check; like updateCount it never blocks.
func (s *Server) signedOutCount() int {
	s.providers.mu.Lock()
	defer s.providers.mu.Unlock()
	return s.providers.signedOut
}

// agentLooks maps instance names to their driver and tag colour, for the
// marks on thread rows and chips.
func (s *Server) agentLooks() map[string]views.AgentLook {
	out := map[string]views.AgentLook{}
	for _, in := range s.Providers.Instances() {
		out[in.Name] = views.AgentLook{Driver: in.Driver, Color: in.Color, Label: in.DisplayName()}
	}
	return out
}

// watchProviders runs the checks at startup and then every check
// interval, telling open pages when the update count changes. An interval
// of zero means once at startup only.
func (s *Server) watchProviders(ctx context.Context) {
	last := -1
	check := func() {
		s.providerRows(ctx, true)
		if n := s.updateCount() + s.signedOutCount(); n != last {
			last = n
			s.App.Bus.Publish(domain.ProvidersChanged{})
		}
		s.probeAllLimits(ctx)
	}
	go s.watchLimits(ctx)
	check()
	for {
		interval := s.Providers.CheckInterval()
		if interval <= 0 {
			interval = 24 * time.Hour
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			if s.Providers.CheckInterval() > 0 {
				check()
			}
		}
	}
}

// applyInstance makes the registry match the stored instance: enabled
// means a freshly built agent (idle sessions on it are closed so the next
// prompt uses the new settings), disabled means none.
func (s *Server) applyInstance(in providers.Instance) error {
	s.forgetProvider(in.Name)
	s.Usage.SetRoots(s.usageRoots())
	if !in.Enabled {
		s.App.RemoveAgent(in.Name)
		return nil
	}
	ag, err := s.Providers.Build(in)
	if err != nil {
		return err
	}
	s.App.SetAgent(in.Name, ag)
	return nil
}

// startUpdate runs the CLI's own update command for an instance in the
// background. The page shows "updating…" until it ends, then the output
// tail and a fresh version check.
func (s *Server) startUpdate(name string) error {
	in, ok := s.Providers.Get(name)
	if !ok {
		return fmt.Errorf("no instance %q", name)
	}
	ag, err := s.Providers.Build(in)
	if err != nil {
		return err
	}
	pr, ok := ag.(agent.Prober)
	if !ok {
		return errors.New("this provider has no update command")
	}
	info, err := pr.Provider(context.Background())
	if err != nil {
		return err
	}
	words := strings.Fields(info.UpdateCommand)
	if len(words) < 2 {
		return errors.New("this provider has no update command")
	}
	s.providers.mu.Lock()
	s.providers.init()
	if run := s.providers.runs[name]; run != nil && run.running {
		s.providers.mu.Unlock()
		return errors.New("an update is already running")
	}
	run := &updateRun{running: true, started: time.Now()}
	s.providers.runs[name] = run
	s.providers.mu.Unlock()
	s.App.Bus.Publish(domain.ProvidersChanged{})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.Providers.Binary(in), words[1:]...)
		cmd.Env = updateEnv(in.ChildEnv())
		out, err := cmd.CombinedOutput()
		s.Log.Info("provider update finished", "instance", name, "command", info.UpdateCommand, "err", err)
		text := strings.TrimSpace(string(out))
		if len(text) > updateLogMax {
			text = "…" + text[len(text)-updateLogMax:]
		}
		s.providers.mu.Lock()
		run.running = false
		run.finished = time.Now()
		run.log = text
		if err != nil {
			run.err = err.Error()
		}
		s.providers.mu.Unlock()
		s.forgetProvider(name)
		s.providerRows(context.Background(), false)
		s.App.Bus.Publish(domain.ProvidersChanged{})
	}()
	return nil
}

// updateEnv is the inherited environment without Claude Code's nested-run
// guard (an update started from inside a Claude session must still run),
// plus the instance's own variables.
func updateEnv(extra []string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+len(extra))
	for _, kv := range src {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// startLogin begins a sign-in for an instance through its CLI. The page
// shows the link the CLI printed and a box for the code; finishLogin
// runs when the CLI exits.
func (s *Server) startLogin(name string) error {
	in, ok := s.Providers.Get(name)
	if !ok {
		return fmt.Errorf("no instance %q", name)
	}
	ag, err := s.Providers.Build(in)
	if err != nil {
		return err
	}
	si, ok := ag.(agent.SignInner)
	if !ok {
		return errors.New("this provider signs in from a terminal")
	}
	s.providers.mu.Lock()
	s.providers.init()
	if lr := s.providers.logins[name]; lr != nil && lr.finished.IsZero() {
		s.providers.mu.Unlock()
		return errors.New("a sign-in is already running")
	}
	s.providers.mu.Unlock()
	login, err := si.SignIn(context.Background())
	if err != nil {
		return err
	}
	lr := &loginRun{login: login, started: time.Now()}
	s.providers.mu.Lock()
	s.providers.logins[name] = lr
	s.providers.mu.Unlock()
	go s.finishLogin(name, lr)
	// The URL shows up a moment after the process starts; redraw once
	// it is there so the page does not have to be refreshed by hand.
	go func() {
		for i := 0; i < 100 && login.URL() == ""; i++ {
			select {
			case <-login.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		s.App.Bus.Publish(domain.ProvidersChanged{})
	}()
	return nil
}

// finishLogin waits for the CLI, records the outcome and re-probes the
// instance so the row says "signed in" without waiting for the interval.
func (s *Server) finishLogin(name string, lr *loginRun) {
	<-lr.login.Done()
	err := lr.login.Err()
	s.Log.Info("provider sign-in finished", "instance", name, "err", err)
	s.providers.mu.Lock()
	lr.finished = time.Now()
	if err != nil {
		lr.err = err.Error()
	}
	s.providers.mu.Unlock()
	s.forgetProvider(name)
	s.providerRows(context.Background(), false)
	s.App.Bus.Publish(domain.ProvidersChanged{})
}

// currentLogin is the instance's running sign-in, if any.
func (s *Server) currentLogin(name string) (agent.Login, bool) {
	s.providers.mu.Lock()
	defer s.providers.mu.Unlock()
	lr := s.providers.logins[name]
	if lr == nil || !lr.finished.IsZero() {
		return nil, false
	}
	return lr.login, true
}

// newerVersion reports whether a is a later release than b, comparing
// dotted numbers left to right ("2.1.263" > "2.1.261"). Anything after a
// dash (pre-release tags) is ignored.
func newerVersion(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "-")
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

// ---- page data ----

// providersData assembles the page: every row, plus the selected
// instance's detail (configuration or its model catalog).
func (s *Server) providersData(ctx context.Context, sel, tab string, force bool) views.ProvidersData {
	d := views.ProvidersData{
		Rows: s.providerRows(ctx, force), Tab: tab, CheckedAt: time.Now(),
		Interval: int(s.Providers.CheckInterval() / time.Second), Drivers: providers.Drivers, Colors: providers.Colors,
	}
	if d.Tab != "models" {
		d.Tab = "config"
	}
	d.Updates = s.updateCount()
	d.SignedOut = s.signedOutCount()
	if sel == "new" {
		d.Adding = true
		return d
	}
	if sel == "" && len(d.Rows) > 0 {
		sel = d.Rows[0].Inst.Name
	}
	for i := range d.Rows {
		if d.Rows[i].Inst.Name == sel {
			d.Sel = &d.Rows[i]
		}
	}
	if d.Sel != nil && d.Tab == "models" {
		if ag, ok := s.App.Agent(d.Sel.Inst.Name); ok {
			if desc, ok := ag.(agent.Describer); ok {
				e := s.capsFor(ctx, d.Sel.Inst.Name, desc)
				d.Models = e.caps.Models
				if e.err != nil {
					d.ModelsErr = e.err.Error()
				}
			}
		} else {
			d.ModelsErr = "enable the instance to load its catalog"
		}
	}
	return d
}

func providerParams(r *http.Request) (sel, tab string) {
	return r.URL.Query().Get("sel"), r.URL.Query().Get("tab")
}

// ---- handlers ----

type providerSignals struct {
	Driver    string `json:"pvDriver"`
	Name      string `json:"pvName"`
	Label     string `json:"pvLabel"`
	Binary    string `json:"pvBinary"`
	ConfigDir string `json:"pvConfigDir"`
	Env       string `json:"pvEnv"`
	Color     string `json:"pvColor"`
	Model     string `json:"pvModel"`
	Interval  string `json:"pvInterval"`
	Code      string `json:"pvCode"`
}

func envLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (s *Server) providersPage(w http.ResponseWriter, r *http.Request) {
	sel, tab := providerParams(r)
	s.page(r.Context(), "providers", views.Page{View: "providers", Theme: s.theme(r), Sidebar: s.sidebarMode(r), ProviderSel: sel, ProviderTab: tab}).Render(r.Context(), w)
}

// refreshProviders re-runs every check now; every open page redraws
// through the bus, this one directly as well.
func (s *Server) refreshProviders(w http.ResponseWriter, r *http.Request) {
	sel, tab := providerParams(r)
	d := s.providersData(r.Context(), sel, tab, true)
	s.App.Bus.Publish(domain.ProvidersChanged{})
	newSSE(w, r).PatchElementTempl(views.ProvidersPage(d))
}

func (s *Server) addProvider(w http.ResponseWriter, r *http.Request) {
	var sig providerSignals
	datastar.ReadSignals(r, &sig)
	name := strings.TrimSpace(sig.Name)
	if name == "" {
		name = providers.Slug(sig.Label)
	}
	in := providers.Instance{
		Name: name, Driver: sig.Driver, Label: sig.Label, Binary: sig.Binary, ConfigDir: sig.ConfigDir,
		Env: envLines(sig.Env), Color: sig.Color, Enabled: true,
	}
	if _, exists := s.Providers.Get(in.Name); exists {
		s.fail(w, r, fmt.Errorf("an instance named %q already exists", in.Name))
		return
	}
	if err := s.Providers.Put(in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.applyInstance(in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.ProvidersChanged{})
	newSSE(w, r).Redirect("/settings/providers?sel=" + in.Name)
}

func (s *Server) saveProvider(w http.ResponseWriter, r *http.Request) {
	in, ok := s.Providers.Get(r.PathValue("name"))
	if !ok {
		s.fail(w, r, errors.New("no such instance"))
		return
	}
	var sig providerSignals
	datastar.ReadSignals(r, &sig)
	in.Label, in.Binary, in.ConfigDir, in.Env, in.Color = sig.Label, sig.Binary, sig.ConfigDir, envLines(sig.Env), sig.Color
	if err := s.Providers.Put(in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.applyInstance(in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.ProvidersChanged{})
	s.ok(w, r)
}

func (s *Server) toggleProvider(w http.ResponseWriter, r *http.Request) {
	in, ok := s.Providers.Get(r.PathValue("name"))
	if !ok {
		s.fail(w, r, errors.New("no such instance"))
		return
	}
	in.Enabled = !in.Enabled
	if err := s.Providers.Put(in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.applyInstance(in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.ProvidersChanged{})
	s.ok(w, r)
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.Providers.Delete(name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.RemoveAgent(name)
	s.forgetProvider(name)
	s.Usage.SetRoots(s.usageRoots())
	s.App.Bus.Publish(domain.ProvidersChanged{})
	newSSE(w, r).Redirect("/settings/providers")
}

func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.startUpdate(r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

func (s *Server) loginProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.startLogin(r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r)
}

// loginCode hands the pasted code to the running sign-in; the outcome
// arrives through the bus when the CLI exits.
func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	login, ok := s.currentLogin(r.PathValue("name"))
	if !ok {
		s.fail(w, r, errors.New("no sign-in is running; start it again"))
		return
	}
	var sig providerSignals
	datastar.ReadSignals(r, &sig)
	if err := login.Submit(sig.Code); err != nil {
		s.fail(w, r, err)
		return
	}
	newSSE(w, r).MarshalAndPatchSignals(map[string]any{"pvCode": ""})
}

func (s *Server) loginCancel(w http.ResponseWriter, r *http.Request) {
	if login, ok := s.currentLogin(r.PathValue("name")); ok {
		login.Cancel()
	}
	s.ok(w, r)
}

// toggleModel hides a catalog model or shows it again; for an added id it
// removes the addition instead.
func (s *Server) toggleModel(w http.ResponseWriter, r *http.Request) {
	in, ok := s.Providers.Get(r.PathValue("name"))
	if !ok {
		s.fail(w, r, errors.New("no such instance"))
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		s.fail(w, r, errors.New("missing model id"))
		return
	}
	if i := indexOf(in.ExtraModels, id); i >= 0 {
		in.ExtraModels = append(in.ExtraModels[:i], in.ExtraModels[i+1:]...)
	} else if i := indexOf(in.HiddenModels, id); i >= 0 {
		in.HiddenModels = append(in.HiddenModels[:i], in.HiddenModels[i+1:]...)
	} else {
		in.HiddenModels = append(in.HiddenModels, id)
	}
	if err := s.Providers.Put(in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.ProvidersChanged{})
	s.ok(w, r)
}

func (s *Server) addModel(w http.ResponseWriter, r *http.Request) {
	in, ok := s.Providers.Get(r.PathValue("name"))
	if !ok {
		s.fail(w, r, errors.New("no such instance"))
		return
	}
	var sig providerSignals
	datastar.ReadSignals(r, &sig)
	id := strings.TrimSpace(sig.Model)
	if id == "" {
		s.ok(w, r)
		return
	}
	if indexOf(in.ExtraModels, id) < 0 {
		in.ExtraModels = append(in.ExtraModels, id)
	}
	if err := s.Providers.Put(in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.ProvidersChanged{})
	newSSE(w, r).MarshalAndPatchSignals(map[string]any{"pvModel": ""})
}

func (s *Server) setCheckInterval(w http.ResponseWriter, r *http.Request) {
	var sig providerSignals
	datastar.ReadSignals(r, &sig)
	n, err := strconv.Atoi(strings.TrimSpace(sig.Interval))
	if err != nil {
		s.fail(w, r, errors.New("interval must be a number of seconds"))
		return
	}
	if err := s.Providers.SetCheckInterval(n); err != nil {
		s.fail(w, r, err)
		return
	}
	s.App.Bus.Publish(domain.ProvidersChanged{})
	s.ok(w, r)
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

// pickerModels applies an instance's hidden and added model ids to its
// catalog for the composer chips.
func (s *Server) pickerModels(name string, caps agent.Capabilities) agent.Capabilities {
	in, ok := s.Providers.Get(name)
	if !ok || (len(in.HiddenModels) == 0 && len(in.ExtraModels) == 0) {
		return caps
	}
	out := caps
	out.Models = nil
	for _, m := range caps.Models {
		if indexOf(in.HiddenModels, m.ID) < 0 {
			out.Models = append(out.Models, m)
		}
	}
	for _, id := range in.ExtraModels {
		if indexOf(in.HiddenModels, id) < 0 {
			out.Models = append(out.Models, agent.Model{ID: id, DisplayName: id, Description: "Added in Settings > Providers", Group: "Added"})
		}
	}
	return out
}
