package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ratesURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

type modelRate struct {
	input, output, cacheRead, cacheCreation float64
}

type rateTable map[string]modelRate

type ratesDocument struct {
	FetchedAtMS int64                      `json:"fetchedAtMs"`
	Document    map[string]json.RawMessage `json:"document"`
}

func (s *Scanner) loadRates(ctx context.Context) (rateTable, string) {
	now := time.Now()
	if len(s.rates) > 0 && now.Sub(s.ratesAt) < 24*time.Hour {
		return s.rates, s.status
	}
	if !s.ratesChecked.IsZero() && now.Sub(s.ratesChecked) < time.Hour {
		return s.rates, s.status
	}
	s.ratesChecked = now
	var cached ratesDocument
	paths := []string{}
	if s.cacheDir != "" {
		paths = append(paths, filepath.Join(s.cacheDir, "usage-model-rates.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".t3", "userdata", "usage-model-rates.json"))
	}
	for _, path := range paths {
		if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &cached) == nil {
			if parsed := parseRates(cached.Document); len(parsed) > 0 {
				s.rates, s.ratesAt, s.status = parsed, time.UnixMilli(cached.FetchedAtMS), "cached"
				if now.Sub(s.ratesAt) < 24*time.Hour {
					return s.rates, s.status
				}
				break
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ratesURL, nil)
	if err == nil {
		if response, fetchErr := s.client.Do(req); fetchErr == nil {
			defer response.Body.Close()
			if response.StatusCode == http.StatusOK {
				var document map[string]json.RawMessage
				if json.NewDecoder(response.Body).Decode(&document) == nil {
					if parsed := parseRates(document); len(parsed) > 0 {
						s.rates, s.ratesAt, s.status = parsed, now, "fresh"
						s.persistRates(ratesDocument{FetchedAtMS: now.UnixMilli(), Document: document})
						return s.rates, s.status
					}
				}
			}
		}
	}
	if len(s.rates) > 0 {
		s.status = "cached"
		return s.rates, s.status
	}
	return nil, "unavailable"
}

func (s *Scanner) persistRates(document ratesDocument) {
	if s.cacheDir == "" {
		return
	}
	raw, err := json.Marshal(document)
	if err != nil || os.MkdirAll(s.cacheDir, 0o755) != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.cacheDir, "usage-model-rates.json"), raw, 0o644)
}

func parseRates(document map[string]json.RawMessage) rateTable {
	out := make(rateTable)
	for name, raw := range document {
		var entry struct {
			Input         *float64 `json:"input_cost_per_token"`
			Output        *float64 `json:"output_cost_per_token"`
			CacheRead     *float64 `json:"cache_read_input_token_cost"`
			CacheCreation *float64 `json:"cache_creation_input_token_cost"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Input == nil || entry.Output == nil {
			continue
		}
		rate := modelRate{input: *entry.Input, output: *entry.Output, cacheRead: *entry.Input, cacheCreation: *entry.Input}
		if entry.CacheRead != nil {
			rate.cacheRead = *entry.CacheRead
		}
		if entry.CacheCreation != nil {
			rate.cacheCreation = *entry.CacheCreation
		}
		out[strings.ToLower(strings.TrimSpace(name))] = rate
	}
	return out
}

func lookupRate(rates rateTable, model string) (modelRate, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	bare := key
	if slash := strings.LastIndex(bare, "/"); slash >= 0 {
		bare = bare[slash+1:]
	}
	switch bare {
	case "", "synthetic", "<synthetic>", "opus", "sonnet", "haiku", "fable":
		return modelRate{}, false
	}
	if rate, ok := rates[key]; ok {
		return rate, true
	}
	rate, ok := rates[bare]
	return rate, ok
}

func price(rate modelRate, tokens Tokens) float64 {
	return float64(tokens.UncachedInput)*rate.input +
		float64(tokens.CachedInput)*rate.cacheRead +
		float64(tokens.CacheCreation)*rate.cacheCreation +
		float64(tokens.Output)*rate.output
}
