package claude

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
)

// Recorded from claude 2.1.7x on a Max account: the "limits" array names
// every window, the older fields repeat two of them.
const usageResponse = `{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":29,"resets_at":"2026-09-16T23:00:00.121733+00:00"},"seven_day":{"utilization":6,"resets_at":"2026-09-21T22:59:59.121753+00:00"},"limits":[{"kind":"session","group":"session","percent":29,"severity":"normal","resets_at":"2026-09-16T23:00:00.121733+00:00","scope":null,"is_active":true},{"kind":"weekly_all","group":"weekly","percent":6,"resets_at":"2026-09-21T22:59:59.121753+00:00","scope":null},{"kind":"weekly_scoped","group":"weekly","percent":10,"resets_at":"2026-09-21T22:59:59.121952+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}],"model_scoped":[{"display_name":"Fable","utilization":10,"resets_at":"2026-09-21T22:59:59.121952+00:00"}]}}`

// The same account as an older CLI reports it: no "limits" array.
const usageResponseOld = `{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":29,"resets_at":"2026-09-16T23:00:00.121733+00:00"},"seven_day":{"utilization":6,"resets_at":"2026-09-21T22:59:59.121753+00:00"},"model_scoped":[{"display_name":"Fable","utilization":10,"resets_at":"2026-09-21T22:59:59.121952+00:00"}]}}`

