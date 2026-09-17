package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"starcode/internal/agent"
)

// Subscription usage. Two sources feed the same rows:
//
//   - The get_usage control request (Limits, on demand) reports every
//     window at once, as 0..100 percentages with ISO reset times.
//   - The rate_limit_event line (streamed during a turn) names one window
//     at a time, as a 0..1 fraction with an epoch-seconds reset.
//
// Both map onto the ids below so a mid-turn update lands on the row the
// probe drew.

// overageEventType is how the streamed event names the model-scoped weekly
// (Fable today). get_usage names the same window by the model's display
// name, so the probe records that name and the event mapper reuses it.
const overageEventType = "seven_day_overage_included"

// Limits asks the CLI for the account's usage windows. Like Capabilities
// it goes through the stream-json control channel without a prompt, so it
// costs a process launch and no tokens. Accounts on an API key report no
// windows; that is an error here.
func (a *Agent) Limits(ctx context.Context) (agent.Limits, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.binary, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	cmd.Env = childEnv(a.env)
	cmd.Stdin = strings.NewReader(`{"type":"control_request","request_id":"usage","request":{"subtype":"get_usage"}}` + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.Limits{}, err
	}
	if err := cmd.Start(); err != nil {
		return agent.Limits{}, err
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string          `json:"request_id"`
				Subtype   string          `json:"subtype"`
				Error     string          `json:"error"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != "usage" {
			continue
		}
		if msg.Response.Subtype != "success" {
			return agent.Limits{}, fmt.Errorf("claude: get_usage: %s", msg.Response.Error)
		}
		limits, scoped, err := usageLimits(msg.Response.Response)
		if err != nil {
			return agent.Limits{}, err
		}
		limits.CheckedAt = time.Now()
		a.lmu.Lock()
		a.overageModel = scoped
		a.lmu.Unlock()
		return limits, nil
	}
	if err := ctx.Err(); err != nil {
		return agent.Limits{}, fmt.Errorf("claude: get_usage: %w", err)
	}
	return agent.Limits{}, errors.New("claude: get_usage returned nothing (is the CLI signed in?)")
}

// scopedModel is the display name of the model-scoped weekly the last
// Limits probe saw, empty until a probe ran.
func (a *Agent) scopedModel() string {
	a.lmu.Lock()
	defer a.lmu.Unlock()
	return a.overageModel
}

// limitsFromUsage maps the get_usage response body (the inner "response"
// object) to Limits. CheckedAt is left for the caller.
func limitsFromUsage(raw json.RawMessage) (agent.Limits, error) {
	limits, _, err := usageLimits(raw)
	return limits, err
}

// usageLimits is limitsFromUsage plus the display name of the first
// model-scoped window, which is the one the streamed overage event refers
// to. The CLI filters model_scoped to the overage-included list, one model
// today.
func usageLimits(raw json.RawMessage) (agent.Limits, string, error) {
	var res struct {
		RateLimitsAvailable bool `json:"rate_limits_available"`
		RateLimits          *struct {
			FiveHour *usageWindow `json:"five_hour"`
			SevenDay *usageWindow `json:"seven_day"`
			Limits   []struct {
				Kind     string   `json:"kind"`
				Group    string   `json:"group"`
				Percent  *float64 `json:"percent"`
				ResetsAt string   `json:"resets_at"`
				Scope    *struct {
					Model *struct {
						ID          string `json:"id"`
						DisplayName string `json:"display_name"`
					} `json:"model"`
				} `json:"scope"`
			} `json:"limits"`
			ModelScoped []struct {
				DisplayName string   `json:"display_name"`
				Utilization *float64 `json:"utilization"`
				ResetsAt    string   `json:"resets_at"`
			} `json:"model_scoped"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return agent.Limits{}, "", fmt.Errorf("claude: get_usage: bad response: %w", err)
	}
	if !res.RateLimitsAvailable || res.RateLimits == nil {
		return agent.Limits{}, "", errors.New("claude: the account has no subscription windows (an API key account, or the CLI reported none)")
	}
	rl := res.RateLimits
	var limits agent.Limits
	scoped := ""
	for _, l := range rl.Limits {
		if l.Percent == nil {
			continue
		}
		reset := parseISO(l.ResetsAt)
		switch l.Kind {
		case "session":
			limits.Windows = append(limits.Windows, sessionWindow(*l.Percent, reset))
		case "weekly_all":
			limits.Windows = append(limits.Windows, weeklyWindow(*l.Percent, reset))
		case "weekly_scoped":
			name := ""
			if l.Scope != nil && l.Scope.Model != nil {
				name = l.Scope.Model.DisplayName
				if name == "" {
					name = l.Scope.Model.ID
				}
			}
			if name == "" {
				continue
			}
			limits.Windows = append(limits.Windows, scopedWindow(name, *l.Percent, reset))
			if scoped == "" {
				scoped = name
			}
		default:
			kind := l.Group
			switch kind {
			case "session", "weekly", "monthly":
			default:
				kind = "other"
			}
			limits.Windows = append(limits.Windows, agent.LimitWindow{
				ID: l.Kind, Kind: kind, Label: strings.ReplaceAll(l.Kind, "_", " "),
				Percent: clampPercent(*l.Percent), ResetsAt: reset,
			})
		}
	}
	if len(limits.Windows) > 0 {
		return limits, scoped, nil
	}
	// Older CLIs have no "limits" array, only the two account windows and
	// the model-scoped list.
	if w := rl.FiveHour; w != nil && w.Utilization != nil {
		limits.Windows = append(limits.Windows, sessionWindow(*w.Utilization, parseISO(w.ResetsAt)))
	}
	if w := rl.SevenDay; w != nil && w.Utilization != nil {
		limits.Windows = append(limits.Windows, weeklyWindow(*w.Utilization, parseISO(w.ResetsAt)))
	}
	for _, m := range rl.ModelScoped {
		if m.Utilization == nil || m.DisplayName == "" {
			continue
		}
		limits.Windows = append(limits.Windows, scopedWindow(m.DisplayName, *m.Utilization, parseISO(m.ResetsAt)))
		if scoped == "" {
			scoped = m.DisplayName
		}
	}
	if len(limits.Windows) == 0 {
		return agent.Limits{}, "", errors.New("claude: get_usage reported no windows")
	}
	return limits, scoped, nil
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// rateLimitInfo is the SDK's SDKRateLimitInfo as carried by a streamed
// rate_limit_event. Utilization is a 0..1 fraction; resetsAt is epoch
// seconds.
type rateLimitInfo struct {
	Status        string   `json:"status"`
	RateLimitType string   `json:"rateLimitType"`
	Utilization   *float64 `json:"utilization"`
	ResetsAt      int64    `json:"resetsAt"`
}

// limitWindowFromEvent maps one streamed event onto the row the probe drew.
// Events without a utilization say nothing about usage. An overage event
// before any probe named the scoped model is dropped: guessing a name would
// draw a row the next probe cannot reconcile.
func limitWindowFromEvent(info rateLimitInfo, scopedModel string) (agent.LimitWindow, bool) {
	if info.Utilization == nil {
		return agent.LimitWindow{}, false
	}
	// Rounded so 0.29 reads as 29, not 28.999999999999996.
	percent := math.Round(*info.Utilization*10000) / 100
	var reset time.Time
	if info.ResetsAt > 0 {
		reset = time.Unix(info.ResetsAt, 0).UTC()
	}
	switch info.RateLimitType {
	case "five_hour":
		return sessionWindow(percent, reset), true
	case "seven_day":
		return weeklyWindow(percent, reset), true
	case overageEventType:
		if scopedModel == "" {
			return agent.LimitWindow{}, false
		}
		return scopedWindow(scopedModel, percent, reset), true
	}
	return agent.LimitWindow{}, false
}

func sessionWindow(percent float64, reset time.Time) agent.LimitWindow {
	return agent.LimitWindow{ID: "session", Kind: "session", Label: "Session", Percent: clampPercent(percent), ResetsAt: reset}
}

func weeklyWindow(percent float64, reset time.Time) agent.LimitWindow {
	return agent.LimitWindow{ID: "weekly", Kind: "weekly", Label: "Weekly", Percent: clampPercent(percent), ResetsAt: reset}
}

func scopedWindow(model string, percent float64, reset time.Time) agent.LimitWindow {
	return agent.LimitWindow{
		ID: "weekly_" + slug(model), Kind: "weekly", Label: "Weekly · " + model,
		Percent: clampPercent(percent), ResetsAt: reset,
	}
}

// slug lowercases a display name and turns every run of other characters
// into one underscore: "Fable" is "fable", "Opus 4.6" is "opus_4_6".
func slug(s string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(r)
		} else {
			gap = true
		}
	}
	return b.String()
}

func clampPercent(p float64) float64 {
	return min(max(p, 0), 100)
}

// parseISO reads the reset times get_usage sends
// ("2026-09-16T23:00:00.121733+00:00"); zero when absent or unreadable.
func parseISO(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
