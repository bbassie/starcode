package web

import (
	"context"
	"encoding/json"
	"os"
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
	// inflight marks agents whose catalog is being fetched right now, so
	// concurrent pages share one process launch.
	inflight map[string]chan struct{}
	// path is the on-disk copy, so a restarted starcode serves the last
	// catalogs at once (stale, refreshing in the background) instead of
	// making the first page wait for two process launches.
	path string
}

type capsFile struct {
	Entries map[string]capsFileEntry `json:"entries"`
}

type capsFileEntry struct {
	Caps    agent.Capabilities `json:"caps"`
	Fetched time.Time          `json:"fetched"`
}

// load reads the on-disk catalogs. Anything older than a day is left out.
func (c *capsCache) load() {
	if c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var f capsFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	for name, e := range f.Entries {
		if time.Since(e.Fetched) < 24*time.Hour && len(e.Caps.Models) > 0 {
			c.entries[name] = capsEntry{caps: e.Caps, fetched: e.Fetched}
		}
	}
}

// save writes every good catalog. Called with c.mu held.
func (c *capsCache) save() {
	if c.path == "" {
		return
	}
	f := capsFile{Entries: map[string]capsFileEntry{}}
	for name, e := range c.entries {
		if e.err == nil && len(e.caps.Models) > 0 {
			f.Entries[name] = capsFileEntry{Caps: e.caps, Fetched: e.fetched}
		}
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		os.Rename(tmp, c.path)
	}
}

func (c *capsCache) init() {
	if c.entries == nil {
		c.entries = map[string]capsEntry{}
		c.inflight = map[string]chan struct{}{}
	}
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
	for _, name := range s.App.AgentNames() {
		ag, _ := s.App.Agent(name)
		d, ok := ag.(agent.Describer)
		if !ok {
			continue
		}
		e := s.capsFor(ctx, name, d)
		out[name] = s.pickerModels(name, e.caps)
		if e.err != nil {
			errs[name] = e.err
		}
	}
	return out, errs
}

func (s *Server) capsFor(ctx context.Context, name string, d agent.Describer) capsEntry {
	s.cache.mu.Lock()
	s.cache.init()
	e, ok := s.cache.entries[name]
	ttl := capsTTL
	if e.err != nil {
		ttl = capsErrorTTL
	}
	if ok && time.Since(e.fetched) < ttl {
		s.cache.mu.Unlock()
		return e
	}
	wait, fetching := s.cache.inflight[name]
	if !fetching {
		wait = make(chan struct{})
		s.cache.inflight[name] = wait
	}
	s.cache.mu.Unlock()
	// A stale catalog is still the right catalog nearly always, so pages
	// keep rendering with it while one fetch runs in the background. Only
	// a page with nothing to show waits for the launch.
	if ok && e.err == nil {
		if !fetching {
			go s.fetchCaps(name, d, wait)
		}
		return e
	}
	if !fetching {
		s.fetchCaps(name, d, wait)
	} else {
		<-wait
	}
	s.cache.mu.Lock()
	e = s.cache.entries[name]
	s.cache.mu.Unlock()
	return e
}

// fetchCaps asks the agent and stores the answer, then releases anyone
// waiting on done. It runs detached from any request: the result is shared
// by every page, and a browser navigating away mid-fetch must not poison
// the cache.
func (s *Server) fetchCaps(name string, d agent.Describer, done chan struct{}) {
	fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	caps, err := d.Capabilities(fetchCtx)
	if err != nil {
		s.Log.Warn("capabilities", "agent", name, "err", err)
	}
	s.cache.mu.Lock()
	s.cache.entries[name] = capsEntry{caps: caps, err: err, fetched: time.Now()}
	delete(s.cache.inflight, name)
	if err == nil {
		s.cache.save()
	}
	s.cache.mu.Unlock()
	close(done)
}
