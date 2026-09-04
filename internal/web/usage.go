package web

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"starcode/internal/web/views"
)

func usageParams(r *http.Request) (int, string) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days != 1 && days != 7 && days != 90 {
		days = 30
	}
	metric := r.URL.Query().Get("metric")
	if metric != "tokens" {
		metric = "cost"
	}
	return days, metric
}

type usageAccumulator struct {
	group   views.UsageGroup
	threads map[string]bool
}

func (s *Server) usageData(ctx context.Context, days int, metric string) (views.UsageData, error) {
	now := time.Now().UTC()
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start := end.AddDate(0, 0, -(days - 1))
	pointCount := days
	if days == 1 {
		end = now.Truncate(time.Hour)
		start = end.Add(-23 * time.Hour)
		pointCount = 24
	}
	entries, err := s.App.Store.Usage(ctx, start)
	if err != nil {
		return views.UsageData{}, err
	}
	d := views.UsageData{Days: days, Metric: metric, Start: start, End: end, Daily: make([]views.UsagePoint, pointCount)}
	for i := range d.Daily {
		if days == 1 {
			d.Daily[i].Date = start.Add(time.Duration(i) * time.Hour)
		} else {
			d.Daily[i].Date = start.AddDate(0, 0, i)
		}
	}
	agents := make(map[string]*usageAccumulator)
	models := make(map[string]*usageAccumulator)
	threads := make(map[string]bool)
	for _, entry := range entries {
		bucket := time.Date(entry.CreatedAt.UTC().Year(), entry.CreatedAt.UTC().Month(), entry.CreatedAt.UTC().Day(), 0, 0, 0, 0, time.UTC)
		divisor := 24.0
		if days == 1 {
			bucket = entry.CreatedAt.UTC().Truncate(time.Hour)
			divisor = 1
		}
		index := int(bucket.Sub(start).Hours() / divisor)
		if index < 0 || index >= len(d.Daily) {
			continue
		}
		d.Turns++
		d.CostUSD += entry.CostUSD
		d.InputTokens += entry.InputTokens
		d.OutputTokens += entry.OutputTokens
		threads[entry.ThreadID] = true
		d.Daily[index].CostUSD += entry.CostUSD
		d.Daily[index].InputTokens += entry.InputTokens
		d.Daily[index].OutputTokens += entry.OutputTokens

		agent := entry.Agent
		if agent == "" {
			agent = "unknown"
		}
		addUsage(agents, agent, agentName(agent), agent, entry.ThreadID, entry.CostUSD, entry.InputTokens, entry.OutputTokens)
		model := entry.Model
		if model == "" {
			model = "default"
		}
		addUsage(models, agent+"\x00"+model, model, agent, entry.ThreadID, entry.CostUSD, entry.InputTokens, entry.OutputTokens)
	}
	d.Threads = len(threads)
	d.Agents = finishUsageGroups(agents, metric)
	d.Models = finishUsageGroups(models, metric)
	return d, nil
}

func addUsage(groups map[string]*usageAccumulator, key, name, agent, threadID string, cost float64, input, output int64) {
	g := groups[key]
	if g == nil {
		g = &usageAccumulator{group: views.UsageGroup{Name: name, Agent: agent}, threads: make(map[string]bool)}
		groups[key] = g
	}
	g.group.Turns++
	g.group.CostUSD += cost
	g.group.InputTokens += input
	g.group.OutputTokens += output
	g.threads[threadID] = true
}

func finishUsageGroups(groups map[string]*usageAccumulator, metric string) []views.UsageGroup {
	out := make([]views.UsageGroup, 0, len(groups))
	for _, item := range groups {
		item.group.Threads = len(item.threads)
		out = append(out, item.group)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := out[i].CostUSD, out[j].CostUSD
		if metric == "tokens" {
			left = float64(out[i].InputTokens + out[i].OutputTokens)
			right = float64(out[j].InputTokens + out[j].OutputTokens)
		}
		if left == right {
			return out[i].Name < out[j].Name
		}
		return left > right
	})
	return out
}

func agentName(agent string) string {
	switch agent {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "fake":
		return "Fake agent"
	default:
		if agent == "" {
			return "Unknown"
		}
		return strings.ToUpper(agent[:1]) + agent[1:]
	}
}
