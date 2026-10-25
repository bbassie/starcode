package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"starcode/internal/agent"
	"starcode/internal/app"
	"starcode/internal/domain"
	"starcode/internal/web/views"

	"github.com/starfederation/datastar-go/datastar"
)

// Subscription limits: what each instance's account has used of its
// 5-hour and weekly windows, read from the CLI (agent.LimitReader) in the
// background and kept here per instance. A turn that streams a window
// (Claude's rate_limit_event, Codex's account/rateLimits/updated) updates
// the row through the bus, so the numbers move while agents work.

// limitsKeep is how long a probe's reading stands before a page that
// wants the numbers asks for a fresh one.
const limitsKeep = 10 * time.Minute

type limitEntry struct {
	limits agent.Limits
	err    error
	at     time.Time
}

type limitsCache struct {
	mu      sync.Mutex
	entries map[string]limitEntry
	probing map[string]bool
	// claims is the request id of a reset claim per instance and grant,
	// kept until Claude confirms it (see redeemReset).
	claims map[string]string
}

// init makes the maps; called under mu by every writer.
func (c *limitsCache) init() {
	if c.entries == nil {
		c.entries = map[string]limitEntry{}
		c.probing = map[string]bool{}
		c.claims = map[string]string{}
	}
}

// limitsFor is the cached reading for instance name. Stale or missing
// readings are probed in the background; the page redraws when the bus
// says so. force probes now, in the caller's goroutine.
func (s *Server) limitsFor(ctx context.Context, name string, force bool) limitEntry {
	s.limits.mu.Lock()
	s.limits.init()
	e, ok := s.limits.entries[name]
	fresh := ok && time.Since(e.at) < limitsKeep
	busy := s.limits.probing[name]
	if !force && !fresh && !busy {
		s.limits.probing[name] = true
	}
	s.limits.mu.Unlock()
	if force {
		return s.probeLimits(context.WithoutCancel(ctx), name)
	}
	if !fresh && !busy {
		go s.probeLimits(context.Background(), name)
	}
	return e
}

// probeLimits asks the instance's CLI and stores what it said; a failure
// keeps the last good windows so the bars do not vanish on one bad read.
func (s *Server) probeLimits(ctx context.Context, name string) limitEntry {
	in, ok := s.Providers.Get(name)
	var e limitEntry
	if !ok || !in.Enabled {
		e = limitEntry{err: errNoLimits, at: time.Now()}
	} else if ag, err := s.Providers.Build(in); err != nil {
		e = limitEntry{err: err, at: time.Now()}
	} else if lr, ok := ag.(agent.LimitReader); !ok {
		e = limitEntry{err: errNoLimits, at: time.Now()}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		l, err := lr.Limits(ctx)
		cancel()
		e = limitEntry{limits: l, err: err, at: time.Now()}
		if e.limits.CheckedAt.IsZero() {
			e.limits.CheckedAt = e.at
		}
	}
	s.limits.mu.Lock()
	s.limits.init()
	if old, had := s.limits.entries[name]; had && e.err != nil && len(old.limits.Windows) > 0 {
		e.limits = old.limits
	}
	changed := !sameLimits(s.limits.entries[name].limits, e.limits) || (s.limits.entries[name].err == nil) != (e.err == nil)
	s.limits.entries[name] = e
	delete(s.limits.probing, name)
	s.limits.mu.Unlock()
	if changed {
		s.App.Bus.Publish(domain.LimitsChanged{})
	}
	return e
}

var errNoLimits = errNoLimitsType{}

type errNoLimitsType struct{}

func (errNoLimitsType) Error() string { return "this agent does not report its limits" }

// mergeLimits folds a streamed update into the instance's row: windows
// upsert by id, the rest stay. A reset time the update lacks keeps the
// probe's.
func (s *Server) mergeLimits(name string, upd agent.Limits) {
	if len(upd.Windows) == 0 {
		return
	}
	s.limits.mu.Lock()
	s.limits.init()
	e := s.limits.entries[name]
	changed := false
	for _, w := range upd.Windows {
		found := false
		for i, old := range e.limits.Windows {
			if old.ID != w.ID {
				continue
			}
			found = true
			if w.ResetsAt.IsZero() {
				w.ResetsAt = old.ResetsAt
			}
			if old != w {
				e.limits.Windows[i] = w
				changed = true
			}
		}
		if !found {
			e.limits.Windows = append(e.limits.Windows, w)
			changed = true
		}
	}
	if changed {
		sortWindows(e.limits.Windows)
		e.limits.CheckedAt = time.Now()
		e.err = nil
		if e.at.IsZero() {
			e.at = time.Now()
		}
		s.limits.entries[name] = e
	}
	s.limits.mu.Unlock()
	if changed {
		s.App.Bus.Publish(domain.LimitsChanged{})
	}
}

