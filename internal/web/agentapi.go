package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"starcode/internal/app"
	"starcode/internal/domain"
	"starcode/internal/gitx"
	"starcode/internal/mcp"
	"starcode/internal/store"
	"starcode/internal/web/views"
)

// The agent API is the other end of `starcode mcp` (internal/mcp): the
// starcode tools an agent calls, served on a Unix socket in the data dir
// rather than on the HTTP port, so the agents need neither the token nor
// the TLS certificate. Every call names the calling thread and proves it
// with the key the app put in the agent's environment.

// AgentKey is the key the agent of threadID gets. The secret is new on
// every start; the agents are restarted with the server anyway.
func (s *Server) AgentKey(threadID string) string {
	return mcp.Sign(s.agentSecret(), threadID)
}

func (s *Server) agentSecret() []byte {
	s.agentOnce.Do(func() {
		s.agentKey = make([]byte, 32)
		rand.Read(s.agentKey)
	})
	return s.agentKey
}

// AgentHandler serves the agent API.
func (s *Server) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.agentCaller(w, r); !ok {
			return
		}
		ts := make([]mcp.Tool, len(agentTools))
		for i, t := range agentTools {
			ts[i] = t.Tool
		}
		writeJSON(w, ts)
	})
	mux.HandleFunc("POST /call", func(w http.ResponseWriter, r *http.Request) {
		caller, ok := s.agentCaller(w, r)
		if !ok {
			return
		}
		var call mcp.Call
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&call); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.runAgentTool(r.Context(), caller, call))
	})
	return mux
}

// agentCaller is the thread a request comes from, after its key checks out.
func (s *Server) agentCaller(w http.ResponseWriter, r *http.Request) (store.Thread, bool) {
	id := r.Header.Get(mcp.HeaderThread)
	if !mcp.Verify(s.agentSecret(), id, r.Header.Get(mcp.HeaderKey)) {
		http.Error(w, "bad agent key", http.StatusForbidden)
		return store.Thread{}, false
	}
	t, err := s.App.Store.Thread(r.Context(), id)
	if err != nil {
		http.Error(w, "unknown thread", http.StatusNotFound)
		return store.Thread{}, false
	}
	return t, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) runAgentTool(ctx context.Context, caller store.Thread, call mcp.Call) mcp.Result {
	for _, t := range agentTools {
		if t.Name != call.Name {
			continue
		}
		args := call.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		text, err := t.run(s, ctx, caller, args)
		if err != nil {
			s.Log.Debug("agent tool failed", "tool", call.Name, "thread", caller.ID, "err", err)
			return mcp.Result{Text: err.Error(), IsError: true}
		}
		return mcp.Result{Text: text}
	}
	return mcp.Result{Text: "no such tool: " + call.Name, IsError: true}
}

type agentTool struct {
	mcp.Tool
	run func(s *Server, ctx context.Context, caller store.Thread, args json.RawMessage) (string, error)
}

