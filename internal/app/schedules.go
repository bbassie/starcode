package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"starcode/internal/cron"
	"starcode/internal/domain"
	"starcode/internal/store"
)

// Schedules are prompts sent later: once at a time ("send later"), or
// on a cron. A schedule on a thread sends its prompt there (queued when a
// turn runs); one on a project opens a new thread for every run. The
// minute sweep (reapLoop) runs what is due. A run missed while starcode
// was down happens at the first sweep after it comes back, once.

// SetSchedule adds a schedule, or replaces the one with sc.ID. A cron
// schedule without At gets its next run worked out; a one-off needs At in
// the future. It returns the schedule's id.
func (a *App) SetSchedule(ctx context.Context, sc domain.ScheduleSet) (string, error) {
	sc.Prompt = strings.TrimSpace(sc.Prompt)
	if sc.Prompt == "" {
		return "", errors.New("prompt is empty")
	}
	if sc.ThreadID != "" {
		t, err := a.Store.Thread(ctx, sc.ThreadID)
		if err != nil {
			return "", err
		}
		sc.ProjectID = t.ProjectID
	} else if _, err := a.Store.Project(ctx, sc.ProjectID); err != nil {
		return "", fmt.Errorf("project: %w", err)
	}
	if sc.Agent != "" {
		if _, ok := a.Agent(sc.Agent); !ok {
			return "", fmt.Errorf("unknown agent %q", sc.Agent)
		}
	}
	sc.Cron = strings.TrimSpace(sc.Cron)
	if sc.Cron != "" {
		next, err := nextRun(sc.Cron, sc.TZ, time.Now())
		if err != nil {
			return "", err
		}
		if sc.At.IsZero() {
			sc.At = next
		}
	} else if !sc.At.After(time.Now()) {
		return "", errors.New("pick a time in the future")
	}
	if sc.ID == "" {
		sc.ID = newID()
	}
	_, err := a.Store.Append(ctx, sc.ThreadID, sc)
	return sc.ID, err
}

// nextRun is when expr, read in zone tz, fires after now.
func nextRun(expr, tz string, now time.Time) (time.Time, error) {
	s, err := cron.Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	loc := time.Local
	if tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return time.Time{}, fmt.Errorf("time zone %q: %w", tz, err)
		}
	}
	next := s.Next(now, loc)
	if next.IsZero() {
		return next, fmt.Errorf("cron %q never fires", expr)
	}
	return next, nil
}

// RemoveSchedule deletes a schedule.
func (a *App) RemoveSchedule(ctx context.Context, id string) error {
	sc, err := a.Store.Schedule(ctx, id)
	if err != nil {
		return err
	}
	_, err = a.Store.Append(ctx, sc.ThreadID, domain.ScheduleRemoved{ID: id})
	return err
}

// PauseSchedule stops a schedule from running, or lets it run again; a
// cron schedule resumed skips the runs it missed.
func (a *App) PauseSchedule(ctx context.Context, id string, paused bool) error {
	sc, err := a.Store.Schedule(ctx, id)
	if err != nil {
		return err
	}
	if sc.Paused == paused {
		return nil
	}
	set := scheduleEvent(sc)
	set.Paused = paused
	if !paused && sc.Cron != "" && sc.Next.Before(time.Now()) {
		if set.At, err = nextRun(sc.Cron, sc.TZ, time.Now()); err != nil {
			return err
		}
	}
	_, err = a.Store.Append(ctx, sc.ThreadID, set)
	return err
}

// RunSchedule runs a schedule now, out of turn; a cron schedule keeps its
// next run, a one-off is done.
func (a *App) RunSchedule(ctx context.Context, id string) error {
	sc, err := a.Store.Schedule(ctx, id)
	if err != nil {
		return err
	}
	next := time.Time{}
	if sc.Cron != "" {
		next = sc.Next
	}
	return a.runSchedule(ctx, sc, next)
}

func scheduleEvent(sc store.Schedule) domain.ScheduleSet {
	return domain.ScheduleSet{ID: sc.ID, ThreadID: sc.ThreadID, ProjectID: sc.ProjectID, Prompt: sc.Prompt, Title: sc.Title, At: sc.Next, Cron: sc.Cron, TZ: sc.TZ,
		Agent: sc.Agent, Model: sc.Model, Effort: sc.Effort, Mode: sc.Mode, Paused: sc.Paused, By: sc.By}
}

// runSchedules runs every schedule due by now.
func (a *App) runSchedules(ctx context.Context, now time.Time) {
	scs, err := a.Store.Schedules(ctx, "")
	if err != nil {
		a.Log.Warn("read schedules", "err", err)
		return
	}
	for _, sc := range scs {
		if sc.Paused || sc.Next.After(now) {
			continue
		}
		next := time.Time{}
		if sc.Cron != "" {
			if next, err = nextRun(sc.Cron, sc.TZ, now); err != nil {
				a.Log.Warn("schedule", "id", sc.ID, "err", err)
			}
		}
		if err := a.runSchedule(ctx, sc, next); err != nil {
			a.Log.Warn("run schedule", "id", sc.ID, "err", err)
		}
	}
}

// runSchedule sends sc's prompt and records the run with next as the
// next one (zero ends it).
func (a *App) runSchedule(ctx context.Context, sc store.Schedule, next time.Time) error {
	ran := domain.ScheduleRan{ID: sc.ID, Next: next}
	var err error
	if sc.ThreadID != "" {
		ran.Thread = sc.ThreadID
		err = a.AutoPrompt(ctx, sc.ThreadID, sc.Prompt, "schedule")
	} else {
		ran.Thread, err = a.scheduledThread(ctx, sc)
	}
	if err != nil {
		ran.Error = err.Error()
		if sc.ThreadID != "" {
			a.warn(ctx, sc.ThreadID, "A scheduled prompt could not be sent: "+err.Error())
		}
	}
	if _, aerr := a.Store.Append(ctx, sc.ThreadID, ran); aerr != nil {
		return aerr
	}
	return err
}

// scheduledThread opens the thread a project schedule's run goes to and
// sends it the prompt.
func (a *App) scheduledThread(ctx context.Context, sc store.Schedule) (string, error) {
	agentName, model, effort, mode := sc.Agent, sc.Model, sc.Effort, sc.Mode
	if agentName == "" {
		if p, err := a.Store.Project(ctx, sc.ProjectID); err == nil && p.HasDefaults() {
			agentName, model, effort, mode = p.Agent, p.Model, p.Effort, p.Mode
		} else if names := a.AgentNames(); len(names) > 0 {
			agentName = names[0]
		}
	}
	id, err := a.CreateThread(ctx, sc.ProjectID, agentName, model)
	if err != nil {
		return "", err
	}
	if effort != "" || mode != "" {
		if err := a.SetThreadSettings(ctx, id, ThreadSettings{Agent: agentName, Model: model, Effort: effort, PermissionMode: mode}); err != nil {
			return id, err
		}
	}
	if sc.Title != "" {
		title := sc.Title + " · " + time.Now().Format("Jan 2")
		if err := a.RenameThread(ctx, id, title); err != nil {
			return id, err
		}
	}
	return id, a.SendPrompt(ctx, id, sc.Prompt)
}
