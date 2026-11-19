//go:build e2e

// Package e2e drives the real pages in a headless Chromium through Rod.
// Each test builds the whole server in-process (store, app, web) on a
// temporary data directory with only the scripted fake agent, so no test
// can reach a paid agent, and serves it with httptest. Tests run in
// parallel, each with its own server and its own incognito browser
// context, so no two share cookies or storage.
//
//	go test -tags e2e ./e2e/...
//
// The browser is STARCODE_E2E_BROWSER when set, else a Chrome or Chromium
// Rod finds on the machine (Playwright's download included), else one Rod
// downloads into its own cache on the first run.
package e2e

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/devices"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/rod/lib/utils"

	"starcode/internal/agent"
	"starcode/internal/agent/fake"
	"starcode/internal/app"
	"starcode/internal/bus"
	"starcode/internal/domain"
	"starcode/internal/providers"
	"starcode/internal/store"
	"starcode/internal/web"
)

// timeout bounds every wait on a page.
const timeout = 15 * time.Second

var (
	browserOnce sync.Once
	browser     *rod.Browser
	browserErr  error
)

// sharedBrowser starts one browser for the whole run; each test opens
// its own incognito context in it.
func sharedBrowser(t *testing.T) *rod.Browser {
	t.Helper()
	browserOnce.Do(func() {
		l := launcher.New().Headless(true)
		if bin := os.Getenv("STARCODE_E2E_BROWSER"); bin != "" {
			l = l.Bin(bin)
		} else if bin := playwrightChromium(); bin != "" {
			l = l.Bin(bin)
		} else if bin, ok := launcher.LookPath(); ok {
			l = l.Bin(bin)
		}
		u, err := l.Launch()
		if err != nil {
			browserErr = err
			return
		}
		// Rod polls a wait every 100ms, backing off to a second; a
		// page here changes within milliseconds, so poll at 10ms and
		// back off only to 100ms.
		browser = rod.New().ControlURL(u).Sleeper(func() utils.Sleeper {
			return utils.BackoffSleeper(10*time.Millisecond, 100*time.Millisecond, nil)
		})
		browserErr = browser.Connect()
	})
	if browserErr != nil {
		t.Fatalf("start the browser: %v", browserErr)
	}
	return browser
}

// playwrightChromium is the Chromium an earlier Playwright install left
// in its cache, if any: a real browser build, which the headless shell
// Rod would otherwise fetch is not quite.
func playwrightChromium() string {
	home, _ := os.UserHomeDir()
	found, _ := filepath.Glob(filepath.Join(home, ".cache", "ms-playwright", "chromium-*", "chrome-linux*", "chrome"))
	if len(found) == 0 {
		return ""
	}
	return found[len(found)-1]
}

// env is one server with one git project in it.
type env struct {
	t       *testing.T
	url     string
	app     *app.App
	store   *store.Store
	project string // project id
	dir     string // the project's checkout
	srv     *httptest.Server
	fake    *fake.Agent
	browser *rod.Browser // this test's incognito context
}