// schema builds an object schema from property name to (type,
// description); required names the ones that must be there.
func schema(props map[string][2]string, required ...string) map[string]any {
	ps := map[string]any{}
	for name, p := range props {
		ps[name] = map[string]any{"type": p[0], "description": p[1]}
	}
	out := map[string]any{"type": "object", "properties": ps}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func tool(name, desc string, sch map[string]any, run func(s *Server, ctx context.Context, caller store.Thread, args json.RawMessage) (string, error)) agentTool {
	if sch == nil {
		sch = schema(nil)
	}
	return agentTool{Tool: mcp.Tool{Name: name, Description: desc, InputSchema: sch}, run: run}
}

// decode reads a tool's arguments.
func decode[T any](args json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(args, &v); err != nil {
		return v, fmt.Errorf("bad arguments: %w", err)
	}
	return v, nil
}

var agentTools []agentTool

func init() {
	agentTools = []agentTool{
		tool("thread_info", "Where your own thread stands in starcode: id, title, project, linked pull request and whether it is watched, snooze, the thread that spawned you, and the threads you spawned.", nil,
			func(s *Server, ctx context.Context, t store.Thread, _ json.RawMessage) (string, error) {
				return s.describeThread(ctx, t, true), nil
			}),
		tool("set_title", "Rename your thread in the sidebar. Use a short title (under 60 characters) that says what the work is.",
			schema(map[string][2]string{"title": {"string", "the new title"}}, "title"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct{ Title string }](args)
				if err != nil {
					return "", err
				}
				if err := s.App.RenameThread(ctx, t.ID, clipTitle(a.Title)); err != nil {
					return "", err
				}
				return "Renamed.", nil
			}),
		tool("link_pr", "Link a pull request to your thread, so the user sees its state and checks next to the thread. A PR you open with `gh pr create` is linked on its own; use this for one you found or one opened another way.",
			schema(map[string][2]string{"pr": {"string", "the pull request: a github.com URL, owner/repo#123, or #123 / 123 for the project's repository"}}, "pr"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct{ PR string }](args)
				if err != nil {
					return "", err
				}
				repo, n, err := s.resolvePR(ctx, t, a.PR)
				if err != nil {
					return "", err
				}
				if err := s.App.LinkPR(ctx, t.ID, repo, n, ""); err != nil {
					return "", err
				}
				return fmt.Sprintf("Linked %s#%d.", repo, n), nil
			}),
		tool("unlink_pr", "Remove the pull request linked to your thread.", nil,
			func(s *Server, ctx context.Context, t store.Thread, _ json.RawMessage) (string, error) {
				if err := s.App.UnlinkPR(ctx, t.ID); err != nil {
					return "", err
				}
				return "Unlinked.", nil
			}),
		tool("watch_pr", "Watch the linked pull request: starcode sends you a prompt when a check fails, all checks pass, someone else comments or reviews, or the branch conflicts. The watch ends when the PR is merged or closed. Pass on=false to stop.",
			schema(map[string][2]string{"on": {"boolean", "true to watch (the default), false to stop"}}),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct{ On *bool }](args)
				if err != nil {
					return "", err
				}
				if a.On != nil && !*a.On {
					if err := s.App.StopPRWatch(ctx, t.ID, "the agent stopped watching"); err != nil {
						return "", err
					}
					return "Stopped watching.", nil
				}
				if err := s.App.StartPRWatch(ctx, t.ID); err != nil {
					return "", err
				}
				return "Watching the pull request. Finish your turn; starcode will prompt you when something happens.", nil
			}),
		tool("snooze", "Put your thread aside until later: it leaves the user's list and comes back at the time given. With wake_prompt, you get that prompt then, which is how to check on something again later (a deploy, a slow CI run). Finish your turn after calling it.",
			schema(map[string][2]string{
				"minutes":     {"integer", "how long from now, in minutes"},
				"until":       {"string", "or an RFC 3339 time to wake at"},
				"wake_prompt": {"string", "the prompt you get when the snooze ends; leave out to just come back to the user's list"},
			}),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct {
					Minutes    int
					Until      string
					WakePrompt string `json:"wake_prompt"`
				}](args)
				if err != nil {
					return "", err
				}
				var at time.Time
				switch {
				case a.Until != "":
					if at, err = time.Parse(time.RFC3339, a.Until); err != nil {
						return "", fmt.Errorf("until: %w", err)
					}
				case a.Minutes > 0:
					at = time.Now().Add(time.Duration(a.Minutes) * time.Minute)
				default:
					return "", errors.New("give minutes or until")
				}
				if err := s.App.SnoozeWith(ctx, t.ID, at, a.WakePrompt); err != nil {
					return "", err
				}
				return "Snoozed until " + at.Format("Mon 15:04 MST") + ".", nil
			}),
		tool("notify", "Get the user's attention: the message shows in your thread and marks it unread, which sends a notification to their devices. For when you need them and they may not be watching; your normal replies need no notify.",
			schema(map[string][2]string{"message": {"string", "what the user should know, one or two sentences"}}, "message"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct{ Message string }](args)
				if err != nil {
					return "", err
				}
				if strings.TrimSpace(a.Message) == "" {
					return "", errors.New("message is empty")
				}
				s.App.Notify(ctx, t.ID, a.Message)
				return "Sent.", nil
			}),
		tool("schedule", "Schedule a prompt for later. Without cron it goes out once, to your own thread, at the time given (like snooze with wake_prompt, but the thread stays in the user's list). With cron (five fields: minute hour day month weekday, in the user's time zone) it repeats; with new_thread every run opens a new thread in your project instead, running your agent and permission mode.",
			schema(map[string][2]string{
				"prompt":     {"string", "what to send"},
				"minutes":    {"integer", "send once, this many minutes from now"},
				"at":         {"string", "send once, at this RFC 3339 time"},
				"cron":       {"string", "repeat on this cron, e.g. 0 9 * * 1-5 for 9:00 on weekdays"},
				"new_thread": {"boolean", "with cron: open a new thread for every run (default false: send to your thread)"},
				"title":      {"string", "with new_thread: the title of the threads it opens"},
			}, "prompt"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct {
					Prompt, At, Cron, Title string
					Minutes                 int
					NewThread               bool `json:"new_thread"`
				}](args)
				if err != nil {
					return "", err
				}
				sc := domain.ScheduleSet{ThreadID: t.ID, Prompt: a.Prompt, Cron: a.Cron, TZ: s.userZone(ctx), By: t.ID}
				switch {
				case a.Cron != "":
					if a.NewThread {
						sc.ThreadID, sc.ProjectID, sc.Title = "", t.ProjectID, clipTitle(a.Title)
						sc.Agent, sc.Model, sc.Effort, sc.Mode = t.Agent, t.Model, t.Effort, t.PermissionMode
					}
				case a.At != "":
					if sc.At, err = time.Parse(time.RFC3339, a.At); err != nil {
						return "", fmt.Errorf("at: %w", err)
					}
				case a.Minutes > 0:
					sc.At = time.Now().Add(time.Duration(a.Minutes) * time.Minute)
				default:
					return "", errors.New("give minutes, at or cron")
				}
				id, err := s.App.SetSchedule(ctx, sc)
				if err != nil {
					return "", err
				}
				got, _ := s.App.Store.Schedule(ctx, id)
				return fmt.Sprintf("Scheduled %s, first at %s.", id, got.Next.In(s.userLoc(ctx)).Format("Mon Jan 2 15:04 MST")), nil
			}),
		tool("list_schedules", "List the schedules on your thread and the ones you made.", nil,
			func(s *Server, ctx context.Context, t store.Thread, _ json.RawMessage) (string, error) {
				scs, err := s.App.Store.Schedules(ctx, "")
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, sc := range scs {
					if sc.ThreadID != t.ID && sc.By != t.ID {
						continue
					}
					when := sc.Next.In(s.userLoc(ctx)).Format("Mon Jan 2 15:04 MST")
					if sc.Paused {
						when = "paused"
					}
					fmt.Fprintf(&b, "%s  %s", sc.ID, when)
					if sc.Cron != "" {
						fmt.Fprintf(&b, "  cron %q", sc.Cron)
					}
					if sc.ThreadID == "" {
						b.WriteString("  (new thread each run)")
					}
					fmt.Fprintf(&b, "\n  %s\n", clipTitle(sc.Prompt))
				}
				if b.Len() == 0 {
					return "Nothing scheduled.", nil
				}
				return b.String(), nil
			}),
		tool("cancel_schedule", "Cancel a schedule on your thread or one you made.",
			schema(map[string][2]string{"id": {"string", "the schedule id"}}, "id"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct{ ID string }](args)
				if err != nil {
					return "", err
				}
				sc, err := s.App.Store.Schedule(ctx, a.ID)
				if err != nil {
					return "", err
				}
				if sc.ThreadID != t.ID && sc.By != t.ID {
					return "", errors.New("that schedule is not yours to cancel")
				}
				if err := s.App.RemoveSchedule(ctx, a.ID); err != nil {
					return "", err
				}
				return "Cancelled.", nil
			}),
		tool("list_threads", "List starcode threads (other conversations, yours included) with their id, state and title, newest activity first. A snoozed thread shows when it comes back.",
			schema(map[string][2]string{
				"query":        {"string", "only threads whose title contains this"},
				"state":        {"string", "only threads in this state: active (not settled or snoozed), snoozed, settled, or a status such as running or idle"},
				"all_projects": {"boolean", "include other projects (default: only your project); each line then names its project"},
				"limit":        {"integer", "at most this many (default 20)"},
			}),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct {
					Query       string
					State       string
					AllProjects bool `json:"all_projects"`
					Limit       int
				}](args)
				if err != nil {
					return "", err
				}
				return s.listThreads(ctx, t, a.Query, a.State, a.AllProjects, a.Limit)
			}),
		tool("read_thread", "Read another thread's conversation: the prompts and the agent's replies, without tool calls. A prompt that links /threads/<id> means that thread.",
			schema(map[string][2]string{
				"id":   {"string", "the thread id, or its /threads/<id> link"},
				"last": {"integer", "only the last this many messages (default 10)"},
			}, "id"),
			func(s *Server, ctx context.Context, _ store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct {
					ID   string
					Last int
				}](args)
				if err != nil {
					return "", err
				}
				return s.readThread(ctx, threadIDOf(a.ID), a.Last)
			}),
		tool("spawn_thread", "Start a new thread with its own agent to work on a task in parallel with you. It shows under your thread in the user's sidebar. When it finishes, the user hears from it directly, and its reply comes to you with the next prompt you get, so you do not need to relay it. Set report to now only when you have more work to do with the result (a next step that depends on it); call wait_thread to block on it instead. The child works in the same checkout as you, so give parallel children separate files to touch.",
			schema(map[string][2]string{
				"prompt": {"string", "the task, written for an agent that has not seen your conversation"},
				"title":  {"string", "a short title for the sidebar"},
				"agent":  {"string", "which agent runs it (see thread_info for the names); default: yours"},
				"model":  {"string", "the model; default: yours when the agent is yours"},
				"effort": {"string", "reasoning effort; default: yours when the agent is yours"},
				"report": {"string", "how its reply reaches you: next (default, along with your next prompt), now (a prompt of its own, which starts a turn), none"},
			}, "prompt"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct {
					Prompt, Title, Agent, Model, Effort, Report string
					// WakeMe is the argument report replaced, for sessions
					// that still have the old tool list.
					WakeMe *bool `json:"wake_me"`
				}](args)
				if err != nil {
					return "", err
				}
				report := strings.ToLower(strings.TrimSpace(a.Report))
				switch {
				case report == "" && a.WakeMe != nil && *a.WakeMe:
					report = app.ReportNow
				case report == "" && a.WakeMe != nil:
					report = app.ReportNone
				case report == "":
					report = app.ReportNext
				case report != app.ReportNext && report != app.ReportNow && report != app.ReportNone:
					return "", fmt.Errorf("report is next, now or none, not %q", a.Report)
				}
				return s.spawnThread(ctx, t, a.Prompt, a.Title, a.Agent, a.Model, a.Effort, report)
			}),
		tool("send_to_thread", "Send a prompt to one of the threads you spawned, or to the thread that spawned you. It is queued when that thread is busy.",
			schema(map[string][2]string{
				"id":     {"string", "the thread id"},
				"prompt": {"string", "the message"},
			}, "id", "prompt"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct{ ID, Prompt string }](args)
				if err != nil {
					return "", err
				}
				to, err := s.App.Store.Thread(ctx, a.ID)
				if err != nil {
					return "", err
				}
				if to.Parent != t.ID && t.Parent != to.ID {
					return "", errors.New("you can send to the threads you spawned and to the one that spawned you")
				}
				body := fmt.Sprintf("Message from thread %q (%s):\n\n%s", t.Title, t.ID, a.Prompt)
				if err := s.App.AutoPrompt(ctx, to.ID, body, "agent"); err != nil {
					return "", err
				}
				return "Sent.", nil
			}),
		tool("wait_thread", "Wait for a thread you spawned to finish, then get its status and last reply. Blocks up to timeout_seconds (default 300, at most 840); call again if it is still working.",
			schema(map[string][2]string{
				"id":              {"string", "the thread id"},
				"timeout_seconds": {"integer", "how long to wait at most"},
			}, "id"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct {
					ID      string
					Timeout int `json:"timeout_seconds"`
				}](args)
				if err != nil {
					return "", err
				}
				return s.waitThread(ctx, a.ID, a.Timeout)
			}),
		tool("stop_thread", "Stop the running turn of a thread you spawned.",
			schema(map[string][2]string{"id": {"string", "the thread id"}}, "id"),
			func(s *Server, ctx context.Context, t store.Thread, args json.RawMessage) (string, error) {
				a, err := decode[struct{ ID string }](args)
				if err != nil {
					return "", err
				}
				to, err := s.App.Store.Thread(ctx, a.ID)
				if err != nil {
					return "", err
				}
				if to.Parent != t.ID {
					return "", errors.New("you can only stop the threads you spawned")
				}
				if err := s.App.Interrupt(ctx, to.ID); err != nil {
					return "", err
				}
				return "Stopped.", nil
			}),
	}
}

