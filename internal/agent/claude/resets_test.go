package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResetCredits(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	raw := `{"eligible": true, "next_grant_id": "g2", "grants": [
		{"id": "g1", "resets_left": 1, "usable_now": true, "ends_at": "2026-10-01T00:00:00Z"},
		{"id": "g2", "resets_left": 2, "usable_now": true, "ends_at": "2026-10-20T00:00:00+00:00"},
		{"id": "g3", "resets_left": 1, "usable_now": true, "paused": true},
		{"id": "g4", "resets_left": 1, "usable_now": true},
		{"id": "BAD ID", "resets_left": 5, "usable_now": true}
	]}`
	rc := resetCredits(json.RawMessage(raw), now)
	if rc == nil || rc.Available != 3 || rc.NextID != "g2" || !rc.Expires.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("resetCredits = %+v", rc)
	}
	for _, raw := range []string{
		`{"eligible": false, "ineligible_reason": "plan_changed", "grants": [], "next_grant_id": null}`,
		`{"eligible": true, "next_grant_id": "g1", "grants": [{"id": "g1", "resets_left": 1, "usable_now": false}]}`,
		`null`, ``,
	} {
		if rc := resetCredits(json.RawMessage(raw), now); rc != nil {
			t.Errorf("resetCredits(%s) = %+v, want none", raw, rc)
		}
	}
}

func TestRedeemReset(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the login is in the Keychain on macOS")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth": {"accessToken": "tok"}}`), 0o600)
	os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount": {"organizationUuid": "org-1"}}`), 0o600)
	result := "reset"
	var got struct {
		path, auth string
		body       map[string]string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("authorization")
		json.NewDecoder(r.Body).Decode(&got.body)
		if result == "429" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"result": result})
	}))
	defer srv.Close()
	old := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = old }()

	a := New(WithEnv([]string{"CLAUDE_CONFIG_DIR=" + dir}))
	a.version = "9.9.9"
	ctx := context.Background()
	out, err := a.RedeemReset(ctx, "g2", "req-1")
	if err != nil || out != "Usage reset" {
		t.Fatalf("RedeemReset = %q, %v", out, err)
	}
	if got.path != "/api/organizations/org-1/reset_rate_limits" || got.auth != "Bearer tok" || got.body["grant_id"] != "g2" || got.body["request_id"] != "req-1" || got.body["program"] != "cedar_ember" {
		t.Errorf("request = %+v", got)
	}
	result = "unavailable"
	if _, err := a.RedeemReset(ctx, "g2", "req-1"); err == nil || !strings.Contains(err.Error(), "could not confirm") {
		t.Errorf("unavailable: %v", err)
	}
	result = "429"
	if _, err := a.RedeemReset(ctx, "g2", "req-1"); err == nil || !strings.Contains(err.Error(), "rate limiting") {
		t.Errorf("429: %v", err)
	}
	if _, err := a.RedeemReset(ctx, "../x", "req-1"); err == nil {
		t.Error("a malformed grant id was sent")
	}
}
