package web

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	usagex "starcode/internal/usage"
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

type usageRecord struct {
	sessionID, agent, model          string
	createdAt                        time.Time
	costUSD, cacheSavingsUSD         float64
	input, cached, cacheCreated, out int64
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
	stored, err := s.App.Store.Usage(ctx, start)
	if err != nil {
		return views.UsageData{}, err
	}
	var entries []usageRecord
	seenSessions := make(map[string]bool)
	pricingStatus := "unavailable"
	unpriced := 0
	if s.Usage != nil {
		system, scanErr := s.Usage.Read(ctx, start)
		if scanErr != nil {
			s.Log.Warn("scan system usage", "err", scanErr)
		} else {
			pricingStatus = system.PricingStatus
			unpriced = system.Unpriced
			for _, entry := range system.Entries {
				sessionID := entry.SessionID
				if sessionID == "" {
					sessionID = entry.Agent + ":" + entry.CreatedAt.Format(time.RFC3339Nano)
				}
				entries = append(entries, usageRecord{
					sessionID: sessionID, agent: entry.Agent, model: entry.Model, createdAt: entry.CreatedAt,
					costUSD: entry.CostUSD, cacheSavingsUSD: entry.CacheSavings,
					input: entry.Tokens.UncachedInput, cached: entry.Tokens.CachedInput, cacheCreated: entry.Tokens.CacheCreation, out: entry.Tokens.Output,
				})
				if entry.SessionID != "" {
					seenSessions[entry.SessionID] = true
				}
			}
		}
	}
	// The provider transcripts are authoritative. Keep Starcode events only
	// when their session transcript is absent (or for agents without one).
	// Session ids are UUIDs, so the match ignores which instance scanned
	// the transcript: two instances may share a config dir.
	for _, entry := range stored {
		if entry.SessionID != "" && seenSessions[entry.SessionID] {
			continue
		}
		sessionID := entry.SessionID
		if sessionID == "" {
			sessionID = entry.ThreadID
		}
		entries = append(entries, usageRecord{sessionID: sessionID, agent: entry.Agent, model: entry.Model, createdAt: entry.CreatedAt, costUSD: entry.CostUSD, input: entry.InputTokens, out: entry.OutputTokens})
	}
	d := views.UsageData{Days: days, Metric: metric, Start: start, End: end, PricingStatus: pricingStatus, UnpricedRecords: unpriced, Daily: make([]views.UsagePoint, pointCount)}
	for i := range d.Daily {
		if days == 1 {
			d.Daily[i].Date = start.Add(time.Duration(i) * time.Hour)
		} else {
			d.Daily[i].Date = start.AddDate(0, 0, i)
		}
	}
	agents := make(map[string]*usageAccumulator)
	models := make(map[string]*usageAccumulator)
	series := make(map[string][]views.UsagePoint)
	threads := make(map[string]bool)
	for _, entry := range entries {
		bucket := time.Date(entry.createdAt.UTC().Year(), entry.createdAt.UTC().Month(), entry.createdAt.UTC().Day(), 0, 0, 0, 0, time.UTC)
		divisor := 24.0
		if days == 1 {
			bucket = entry.createdAt.UTC().Truncate(time.Hour)
			divisor = 1
		}
		index := int(bucket.Sub(start).Hours() / divisor)
		if index < 0 || index >= len(d.Daily) {
			continue
		}
		d.Turns++
		d.CostUSD += entry.costUSD
		d.CacheSavingsUSD += entry.cacheSavingsUSD
		d.InputTokens += entry.input
		d.CachedInputTokens += entry.cached
		d.CacheCreationTokens += entry.cacheCreated
		d.OutputTokens += entry.out
		threads[entry.sessionID] = true
		d.Daily[index].CostUSD += entry.costUSD
		d.Daily[index].InputTokens += entry.input
		d.Daily[index].CachedInputTokens += entry.cached
		d.Daily[index].CacheCreationTokens += entry.cacheCreated
		d.Daily[index].OutputTokens += entry.out

		agent := entry.agent
		if agent == "" {
			agent = "unknown"
		}
		if series[agent] == nil {
			series[agent] = make([]views.UsagePoint, len(d.Daily))
			for i := range d.Daily {
				series[agent][i].Date = d.Daily[i].Date
			}
		}
		series[agent][index].CostUSD += entry.costUSD
		series[agent][index].InputTokens += entry.input
		series[agent][index].CachedInputTokens += entry.cached
		series[agent][index].CacheCreationTokens += entry.cacheCreated
		series[agent][index].OutputTokens += entry.out
		addUsage(agents, agent, s.agentLabel(agent), agent, entry.sessionID, entry.costUSD, entry.input, entry.cached, entry.cacheCreated, entry.out)
		model := entry.model
		if model == "" {
			model = "default"
		}
		addUsage(models, agent+"\x00"+model, model, agent, entry.sessionID, entry.costUSD, entry.input, entry.cached, entry.cacheCreated, entry.out)
	}
	d.Threads = len(threads)
	d.Agents = finishUsageGroups(agents, metric)
	for i := range d.Agents {
		d.Agents[i].Driver, d.Agents[i].Color = s.agentLook(d.Agents[i].Agent)
		d.Series = append(d.Series, views.UsageSeries{Name: d.Agents[i].Name, Agent: d.Agents[i].Agent, Driver: d.Agents[i].Driver, Color: d.Agents[i].Color, Points: series[d.Agents[i].Agent]})
	}
	d.Models = finishUsageGroups(models, metric)
	for i := range d.Models {
		d.Models[i].Driver, d.Models[i].Color = s.agentLook(d.Models[i].Agent)
	}
	return d, nil
}

// usageRoots lists every instance's transcript directory for the scanner.
func (s *Server) usageRoots() []usagex.Root {
	var roots []usagex.Root
	for _, in := range s.Providers.Instances() {
		if dir := usagex.TranscriptDir(in.Driver, in.ConfigDir); dir != "" {
			roots = append(roots, usagex.Root{Agent: in.Name, Driver: in.Driver, Dir: dir})
		}
	}
	return roots
}

// agentLabel names an instance on the dashboard; usage from a name that
// no longer has an instance keeps the name.
func (s *Server) agentLabel(agent string) string {
	if in, ok := s.Providers.Get(agent); ok {
		return in.DisplayName()
	}
	return agentName(agent)
}

// agentLook is the driver (for the default colours) and the instance's own
// tag colour, if any.
func (s *Server) agentLook(agent string) (driver, color string) {
	if in, ok := s.Providers.Get(agent); ok {
		return in.Driver, in.Color
	}
	return agent, ""
}

func addUsage(groups map[string]*usageAccumulator, key, name, agent, sessionID string, cost float64, input, cached, cacheCreated, output int64) {
	g := groups[key]
	if g == nil {
		g = &usageAccumulator{group: views.UsageGroup{Name: name, Agent: agent}, threads: make(map[string]bool)}
		groups[key] = g
	}
	g.group.Turns++
	g.group.CostUSD += cost
	g.group.InputTokens += input
	g.group.CachedInputTokens += cached
	g.group.CacheCreationTokens += cacheCreated
	g.group.OutputTokens += output
	g.threads[sessionID] = true
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
			left = float64(out[i].InputTokens + out[i].CachedInputTokens + out[i].CacheCreationTokens + out[i].OutputTokens)
			right = float64(out[j].InputTokens + out[j].CachedInputTokens + out[j].CacheCreationTokens + out[j].OutputTokens)
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