// clipTitle keeps an agent's title to the length the sidebar shows.
func clipTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}

var prRef = regexp.MustCompile(`^(?:([\w.-]+/[\w.-]+))?#?(\d+)$`)

// resolvePR reads a pull request reference. A bare number belongs to the
// project's repository, which has to be clear: a fork's clone has two.
func (s *Server) resolvePR(ctx context.Context, t store.Thread, ref string) (string, int, error) {
	ref = strings.TrimSpace(ref)
	if repo, n, _, ok := gitx.FindPullURL(ref); ok {
		return repo, n, nil
	}
	m := prRef.FindStringSubmatch(ref)
	if m == nil {
		return "", 0, fmt.Errorf("%q is not a pull request URL, owner/repo#123 or #123", ref)
	}
	n, _ := strconv.Atoi(m[2])
	if m[1] != "" {
		return m[1], n, nil
	}
	p, err := s.App.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return "", 0, err
	}
	repos := s.projectRemotes(ctx, p)
	switch len(repos) {
	case 0:
		return "", 0, errors.New("the project has no GitHub remote; pass the full URL")
	case 1:
		return repos[0], n, nil
	}
	return "", 0, fmt.Errorf("the project has several GitHub remotes (%s); say which as owner/repo#%d", strings.Join(repos, ", "), n)
}