var windowOrder = map[string]int{"session": 0, "weekly": 1, "monthly": 2}

func sortWindows(ws []agent.LimitWindow) {
	sort.SliceStable(ws, func(i, j int) bool {
		a, b := windowOrder[ws[i].Kind], windowOrder[ws[j].Kind]
		if _, ok := windowOrder[ws[i].Kind]; !ok {
			a = 3
		}
		if _, ok := windowOrder[ws[j].Kind]; !ok {
			b = 3
		}
		if a != b {
			return a < b
		}
		return ws[i].ID < ws[j].ID
	})
}

func sameLimits(a, b agent.Limits) bool {
	if len(a.Windows) != len(b.Windows) || (a.Resets == nil) != (b.Resets == nil) || a.Resets != nil && *a.Resets != *b.Resets {
		return false
	}
	for i := range a.Windows {
		if a.Windows[i] != b.Windows[i] {
			return false
		}
	}
	return true
}

// watchLimits merges the windows agents stream mid-turn into the cache.
func (s *Server) watchLimits(ctx context.Context) {
	ch := s.App.Bus.Subscribe(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if u, ok := msg.(app.LimitsUpdate); ok {
				s.mergeLimits(u.Agent, u.Limits)
			}
		}
	}
}

// probeAllLimits reads every enabled instance that can report; the
// providers watch calls it on its schedule.
func (s *Server) probeAllLimits(ctx context.Context) {
	for _, in := range s.Providers.Instances() {
		if in.Enabled {
			s.probeLimits(ctx, in.Name)
		}
	}
}

// limitsRows is the Usage page's Limits section: one row per enabled
// instance, probed in the background when stale.
func (s *Server) limitsRows(ctx context.Context) []views.LimitsRow {
	looks := s.agentLooks()
	var rows []views.LimitsRow
	for _, in := range s.Providers.Instances() {
		if !in.Enabled {
			continue
		}
		e := s.limitsFor(ctx, in.Name, false)
		row := views.LimitsRow{Agent: in.Name, Look: looks[in.Name], Windows: e.limits.Windows, CheckedAt: e.limits.CheckedAt, Pending: e.at.IsZero(), Resets: e.limits.Resets}
		if e.err != nil {
			row.Err = e.err.Error()
			if e.err == errNoLimits {
				row.Unsupported = true
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// agentLimits is the cached windows of one instance for the composer's
// context card; nothing is probed here, the card is drawn too often.
func (s *Server) agentLimits(name string) []agent.LimitWindow {
	s.limits.mu.Lock()
	defer s.limits.mu.Unlock()
	return s.limits.entries[name].limits.Windows
}

// refreshLimits is the section's refresh button: probe now, then the
// page redraws through the bus.
func (s *Server) refreshLimits(w http.ResponseWriter, r *http.Request) {
	for _, in := range s.Providers.Instances() {
		if in.Enabled {
			s.limitsFor(r.Context(), in.Name, true)
		}
	}
	s.App.Bus.Publish(domain.LimitsChanged{})
	s.ok(w, r)
}

// redeemReset spends one of instance {name}'s banked resets, then reads
// the limits again so the bars show the fresh windows. A claim Claude
// could not confirm keeps its request id, so pressing again asks about
// the same claim instead of making a second one.
func (s *Server) redeemReset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.limits.mu.Lock()
	s.limits.init()
	rc := s.limits.entries[name].limits.Resets
	s.limits.mu.Unlock()
	if rc == nil || rc.NextID == "" {
		s.fail(w, r, errors.New("no banked reset to use; refresh the limits"))
		return
	}
	in, ok := s.Providers.Get(name)
	if !ok {
		s.fail(w, r, errors.New("unknown agent"))
		return
	}
	ag, err := s.Providers.Build(in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rr, ok := ag.(agent.ResetRedeemer)
	if !ok {
		s.fail(w, r, errors.New("this agent has no resets to redeem"))
		return
	}
	key := name + "\x00" + rc.NextID
	s.limits.mu.Lock()
	reqID := s.limits.claims[key]
	if reqID == "" {
		reqID = randomID()
		s.limits.claims[key] = reqID
	}
	s.limits.mu.Unlock()
	outcome, err := rr.RedeemReset(r.Context(), rc.NextID, reqID)
	if err == nil {
		s.limits.mu.Lock()
		delete(s.limits.claims, key)
		s.limits.mu.Unlock()
	}
	s.limitsFor(r.Context(), name, true)
	s.App.Bus.Publish(domain.LimitsChanged{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	datastar.NewSSE(w, r).PatchElementTempl(views.NoteToast(outcome))
}

// randomID is a request id for a reset claim.
func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