// newEnv starts a server for t and marks t parallel: nothing in an env
// is shared with another test.
func newEnv(t *testing.T) *env {
	t.Helper()
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "starcode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// A linked PR would start a watch, which reads GitHub.
	st.SetSetting(context.Background(), "watch_linked_prs", "0")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov, err := providers.Open(filepath.Join(data, "providers.json"), map[string]string{}, true, log)
	if err != nil {
		t.Fatal(err)
	}
	fk := fake.New()
	fk.Delay = 10 * time.Millisecond
	a := app.New(st, bus.New(1024), map[string]agent.Agent{"fake": fk}, log)
	a.ScratchDir = filepath.Join(data, "scratch")
	a.AttachDir = filepath.Join(data, "attachments")
	h := web.New(a, log, "", a.AttachDir, prov)
	srv := httptest.NewServer(h)
	// Cleanups run last first: pages close, then the streams they held
	// are cut, then the server and the app stop.
	t.Cleanup(func() {
		a.Shutdown()
		h.Close()
		srv.Close()
	})
	t.Cleanup(srv.CloseClientConnections)

	dir := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# proj\n"), 0o644)
	for _, args := range [][]string{{"init", "--quiet", "-b", "main"}, {"add", "."}, {"commit", "--quiet", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	pid, err := a.AddProject(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sharedBrowser(t).Incognito()
	if err != nil {
		t.Fatalf("open an incognito context: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return &env{t: t, url: srv.URL, app: a, store: st, project: pid, dir: dir, srv: srv, fake: fk, browser: b}
}

// thread makes a fake-agent thread in the never-ask mode, so a turn runs
// to its end without an approval.
func (e *env) thread() string {
	e.t.Helper()
	ctx := context.Background()
	id, err := e.app.CreateThread(ctx, e.project, "fake", "")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.app.SetThreadSettings(ctx, id, app.ThreadSettings{Agent: "fake", PermissionMode: "yolo"}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// turns sends prompts over the app and waits for each turn to end, for
// tests that need a transcript to start from. Nothing watches these
// turns stream, so the fake runs them with no delay between chunks.
func (e *env) turns(id string, prompts ...string) {
	e.t.Helper()
	d := e.fake.Delay
	e.fake.Delay = 0
	defer func() { e.fake.Delay = d }()
	for _, p := range prompts {
		if err := e.app.SendPrompt(context.Background(), id, p); err != nil {
			e.t.Fatal(err)
		}
		e.waitIdle(id)
	}
}

// waitIdle waits until the thread's turn is over.
func (e *env) waitIdle(id string) {
	e.t.Helper()
	e.until("thread idle", func() bool {
		th, err := e.store.Thread(context.Background(), id)
		return err == nil && th.Status == domain.StatusIdle && len(e.items(id, domain.KindResult)) > 0
	})
}

// items are the thread's items of kind.
func (e *env) items(id, kind string) []store.Item {
	all, _ := e.store.Items(context.Background(), id)
	var out []store.Item
	for _, it := range all {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

func (e *env) until(what string, ok func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// page opens path in a new tab, desktop sized or as a phone. confirm()
// answers yes, and a script error on the page fails the test.
func (e *env) page(path string, phone bool) *rod.Page {
	e.t.Helper()
	p := e.browser.MustPage("")
	// Closing the target skips beforeunload, so a page that still holds
	// an unsaved edit does not wait out the deadline on a dialog.
	e.t.Cleanup(func() { proto.TargetCloseTarget{TargetID: p.TargetID}.Call(e.browser) })
	if phone {
		p.MustEmulate(devices.IPhoneX)
	} else {
		p.MustSetViewport(1380, 900, 1, false)
	}
	var mu sync.Mutex
	var errs []string
	go p.EachEvent(func(ev *proto.RuntimeExceptionThrown) {
		mu.Lock()
		defer mu.Unlock()
		d := ev.ExceptionDetails
		msg := d.Text
		if d.Exception != nil {
			msg += " " + d.Exception.Description
		}
		errs = append(errs, msg)
	})()
	e.t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if len(errs) > 0 {
			e.t.Errorf("script errors on %s:\n%s", path, strings.Join(errs, "\n"))
		}
	})
	p.MustEvalOnNewDocument(`window.confirm = () => true`)
	// A failed Must call fails this test instead of panicking the run.
	p = p.Timeout(timeout).WithPanic(func(v any) {
		e.t.Helper()
		e.t.Fatalf("%v", v)
	})
	p.MustNavigate(e.url + path).MustWaitLoad()
	// The stream has connected once the sidebar is drawn by it.
	p.MustElement("#sidebar .side-brand")
	return p
}

// click presses an element from script. Hover tools sit at opacity 0
// until the pointer is over them, which a real click would wait out.
func click(el *rod.Element) {
	el.MustEval(`() => this.click()`)
}

// wait waits until js, a function returning a truth value, holds.
func wait(p *rod.Page, js string, args ...any) {
	p.MustWait(js, args...)
}

// value is a form control's current value.
func value(p *rod.Page, selector string) string {
	return p.MustElement(selector).MustEval(`() => this.value`).String()
}

// reconnect makes the page reopen its stream and waits until the new
// stream has drawn the page: the server sends its first heartbeat after
// the initial render, and the heartbeat calls streamAlive.
func reconnect(p *rod.Page) {
	p.MustEval(`() => {
		window._alive = 0;
		if (!window._aliveWrapped) {
			const f = window.streamAlive;
			window.streamAlive = () => { window._alive++; f(); };
			window._aliveWrapped = true;
		}
		window.dispatchEvent(new Event('stream'));
	}`)
	wait(p, `() => window._alive > 0`)
}
