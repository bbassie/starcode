package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"starcode/internal/agent"
)

func TestLimitWindows(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	mins := func(v int64) *int64 { return &v }
	check := func(t *testing.T, got []agent.LimitWindow, want ...agent.LimitWindow) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("windows = %+v, want %+v", got, want)
		}
		for i := range want {
			g, w := got[i], want[i]
			if g.ID != w.ID || g.Kind != w.Kind || g.Label != w.Label || g.Percent != w.Percent || !g.ResetsAt.Equal(w.ResetsAt) {
				t.Errorf("window %d = %+v, want %+v", i, g, w)
			}
		}
	}

	t.Run("plus plan with durations", func(t *testing.T) {
		got := limitWindows(rateLimitSnapshot{
			LimitID: "codex", PlanType: "plus",
			Primary:   &rateLimitWindow{UsedPercent: pct(12), ResetsAt: 1758000000, WindowDurationMins: mins(300)},
			Secondary: &rateLimitWindow{UsedPercent: pct(3), ResetsAt: 1758500000, WindowDurationMins: mins(10080)},
		})
		check(t, got,
			agent.LimitWindow{ID: "primary", Kind: "session", Label: "Session", Percent: 12, ResetsAt: time.Unix(1758000000, 0)},
			agent.LimitWindow{ID: "secondary", Kind: "weekly", Label: "Weekly", Percent: 3, ResetsAt: time.Unix(1758500000, 0)},
		)
	})
	t.Run("older CLI without durations or limit id", func(t *testing.T) {
		got := limitWindows(rateLimitSnapshot{
			PlanType:  "pro",
			Primary:   &rateLimitWindow{UsedPercent: pct(50)},
			Secondary: &rateLimitWindow{UsedPercent: pct(120)},
		})
		check(t, got,
			agent.LimitWindow{ID: "primary", Kind: "session", Label: "Session", Percent: 50},
			agent.LimitWindow{ID: "secondary", Kind: "weekly", Label: "Weekly", Percent: 100},
		)
	})
	t.Run("free plan is monthly", func(t *testing.T) {
		got := limitWindows(rateLimitSnapshot{PlanType: "free", Primary: &rateLimitWindow{UsedPercent: pct(7)}})
		check(t, got, agent.LimitWindow{ID: "primary", Kind: "monthly", Label: "Monthly", Percent: 7})
	})
	t.Run("duration beats the plan", func(t *testing.T) {
		got := limitWindows(rateLimitSnapshot{PlanType: "go", Primary: &rateLimitWindow{UsedPercent: pct(7), WindowDurationMins: mins(300)}})
		check(t, got, agent.LimitWindow{ID: "primary", Kind: "session", Label: "Session", Percent: 7})
		got = limitWindows(rateLimitSnapshot{PlanType: "plus", Primary: &rateLimitWindow{UsedPercent: pct(7), WindowDurationMins: mins(monthMins)}})
		check(t, got, agent.LimitWindow{ID: "primary", Kind: "monthly", Label: "Monthly", Percent: 7})
	})
	t.Run("another metered limit is ignored", func(t *testing.T) {
		if got := limitWindows(rateLimitSnapshot{LimitID: "spark", Primary: &rateLimitWindow{UsedPercent: pct(7)}}); got != nil {
			t.Fatalf("windows = %+v, want none", got)
		}
	})
	t.Run("a window without a percent is skipped", func(t *testing.T) {
		got := limitWindows(rateLimitSnapshot{Primary: &rateLimitWindow{}, Secondary: &rateLimitWindow{UsedPercent: pct(1)}})
		check(t, got, agent.LimitWindow{ID: "secondary", Kind: "weekly", Label: "Weekly", Percent: 1})
	})
}

const rateLimitsReadResult = `{"rateLimits":{"limitId":"spark","planType":"plus","primary":{"usedPercent":99,"resetsAt":1758000000,"windowDurationMins":300}},"rateLimitsByLimitId":{"codex":{"limitId":"codex","planType":"plus","primary":{"usedPercent":12,"resetsAt":1758000000,"windowDurationMins":300},"secondary":{"usedPercent":3,"resetsAt":1758500000,"windowDurationMins":10080}},"spark":{"limitId":"spark","planType":"plus","primary":{"usedPercent":99,"resetsAt":1758000000,"windowDurationMins":300}}},"rateLimitResetCredits":{"availableCount":0,"credits":[]}}`

func TestLimitsReadsTheMainBucket(t *testing.T) {
	a, f := newFake(t)
	type result struct {
		limits agent.Limits
		err    error
	}
	res := make(chan result, 1)
	go func() {
		l, err := a.Limits(context.Background())
		res <- result{l, err}
	}()
	f.handshake()
	m := f.expect("account/rateLimits/read")
	if len(m.Params) != 0 && string(m.Params) != "null" {
		t.Fatalf("account/rateLimits/read sent params %s, want none", m.Params)
	}
	f.reply(m.ID, rateLimitsReadResult)
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.limits.CheckedAt.IsZero() {
		t.Error("CheckedAt not set")
	}
	w := r.limits.Windows
	if len(w) != 2 || w[0].ID != "primary" || w[0].Percent != 12 || w[1].ID != "secondary" || w[1].Kind != "weekly" || w[1].Percent != 3 {
		t.Fatalf("windows = %+v", w)
	}
}

func TestLimitsWithoutWindowsIsAnError(t *testing.T) {
	a, f := newFake(t)
	errc := make(chan error, 1)
	go func() {
		_, err := a.Limits(context.Background())
		errc <- err
	}()
	f.handshake()
	m := f.expect("account/rateLimits/read")
	f.reply(m.ID, `{"rateLimits":{"limitId":"spark","primary":{"usedPercent":1}}}`)
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "no subscription windows") {
		t.Fatalf("err = %v", err)
	}
}

func TestRateLimitsUpdatedReachesEverySession(t *testing.T) {
	a, f, s1 := harness(t)
	res := startAsync(a, agent.Config{Cwd: "/work"})
	m := f.expect("thread/start")
	f.reply(m.ID, `{"thread":{"id":"thr_2","model":"gpt-5.1-codex"}}`)
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	s2 := r.sess
	expectKind(t, s2.Events(), agent.KindSessionInfo)

	// A model-specific bucket says nothing about the main rows.
	f.send(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"spark","planType":"plus","primary":{"usedPercent":99}}}}`)
	f.send(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","planType":"plus","primary":{"usedPercent":12,"resetsAt":1758000000,"windowDurationMins":300},"secondary":{"usedPercent":3,"resetsAt":1758500000,"windowDurationMins":10080}}}}`)
	for _, s := range []agent.Session{s1, s2} {
		ev := expectKind(t, s.Events(), agent.KindLimits)
		w := ev.Limits.Windows
		if len(w) != 2 || w[0].ID != "primary" || w[0].Percent != 12 || w[1].ID != "secondary" || w[1].Percent != 3 {
			t.Fatalf("limits = %+v", w)
		}
		if !w[0].ResetsAt.Equal(time.Unix(1758000000, 0)) {
			t.Fatalf("resets at = %v", w[0].ResetsAt)
		}
	}
}
