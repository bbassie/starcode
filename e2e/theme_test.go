//go:build e2e

package e2e

import (
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// scheme makes the page's device report a light or dark system setting.
func scheme(p *rod.Page, v string) {
	err := proto.EmulationSetEmulatedMedia{Features: []*proto.EmulationMediaFeature{{Name: "prefers-color-scheme", Value: v}}}.Call(p)
	if err != nil {
		panic(err)
	}
}

// themePage opens path with no theme cookies. Cookies do not care about
// the port, so a test server would otherwise get the ones an earlier
// test left on the shared browser; the cleanup clears what this test
// leaves.
func themePage(e *env, path string) *rod.Page {
	p := e.page(path, false)
	p.MustSetCookies()
	e.t.Cleanup(func() { p.MustSetCookies() })
	p.MustReload().MustWaitLoad()
	p.MustElement("#sidebar .side-brand")
	return p
}

// saved waits until the theme cookies hold what was picked.
func saved(p *rod.Page, theme, appearance string) {
	wait(p, `(t, a) => document.cookie.includes('theme=' + t) && document.cookie.includes('appearance=' + a)`, theme, appearance)
}

// themeState is <html>'s theme, its half, and theme-color next to --bg.
const themeState = `() => {
	const h = document.documentElement;
	return [h.dataset.theme, h.dataset.mode, document.querySelector('meta[name="theme-color"]').content, getComputedStyle(h).getPropertyValue('--bg').trim()].join(' ');
}`

// TestThemeFollowsTheSystem: the default appearance takes the device's
// half and changes with it, and theme-color stays on the page background.
func TestThemeFollowsTheSystem(t *testing.T) {
	e := newEnv(t)
	p := themePage(e, "/")
	scheme(p, "dark")
	wait(p, `() => document.documentElement.dataset.mode === 'dark'`)
	if got := p.MustEval(themeState).String(); got != "starcode dark #121212 #121212" {
		t.Errorf("dark system: %q", got)
	}
	scheme(p, "light")
	wait(p, `() => document.documentElement.dataset.mode === 'light'`)
	if got := p.MustEval(themeState).String(); got != "starcode light #fcfcfc #fcfcfc" {
		t.Errorf("light system: %q", got)
	}
}

// TestPickTheme picks Ocean and Light in Settings > Appearance: the page
// switches at once, the choice outlives a reload, and a light system no
// longer matters once Dark is picked.
func TestPickTheme(t *testing.T) {
	e := newEnv(t)
	p := themePage(e, "/settings/appearance")
	scheme(p, "dark")
	click(p.MustElementR(".theme-card", "Ocean"))
	wait(p, `() => document.documentElement.dataset.theme === 'ocean'`)
	saved(p, "ocean", "system")
	click(p.MustElementR(".segmented button", "Light"))
	wait(p, `() => document.documentElement.dataset.mode === 'light'`)
	saved(p, "ocean", "light")
	if got := p.MustEval(themeState).String(); got != "ocean light #f5f7f8 #f5f7f8" {
		t.Errorf("after picking: %q", got)
	}
	wait(p, `() => document.querySelector('.theme-card.current')?.textContent.includes('Ocean')`)

	p.MustReload().MustWaitLoad()
	p.MustElement("#sidebar .side-brand")
	if got := p.MustEval(themeState).String(); got != "ocean light #f5f7f8 #f5f7f8" {
		t.Errorf("after reload: %q", got)
	}

	click(p.MustElementR(".segmented button", "Dark"))
	scheme(p, "light")
	wait(p, `() => document.documentElement.dataset.mode === 'dark'`)
	if got := p.MustEval(themeState).String(); got != "ocean dark #17212b #17212b" {
		t.Errorf("dark on a light system: %q", got)
	}
}

// TestOldLightThemeCookie: a browser that picked the old Light theme
// opens in Starcode's light half.
func TestOldLightThemeCookie(t *testing.T) {
	e := newEnv(t)
	p := themePage(e, "/")
	p.MustSetCookies(&proto.NetworkCookieParam{Name: "theme", Value: "light", URL: e.url})
	p.MustReload().MustWaitLoad()
	p.MustElement("#sidebar .side-brand")
	if got := p.MustEval(themeState).String(); got != "starcode light #fcfcfc #fcfcfc" {
		t.Errorf("old light cookie: %q", got)
	}
}