// describeThread is a thread for an agent to read.
func (s *Server) describeThread(ctx context.Context, t store.Thread, self bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\ntitle: %s\n", t.ID, t.Title)
	if p, err := s.App.Store.Project(ctx, t.ProjectID); err == nil {
		fmt.Fprintf(&b, "project: %s (%s)\n", p.Name, p.Path)
	}
	fmt.Fprintf(&b, "agent: %s", t.Agent)
	if t.Model != "" {
		fmt.Fprintf(&b, ", model %s", t.Model)
	}
	if t.Effort != "" {
		fmt.Fprintf(&b, ", effort %s", t.Effort)
	}
	b.WriteString("\n")
	if !self {
		fmt.Fprintf(&b, "status: %s\n", s.App.ThreadState(t))
	}
	if t.PRNumber > 0 {
		fmt.Fprintf(&b, "pull request: %s", t.PRURL)
		if t.PRWatch != "" {
			b.WriteString(" (watched)")
		}
		b.WriteString("\n")
	}
	if t.Snoozed() {
		fmt.Fprintf(&b, "snoozed until: %s\n", t.SnoozedUntil.Local().Format(time.RFC3339))
	}
	if t.Parent != "" {
		if p, err := s.App.Store.Thread(ctx, t.Parent); err == nil {
			fmt.Fprintf(&b, "spawned by: %s %q\n", p.ID, p.Title)
		}
	}
	if kids := s.App.Children(ctx, t.ID); len(kids) > 0 {
		b.WriteString("spawned threads:\n")
		for _, k := range kids {
			fmt.Fprintf(&b, "- %s %q: %s\n", k.ID, k.Title, s.App.ThreadState(k))
		}
	}
	if self {
		fmt.Fprintf(&b, "agents you can spawn: %s\n", strings.Join(s.agentNames(), ", "))
	}
	return b.String()
}

