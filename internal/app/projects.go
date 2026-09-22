package app

import (
	"context"
	"fmt"
	"strings"

	"starcode/internal/domain"
	"starcode/internal/store"
)

// ProjectSettings is what a project sets for its new threads (see
// store.Project): whether they get a worktree, the agent, model, effort
// and permission mode they start with (Agent empty: none), and the
// worktree cleanup override ("" or "off").
type ProjectSettings struct {
	Worktrees                  bool
	Agent, Model, Effort, Mode string
	Cleanup                    string
}

func settingsOf(p store.Project) ProjectSettings {
	return ProjectSettings{Worktrees: p.Worktrees, Agent: p.Agent, Model: p.Model, Effort: p.Effort, Mode: p.Mode, Cleanup: p.Cleanup}
}

// SetProjectSettings replaces a project's settings. A newly named agent
// that is not registered is refused (one saved before, whose provider has
// since been turned off, stays); clearing the agent clears the rest of
// the defaults with it.
func (a *App) SetProjectSettings(ctx context.Context, id string, ps ProjectSettings) error {
	p, err := a.Store.Project(ctx, id)
	if err != nil {
		return err
	}
	ps.Agent, ps.Model, ps.Effort, ps.Mode = strings.TrimSpace(ps.Agent), strings.TrimSpace(ps.Model), strings.TrimSpace(ps.Effort), strings.TrimSpace(ps.Mode)
	if ps.Agent == "" {
		ps.Model, ps.Effort, ps.Mode = "", "", ""
	} else if _, ok := a.Agent(ps.Agent); !ok && ps.Agent != p.Agent {
		return fmt.Errorf("unknown agent %q", ps.Agent)
	}
	switch ps.Cleanup {
	case "", "off":
	default:
		return fmt.Errorf("unknown cleanup setting %q", ps.Cleanup)
	}
	if settingsOf(p) == ps {
		return nil
	}
	_, err = a.Store.Append(ctx, "", domain.ProjectSettingsChanged{ID: id, Worktrees: ps.Worktrees, Agent: ps.Agent, Model: ps.Model, Effort: ps.Effort, Mode: ps.Mode, Cleanup: ps.Cleanup})
	return err
}
