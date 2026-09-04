package web

import (
	"context"
	"sync"
	"time"

	"starcode/internal/agent"
)

// capsCache remembers each agent's Capabilities for a while. Both real
// agents answer with a live process call (a CLI launch for Claude, an
// app-server request for Codex), so pages should not pay for that on every
// render.
type capsCache struct {
	mu      sync.Mutex
	entries map[string]capsEntry
}

type capsEntry struct {
	caps    agent.Capabilities
	err     error
	fetched time.Time
}

const (
	capsTTL      = 10 * time.Minute
	capsErrorTTL = 30 * time.Second
)

// warm fetches every catalog once in the background so the first page does
// not wait for two process launches.
func (s *Server) warm() {
	go s.capabilities(context.Background())
}

// capabilities returns each agent's Capabilities by name. A failing lookup
// yields empty capabilities and the error is kept for display.
func (s *Server) capabilities(ctx context.Context) (map[string]agent.Capabilities, map[string]error) {
	out := map[string]agent.Capabilities{}
	errs := map[string]error{}
	for name, ag := range s.App.Agents {
		d, ok := ag.(agent.Describer)
		if !ok {
			continue
		}
		e := s.capsFor(ctx, name, d)
		out[name] = e.caps
		if e.err != nil {
			errs[name] = e.err
		}
	}
	return out, errs
}

func (s *Server) capsFor(ctx context.Context, name string, d agent.Describer) capsEntry {
	s.cache.mu.Lock()
	if s.cache.entries == nil {
		s.cache.entries = map[string]capsEntry{}
	}
	e, ok := s.cache.entries[name]
	s.cache.mu.Unlock()
	ttl := capsTTL
	if e.err != nil {
		ttl = capsErrorTTL
	}
	if ok && time.Since(e.fetched) < ttl {
		return e
	}
	// Detached from the caller: the result is shared by every page, and a
	// browser navigating away mid-fetch must not poison the cache.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	caps, err := d.Capabilities(fetchCtx)
	if err != nil {
		s.Log.Warn("capabilities", "agent", name, "err", err)
	}
	e = capsEntry{caps: caps, err: err, fetched: time.Now()}
	s.cache.mu.Lock()
	s.cache.entries[name] = e
	s.cache.mu.Unlock()
	return e
}
