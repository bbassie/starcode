package web

import (
	"net/http/httptest"
	"testing"

	"starcode/internal/web/views"
)

func TestUsageParams(t *testing.T) {
	tests := []struct {
		query  string
		days   int
		metric string
	}{
		{"", 30, "cost"},
		{"?days=1&metric=tokens", 1, "tokens"},
		{"?days=7&metric=tokens", 7, "tokens"},
		{"?days=90&metric=cost", 90, "cost"},
		{"?days=2&metric=other", 30, "cost"},
	}
	for _, test := range tests {
		r := httptest.NewRequest("GET", "/settings/usage"+test.query, nil)
		days, metric := usageParams(r)
		if days != test.days || metric != test.metric {
			t.Errorf("usageParams(%q) = %d, %q; want %d, %q", test.query, days, metric, test.days, test.metric)
		}
	}
}

func TestFinishUsageGroupsSortsBySelectedMetric(t *testing.T) {
	groups := map[string]*usageAccumulator{
		"cost":   {group: views.UsageGroup{Name: "cost", CostUSD: 2, InputTokens: 1}, threads: map[string]bool{"a": true}},
		"tokens": {group: views.UsageGroup{Name: "tokens", CostUSD: 1, InputTokens: 100}, threads: map[string]bool{"a": true, "b": true}},
	}
	byCost := finishUsageGroups(groups, "cost")
	if byCost[0].Name != "cost" || byCost[0].Threads != 1 {
		t.Fatalf("cost sort = %+v", byCost)
	}
	byTokens := finishUsageGroups(groups, "tokens")
	if byTokens[0].Name != "tokens" || byTokens[0].Threads != 2 {
		t.Fatalf("token sort = %+v", byTokens)
	}
}