func TestLimitsFromUsage(t *testing.T) {
	for name, raw := range map[string]string{"limits array": usageResponse, "older fields": usageResponseOld} {
		t.Run(name, func(t *testing.T) {
			limits, err := limitsFromUsage(json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			want := []agent.LimitWindow{
				{ID: "session", Kind: "session", Label: "Session", Percent: 29, ResetsAt: time.Date(2026, 9, 16, 23, 0, 0, 121733000, time.UTC)},
				{ID: "weekly", Kind: "weekly", Label: "Weekly", Percent: 6, ResetsAt: time.Date(2026, 9, 21, 22, 59, 59, 121753000, time.UTC)},
				{ID: "weekly_fable", Kind: "weekly", Label: "Weekly · Fable", Percent: 10, ResetsAt: time.Date(2026, 9, 21, 22, 59, 59, 121952000, time.UTC)},
			}
			if len(limits.Windows) != len(want) {
				t.Fatalf("windows = %+v, want %d", limits.Windows, len(want))
			}
			for i, w := range want {
				got := limits.Windows[i]
				if got.ID != w.ID || got.Kind != w.Kind || got.Label != w.Label || got.Percent != w.Percent || !got.ResetsAt.Equal(w.ResetsAt) {
					t.Errorf("window %d = %+v, want %+v", i, got, w)
				}
			}
		})
	}
}

func TestUsageLimitsNamesTheScopedModel(t *testing.T) {
	for _, raw := range []string{usageResponse, usageResponseOld} {
		_, scoped, err := usageLimits(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		if scoped != "Fable" {
			t.Errorf("scoped model = %q, want Fable", scoped)
		}
	}
}

func TestLimitsFromUsageWithoutSubscription(t *testing.T) {
	_, err := limitsFromUsage(json.RawMessage(`{"subscription_type":null,"rate_limits_available":false}`))
	if err == nil || !strings.Contains(err.Error(), "no subscription windows") {
		t.Fatalf("err = %v, want the no-subscription error", err)
	}
	if _, err := limitsFromUsage(json.RawMessage(`not json`)); err == nil {
		t.Fatal("want an error for a malformed response")
	}
}

func TestLimitsFromUsageKeepsUnknownKinds(t *testing.T) {
	limits, err := limitsFromUsage(json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"limits":[{"kind":"monthly_all","group":"monthly","percent":120},{"kind":"surface_thing","group":"surface","percent":-3}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(limits.Windows) != 2 {
		t.Fatalf("windows = %+v", limits.Windows)
	}
	if w := limits.Windows[0]; w.ID != "monthly_all" || w.Kind != "monthly" || w.Percent != 100 || !w.ResetsAt.IsZero() {
		t.Errorf("monthly window = %+v", w)
	}
	if w := limits.Windows[1]; w.ID != "surface_thing" || w.Kind != "other" || w.Label != "surface thing" || w.Percent != 0 {
		t.Errorf("other window = %+v", w)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Fable": "fable", "Opus 4.6": "opus_4_6", " Sonnet--4.5 ": "sonnet_4_5", "Ünïcode": "ünïcode"} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimitWindowFromEvent(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	reset := time.Unix(1788525000, 0).UTC()
	cases := []struct {
		name   string
		info   rateLimitInfo
		scoped string
		want   *agent.LimitWindow
	}{
		{"five_hour", rateLimitInfo{RateLimitType: "five_hour", Utilization: pct(0.29), ResetsAt: 1788525000}, "",
			&agent.LimitWindow{ID: "session", Kind: "session", Label: "Session", Percent: 29, ResetsAt: reset}},
		{"seven_day", rateLimitInfo{RateLimitType: "seven_day", Utilization: pct(0.06)}, "",
			&agent.LimitWindow{ID: "weekly", Kind: "weekly", Label: "Weekly", Percent: 6}},
		{"overage with a known model", rateLimitInfo{RateLimitType: overageEventType, Utilization: pct(0.1), ResetsAt: 1788525000}, "Fable",
			&agent.LimitWindow{ID: "weekly_fable", Kind: "weekly", Label: "Weekly · Fable", Percent: 10, ResetsAt: reset}},
		{"overage before any probe", rateLimitInfo{RateLimitType: overageEventType, Utilization: pct(0.1)}, "", nil},
		{"no utilization", rateLimitInfo{RateLimitType: "five_hour", ResetsAt: 1788525000}, "", nil},
		{"unknown type", rateLimitInfo{RateLimitType: "per_minute", Utilization: pct(0.5)}, "", nil},
		{"clamped", rateLimitInfo{RateLimitType: "five_hour", Utilization: pct(1.4)}, "",
			&agent.LimitWindow{ID: "session", Kind: "session", Label: "Session", Percent: 100}},
	}
	for _, c := range cases {
		got, ok := limitWindowFromEvent(c.info, c.scoped)
		if c.want == nil {
			if ok {
				t.Errorf("%s: got %+v, want nothing", c.name, got)
			}
			continue
		}
		if !ok {
			t.Errorf("%s: got nothing, want %+v", c.name, *c.want)
			continue
		}
		if got.ID != c.want.ID || got.Kind != c.want.Kind || got.Label != c.want.Label || got.Percent != c.want.Percent || !got.ResetsAt.Equal(c.want.ResetsAt) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, *c.want)
		}
	}
}

func TestParseRateLimitEmitsLimits(t *testing.T) {
	st := newState(nil)
	// The status-only events the recorded stream carries say nothing.
	if events := feed(st, lineRateAllowed); len(events) != 0 {
		t.Fatalf("status-only event: got %v", kinds(events))
	}
	// With a utilization the window is reported, and the throttle notice
	// keeps coming alongside it.
	events := feed(st, `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1788525000,"rateLimitType":"five_hour","utilization":0.995},"session_id":"s1"}`)
	if got := kinds(events); len(got) != 2 || got[0] != agent.KindNotice || got[1] != agent.KindLimits {
		t.Fatalf("kinds = %v", got)
	}
	w := events[1].Limits.Windows
	if len(w) != 1 || w[0].ID != "session" || w[0].Percent != 99.5 || !w[0].ResetsAt.Equal(time.Unix(1788525000, 0)) {
		t.Fatalf("limits = %+v", w)
	}
	// The overage event needs the name a probe recorded.
	overage := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"seven_day_overage_included","utilization":0.12},"session_id":"s1"}`
	if events := feed(st, overage); len(events) != 0 {
		t.Fatalf("overage before a probe: got %v", kinds(events))
	}
	a := New()
	a.overageModel = "Fable"
	st.scopedModel = a.scopedModel
	ev := one(t, feed(st, overage), agent.KindLimits)
	if w := ev.Limits.Windows; len(w) != 1 || w[0].ID != "weekly_fable" || w[0].Label != "Weekly · Fable" || w[0].Percent != 12 {
		t.Fatalf("overage limits = %+v", w)
	}
}
