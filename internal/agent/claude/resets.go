package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"starcode/internal/agent"
)

// Banked resets: Claude plans can hold grants that clear the usage
// windows once each (the CLI's "cedar_ember" program). The CLI reads them
// from the OAuth usage endpoint and claims one against the organization;
// this does the same with the login the CLI keeps in its config folder.
// macOS keeps that login in the Keychain, so there they are not offered.
// The endpoints are the CLI's own, not a published API, so every read
// failure means "no resets" and the bars carry on without them.

const (
	apiBase      = "https://api.anthropic.com"
	resetProgram = "cedar_ember"
)

// apiClient is the HTTP client for these calls; tests point apiBaseURL
// at a local server.
var (
	apiClient  = &http.Client{Timeout: 25 * time.Second}
	apiBaseURL = apiBase
)

var (
	grantIDPattern   = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// readResets reads the account's banked resets; nil when it has none, is
// not eligible, or anything fails.
func (a *Agent) readResets(ctx context.Context) *agent.ResetCredits {
	token := a.accessToken()
	if token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/api/oauth/usage?cedar_ember=1&skip_spend=1", nil)
	if err != nil {
		return nil
	}
	a.apiHeaders(ctx, req, token)
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		CedarEmber json.RawMessage `json:"cedar_ember"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	return resetCredits(body.CedarEmber, time.Now())
}

// resetCredits reads the usage response's cedar_ember block. Grants that
// are paused, not usable now or past their end do not count, and without
// a usable next grant there is nothing to redeem.
func resetCredits(raw json.RawMessage, now time.Time) *agent.ResetCredits {
	var block struct {
		Eligible bool `json:"eligible"`
		Grants   []struct {
			ID         string `json:"id"`
			ResetsLeft int    `json:"resets_left"`
			EndsAt     string `json:"ends_at"`
			Paused     bool   `json:"paused"`
			UsableNow  bool   `json:"usable_now"`
		} `json:"grants"`
		NextGrantID string `json:"next_grant_id"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &block) != nil || !block.Eligible {
		return nil
	}
	rc := &agent.ResetCredits{}
	found := false
	for _, g := range block.Grants {
		if !grantIDPattern.MatchString(g.ID) || g.ResetsLeft <= 0 || g.Paused || !g.UsableNow {
			continue
		}
		var ends time.Time
		if g.EndsAt != "" {
			t, err := time.Parse(time.RFC3339Nano, g.EndsAt)
			if err != nil || !t.After(now) {
				continue
			}
			ends = t
		}
		rc.Available += g.ResetsLeft
		if g.ID == block.NextGrantID {
			found = true
			rc.NextID, rc.Expires = g.ID, ends
		}
	}
	if !found {
		return nil
	}
	return rc
}

// RedeemReset claims grantID. A claim Claude could not confirm returns an
// error; retrying with the same requestID asks about the same claim.
func (a *Agent) RedeemReset(ctx context.Context, grantID, requestID string) (string, error) {
	if !grantIDPattern.MatchString(grantID) || !requestIDPattern.MatchString(requestID) {
		return "", errors.New("claude returned a malformed reset credit")
	}
	token := a.accessToken()
	org := a.organization()
	if token == "" || org == "" {
		return "", errors.New("sign in to Claude again to redeem resets")
	}
	payload, _ := json.Marshal(map[string]string{"program": resetProgram, "grant_id": grantID, "request_id": requestID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/api/organizations/"+url.PathEscape(org)+"/reset_rate_limits", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	a.apiHeaders(ctx, req, token)
	req.Header.Set("content-type", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("claude could not redeem the reset: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return "", errors.New("claude is rate limiting resets; try again soon")
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", errors.New("sign in to Claude again to redeem resets")
	case http.StatusOK:
	default:
		return "", fmt.Errorf("claude could not redeem the reset (HTTP %d)", resp.StatusCode)
	}
	var body struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("claude could not redeem the reset: %w", err)
	}
	switch body.Result {
	case "reset":
		return "Usage reset", nil
	case "not_limited":
		return "Nothing to reset: the usage is not at a limit", nil
	case "already_used":
		return "That reset was already used", nil
	case "ineligible":
		return "No reset left to redeem", nil
	case "cooldown":
		return "", errors.New("claude resets are cooling down; try again later")
	}
	// "unavailable" or anything new: Claude could not say whether the
	// claim landed, so the caller keeps the request id for a retry.
	return "", errors.New("claude could not confirm the reset; if you are still limited in a moment, try again")
}

// apiHeaders are the headers the CLI sends with its OAuth login.
func (a *Agent) apiHeaders(ctx context.Context, req *http.Request, token string) {
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("user-agent", "claude-cli/"+a.cliVersion(ctx)+" (external, cli)")
}

// cliVersion is the installed CLI's version, read once.
func (a *Agent) cliVersion(ctx context.Context) string {
	a.lmu.Lock()
	v := a.version
	a.lmu.Unlock()
	if v != "" {
		return v
	}
	v = "2.1.0"
	if out, err := cliOutput(ctx, a.binary, a.env, "--version"); err == nil {
		if f := strings.Fields(out); len(f) > 0 {
			v = f[0]
		}
	}
	a.lmu.Lock()
	a.version = v
	a.lmu.Unlock()
	return v
}

// accessToken is the OAuth token the CLI keeps in its config folder;
// empty on macOS (the Keychain has it) and when signed out.
func (a *Agent) accessToken() string {
	if runtime.GOOS == "darwin" {
		return ""
	}
	dir := claudeConfigDir(a.env)
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return ""
	}
	var c struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return strings.TrimSpace(c.OAuth.AccessToken)
}

// organization is the signed-in account's organization, from the CLI's
// account file: beside its settings when CLAUDE_CONFIG_DIR names a
// folder, in the home folder otherwise.
func (a *Agent) organization() string {
	path := ""
	if explicitConfigDir(a.env) {
		path = filepath.Join(claudeConfigDir(a.env), ".claude.json")
	} else if home, err := os.UserHomeDir(); err == nil {
		path = filepath.Join(home, ".claude.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c struct {
		Account struct {
			Org string `json:"organizationUuid"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return strings.TrimSpace(c.Account.Org)
}

// explicitConfigDir is whether CLAUDE_CONFIG_DIR is set, for this agent
// or for the whole process.
func explicitConfigDir(extra []string) bool {
	for _, kv := range extra {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != ""
}
