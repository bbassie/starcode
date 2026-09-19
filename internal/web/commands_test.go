package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/starfederation/datastar-go/datastar"
)

func TestSetSidebarModeCookies(t *testing.T) {
	cookies := func(body, sidebarCookie string) map[string]string {
		r := httptest.NewRequest("POST", "/api/sidebar", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if sidebarCookie != "" {
			r.AddCookie(&http.Cookie{Name: "sidebar", Value: sidebarCookie})
		}
		w := httptest.NewRecorder()
		(&Server{}).setSidebarMode(w, r)
		got := map[string]string{}
		for _, c := range w.Result().Cookies() {
			got[c.Name] = c.Value
		}
		return got
	}
	if got := cookies(`{"sidebar":"project","dense":true}`, ""); got["sidebar"] != "project" || got["dense"] != "1" {
		t.Errorf("both signals: %v", got)
	}
	if got := cookies(`{"dense":false}`, "project"); got["sidebar"] != "project" || got["dense"] != "0" {
		t.Errorf("dense only keeps the cookie's mode: %v", got)
	}
	if got := cookies(`{"dense":true}`, ""); got["sidebar"] != "" || got["dense"] != "1" {
		t.Errorf("dense only, no cookie yet: %v", got)
	}
}

func TestRunsFrom(t *testing.T) {
	s := &Server{}
	if s.runsFrom("/w/a") {
		t.Error("no self-update: runsFrom = true")
	}
	s.Update = &SelfUpdate{Home: "/home/starcode", Running: "/home/starcode"}
	if s.runsFrom("/home") {
		t.Error("running the main binary: runsFrom = true")
	}
	s.Update.Running = "/w/a/starcode"
	if !s.runsFrom("/w/a") || !s.runsFrom("/w/a/") {
		t.Error("running the build in /w/a: runsFrom = false")
	}
	if s.runsFrom("/w/b") || s.runsFrom("") {
		t.Error("runsFrom = true for another worktree or none")
	}
}

// projectFor reads the tid signal; the handler after it must still find
// its own signals in the same POST body.
func TestPeekSignalsLeavesTheBody(t *testing.T) {
	body := `{"tid":"abc","_file":"package main\n","prBody":"hello"}`
	r := httptest.NewRequest(http.MethodPost, "/api/git/p1/file", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	var first struct {
		TID string `json:"tid"`
	}
	peekSignals(r, &first)
	if first.TID != "abc" {
		t.Fatalf("tid = %q", first.TID)
	}
	var second struct {
		File string `json:"_file"`
		Body string `json:"prBody"`
	}
	if err := datastar.ReadSignals(r, &second); err != nil {
		t.Fatal(err)
	}
	if second.File != "package main\n" || second.Body != "hello" {
		t.Fatalf("second read lost the signals: %+v", second)
	}
}