func (s *Server) listThreads(ctx context.Context, caller store.Thread, q, want string, all bool, limit int) (string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	ts, err := s.App.Store.Threads(ctx)
	if err != nil {
		return "", err
	}
	projects := map[string]string{}
	if all {
		ps, err := s.App.Store.Projects(ctx)
		if err != nil {
			return "", err
		}
		for _, p := range ps {
			projects[p.ID] = p.Name
		}
	}
	q = strings.ToLower(strings.TrimSpace(q))
	want = strings.ToLower(strings.TrimSpace(want))
	var b strings.Builder
	n := 0
	for _, t := range ts {
		if n == limit {
			break
		}
		if !all && t.ProjectID != caller.ProjectID {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(t.Title), q) {
			continue
		}
		state := t.Status
		if t.Archived {
			state = "settled"
		} else if t.Snoozed() {
			state = "snoozed"
		}
		if want != "" && want != state && (want != "active" || state == "settled" || state == "snoozed") {
			continue
		}
		mark := ""
		if t.ID == caller.ID {
			mark = " (you)"
		}
		if state == "snoozed" {
			mark += " (snoozed until " + t.SnoozedUntil.UTC().Format("2006-01-02 15:04 UTC") + ")"
		}
		if all {
			mark += " [project " + projects[t.ProjectID] + "]"
		}
		fmt.Fprintf(&b, "%s  %-18s %s%s\n", t.ID, state, t.Title, mark)
		n++
	}
	if n == 0 {
		return "No threads match.", nil
	}
	return b.String(), nil
}

