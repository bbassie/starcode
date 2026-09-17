package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"starcode/internal/agent"
)

// Subscription usage. The account/rateLimits/read response and the
// account/rateLimits/updated notification carry the same RateLimitSnapshot
// (see `codex app-server generate-json-schema`), so one mapper serves the
// probe and the mid-turn update, and both name windows by the same ids.

// rateLimitSnapshot is the part of codex's RateLimitSnapshot the adapter
// reads. primary and secondary are positions, not durations: on paid plans
// they are the 5-hour and weekly windows, on Free and Go one monthly
// allowance.
type rateLimitSnapshot struct {
	LimitID   string           `json:"limitId"`
	PlanType  string           `json:"planType"`
	Primary   *rateLimitWindow `json:"primary"`
	Secondary *rateLimitWindow `json:"secondary"`
}

type rateLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	ResetsAt           int64    `json:"resetsAt"`           // epoch seconds, 0 when unknown
	WindowDurationMins *int64   `json:"windowDurationMins"` // absent on older CLIs
}

const (
	sessionMins = 5 * 60
	weekMins    = 7 * 24 * 60
	monthMins   = 30 * 24 * 60
)

// Limits reads the account's usage windows from the shared app-server. It
// starts the server when none runs but sends no prompt.
func (a *Agent) Limits(ctx context.Context) (agent.Limits, error) {
	c, err := a.ensureClient(ctx)
	if err != nil {
		return agent.Limits{}, err
	}
	raw, err := c.call(ctx, "account/rateLimits/read", nil)
	if err != nil {
		return agent.Limits{}, err
	}
	var res struct {
		RateLimits rateLimitSnapshot            `json:"rateLimits"`
		ByLimitID  map[string]rateLimitSnapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return agent.Limits{}, fmt.Errorf("codex: account/rateLimits/read: bad result: %w", err)
	}
	// The single-bucket view can describe another metered limit; the map
	// names the main one outright when the CLI is new enough to send it.
	snap := res.RateLimits
	if s, ok := res.ByLimitID["codex"]; ok {
		snap = s
	}
	windows := limitWindows(snap)
	if len(windows) == 0 {
		return agent.Limits{}, errors.New("codex: the account reported no subscription windows")
	}
	return agent.Limits{CheckedAt: time.Now(), Windows: windows}, nil
}

// limitWindows maps a snapshot to windows. Snapshots for another metered
// limit (a model-specific one such as Spark) are dropped so they never
// replace the main rows; older CLIs omit the limit id and mean the main
// one. windowDurationMins decides the kind when present; otherwise the plan
// does.
func limitWindows(snap rateLimitSnapshot) []agent.LimitWindow {
	if snap.LimitID != "" && snap.LimitID != "codex" {
		return nil
	}
	primaryMins := int64(sessionMins)
	if snap.PlanType == "free" || snap.PlanType == "go" {
		primaryMins = monthMins
	}
	var out []agent.LimitWindow
	for _, p := range []struct {
		id       string
		w        *rateLimitWindow
		fallback int64
	}{{"primary", snap.Primary, primaryMins}, {"secondary", snap.Secondary, weekMins}} {
		if p.w == nil || p.w.UsedPercent == nil {
			continue
		}
		mins := p.fallback
		if p.w.WindowDurationMins != nil {
			mins = *p.w.WindowDurationMins
		}
		kind, label := "session", "Session"
		switch {
		case mins >= monthMins:
			kind, label = "monthly", "Monthly"
		case mins >= weekMins:
			kind, label = "weekly", "Weekly"
		}
		var reset time.Time
		if p.w.ResetsAt > 0 {
			reset = time.Unix(p.w.ResetsAt, 0).UTC()
		}
		out = append(out, agent.LimitWindow{
			ID: p.id, Kind: kind, Label: label,
			Percent: min(max(*p.w.UsedPercent, 0), 100), ResetsAt: reset,
		})
	}
	return out
}

// limitsUpdated handles account/rateLimits/updated. The notification is
// account-wide and names no thread, so every session on this server gets
// the update; the receiver merges by window id, so the repeats are
// harmless.
func (c *client) limitsUpdated(params json.RawMessage) {
	var p struct {
		RateLimits rateLimitSnapshot `json:"rateLimits"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		c.log.Debug("codex: unparsable rate limit update", "err", err)
		return
	}
	windows := limitWindows(p.RateLimits)
	if len(windows) == 0 {
		return
	}
	c.mu.Lock()
	sessions := make([]*session, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	c.mu.Unlock()
	for _, s := range sessions {
		s.emit(agent.Event{Kind: agent.KindLimits, Limits: &agent.Limits{CheckedAt: time.Now(), Windows: windows}})
	}
}