func (s *Server) readThread(ctx context.Context, id string, last int) (string, error) {
	t, err := s.App.Store.Thread(ctx, id)
	if err != nil {
		return "", err
	}
	items, err := s.App.Store.Items(ctx, id)
	if err != nil {
		return "", err
	}
	if last <= 0 {
		last = 10
	}
	var msgs []string
	for _, it := range items {
		var m struct {
			Parent string `json:"parent"`
		}
		json.Unmarshal(it.Meta, &m)
		body := strings.TrimSpace(it.Body)
		if m.Parent != "" || body == "" {
			continue
		}
		switch it.Kind {
		case domain.KindUser:
			msgs = append(msgs, "## Prompt\n\n"+body)
		case domain.KindAssistant:
			msgs = append(msgs, "## Agent\n\n"+body)
		case domain.KindError:
			msgs = append(msgs, "## Error\n\n"+body)
		}
	}
	if len(msgs) > last {
		msgs = msgs[len(msgs)-last:]
	}
	head := s.describeThread(ctx, t, false)
	if len(msgs) == 0 {
		return head + "\nNo messages yet.", nil
	}
	return head + "\n" + strings.Join(msgs, "\n\n"), nil
}

// spawnThread opens a child thread and sends it the first prompt. The
// child never runs in a looser mode than its parent: on the parent's
// agent it takes the parent's mode; on another agent it gets that agent's
// mode that never asks only when the parent is in one too.
func (s *Server) spawnThread(ctx context.Context, parent store.Thread, prompt, title, agentName, model, effort, report string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("prompt is empty")
	}
	if agentName == "" {
		agentName = parent.Agent
	}
	if _, ok := s.App.Agent(agentName); !ok {
		return "", fmt.Errorf("unknown agent %q; these are enabled: %s", agentName, strings.Join(s.agentNames(), ", "))
	}
	mode := parent.PermissionMode
	if agentName == parent.Agent {
		if model == "" {
			model = parent.Model
		}
		if effort == "" {
			effort = parent.Effort
		}
	} else {
		caps, _ := s.capabilities(ctx)
		mode = ""
		if loose := views.DefaultMode(caps[parent.Agent]); loose != "" && loose == parent.PermissionMode {
			mode = views.DefaultMode(caps[agentName])
		}
	}
	id, err := s.App.SpawnThread(ctx, parent.ID, parent.ProjectID, agentName, model, report)
	if err != nil {
		return "", err
	}
	if effort != "" || mode != "" {
		if err := s.App.SetThreadSettings(ctx, id, app.ThreadSettings{Agent: agentName, Model: model, Effort: effort, PermissionMode: mode}); err != nil {
			return "", err
		}
	}
	if title = clipTitle(title); title != "" {
		if err := s.App.RenameThread(ctx, id, title); err != nil {
			return "", err
		}
	}
	if err := s.App.SendPrompt(ctx, id, prompt); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Started thread %s.", id)
	switch report {
	case app.ReportNext:
		msg += " The user hears from it when it finishes, and you get its reply with your next prompt. Finish your turn, or call wait_thread if you need the result now."
	case app.ReportNow:
		msg += " You get a prompt with its reply when it finishes; carry on meanwhile, or call wait_thread."
	}
	return msg, nil
}

// maxWait stays under the tool timeout the agents get (Codex: 900s).
const maxWait = 840

func (s *Server) waitThread(ctx context.Context, id string, timeout int) (string, error) {
	if timeout <= 0 {
		timeout = 300
	}
	if timeout > maxWait {
		timeout = maxWait
	}
	wctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	t, err := s.App.WaitThread(wctx, id)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return "", err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("Thread %s is still working after %ds. Call wait_thread again, or carry on.", id, timeout), nil
	}
	out := fmt.Sprintf("Thread %q (%s) %s.", t.Title, t.ID, s.App.ThreadState(t))
	// Settled, so this reply is the one a pending report would carry.
	if t.Status != domain.StatusRunning {
		s.App.ClearReport(ctx, id)
	}
	if reply := s.App.LastReply(ctx, id); reply != "" {
		out += " Its last reply:\n\n" + reply
	}
	return out, nil
}

// userZone is the reader's time zone as the browser last named it (the
// Schedules page and the layout record it), else the server's.
func (s *Server) userZone(ctx context.Context) string {
	return s.App.Store.Setting(ctx, "user_tz", "")
}

func (s *Server) userLoc(ctx context.Context) *time.Location {
	if tz := s.userZone(ctx); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// threadIDOf takes a thread id or a link to the thread (/threads/<id>,
// with or without the host).
func threadIDOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "/threads/"); i >= 0 {
		s = s[i+len("/threads/"):]
		s, _, _ = strings.Cut(s, "/")
		s, _, _ = strings.Cut(s, "?")
	}
	return s
}
