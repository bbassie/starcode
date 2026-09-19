// Package store persists the event log in SQLite and maintains the read
// projections (projects, threads, items, approvals) in the same transaction
// as each append.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"

	"starcode/internal/domain"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("not found")

type Project struct {
	ID        string
	Path      string
	Name      string
	CreatedAt time.Time
	// Worktrees is whether new threads start in a worktree of their own.
	Worktrees bool
}

type Thread struct {
	ID                string
	ProjectID         string
	Title             string
	Agent             string
	Model             string
	Effort            string
	PermissionMode    string
	ResolvedModel     string // what the agent reported it actually used
	ExternalSessionID string
	Status            string
	StatusDetail      string
	Archived          bool
	Pinned            bool
	// Worktree is the thread's own checkout (thread.worktree_set), empty
	// for the project's main one, and WorktreeBranch its branch; Dir is
	// what to use.
	Worktree       string
	WorktreeBranch string
	// ContextTokens is how much of the model's context window the
	// conversation took up at the last reading, and ContextWindow the limit
	// the agent named for it (0 when it named none, or when the model
	// changed and no turn has run on the new one yet).
	ContextTokens int64
	ContextWindow int64
	// PRRepo, PRNumber and PRURL are the pull request the thread is linked
	// to (thread.pr_linked); PRNumber is 0 when there is none.
	PRRepo    string
	PRNumber  int
	PRURL     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PR names a pull request across repositories.
type PR struct {
	Repo   string // "owner/name"
	Number int
}

// Linked is the thread's pull request, if it has one.
// Dir is where the thread's agent, terminal and panel work: its own
// worktree, or the project's checkout when it has none or the worktree
// is gone from disk (the app puts it back before the next turn).
func (t Thread) Dir(p Project) string {
	if t.Worktree != "" {
		if _, err := os.Stat(t.Worktree); err == nil {
			return t.Worktree
		}
	}
	return p.Path
}

func (t Thread) Linked() (PR, bool) {
	if t.PRNumber == 0 {
		return PR{}, false
	}
	return PR{Repo: t.PRRepo, Number: t.PRNumber}, true
}

// PRState is what the last poll found for a linked pull request. State
// is "open", "merged" or "closed"; Review and Checks use the words of
// gitx.PR; Head is the branch.
type PRState struct {
	PR
	Title     string
	URL       string
	State     string
	Review    string
	Checks    string
	Draft     bool
	Head      string
	CheckedAt time.Time
}

type Item struct {
	ID        string
	ThreadID  string
	Seq       int64
	Kind      string
	ToolName  string
	Status    string
	Body      string
	Output    string
	Meta      json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Approval struct {
	ID          string
	ThreadID    string
	ItemID      string
	ToolName    string
	Description string
	Input       json.RawMessage
	Decision    string
	CreatedAt   time.Time
	ResolvedAt  time.Time // zero while pending
}

type QueuedPrompt struct {
	ID        string
	ThreadID  string
	Seq       int64
	Body      string
	CreatedAt time.Time
}

// UsageEntry is one completed agent turn reconstructed from the event log.
// Agent and Model reflect the settings that were active when the turn ended,
// including threads that have since been deleted from the projections.
type UsageEntry struct {
	ThreadID     string
	SessionID    string
	Agent        string
	Model        string
	CostUSD      float64
	InputTokens  int64
	OutputTokens int64
	CreatedAt    time.Time
}

// Store wraps the database. Published is called after every successful
// append with the committed events; it is where the bus hangs off.
type Store struct {
	db        *sql.DB
	Published func(events []domain.Event)
}

func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc sqlite is safest with one writer connection.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate applies every embedded migration that is not recorded in
// schema_migrations yet. Databases created before that table existed had
// all migrations applied on every start; an ALTER that fails with
// "duplicate column" is therefore treated as already applied.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		var applied int
		if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name=?`, n).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		b, err := migrations.ReadFile(n)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(string(b)); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migration %s: %w", n, err)
		}
		if _, err := s.db.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)`, n, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

const timeFmt = time.RFC3339Nano

// Append writes payloads as events for threadID (may be empty for
// project-level events), applies them to the projections, commits, and
// publishes them.
func (s *Store) Append(ctx context.Context, threadID string, payloads ...any) ([]domain.Event, error) {
	if len(payloads) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	events := make([]domain.Event, 0, len(payloads))
	for _, p := range payloads {
		typ := domain.TypeOf(p)
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO events(thread_id, type, payload, created_at) VALUES(?,?,?,?)`,
			nullIfEmpty(threadID), typ, string(raw), now.Format(timeFmt))
		if err != nil {
			return nil, err
		}
		seq, _ := res.LastInsertId()
		ev := domain.Event{Seq: seq, ThreadID: threadID, Type: typ, Payload: p, CreatedAt: now}
		if err := apply(ctx, tx, ev); err != nil {
			return nil, fmt.Errorf("apply %s: %w", typ, err)
		}
		events = append(events, ev)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if s.Published != nil {
		s.Published(events)
	}
	return events, nil
}

// Replay clears every projection table and rebuilds it from the event log.
func (s *Store) Replay(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, t := range []string{"items_fts", "session_rules", "queued_prompts", "approvals", "items", "threads", "projects"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return 0, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, thread_id, type, payload, created_at FROM events ORDER BY seq`)
	if err != nil {
		return 0, err
	}
	var evs []domain.Event
	for rows.Next() {
		var (
			ev      domain.Event
			tid     sql.NullString
			raw, ts string
		)
		if err := rows.Scan(&ev.Seq, &tid, &ev.Type, &raw, &ts); err != nil {
			rows.Close()
			return 0, err
		}
		ev.ThreadID = tid.String
		ev.CreatedAt, _ = time.Parse(timeFmt, ts)
		p, err := domain.Decode(ev.Type, []byte(raw))
		if err != nil {
			rows.Close()
			return 0, err
		}
		ev.Payload = p
		evs = append(evs, ev)
	}
	rows.Close()
	for _, ev := range evs {
		if err := apply(ctx, tx, ev); err != nil {
			return 0, fmt.Errorf("replay seq %d (%s): %w", ev.Seq, ev.Type, err)
		}
	}
	return len(evs), tx.Commit()
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func apply(ctx context.Context, tx execer, ev domain.Event) error {
	ts := ev.CreatedAt.Format(timeFmt)
	touch := func() error {
		_, err := tx.ExecContext(ctx, `UPDATE threads SET updated_at=? WHERE id=?`, ts, ev.ThreadID)
		return err
	}
	switch p := deref(ev.Payload).(type) {
	case domain.ProjectAdded:
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO projects(id,path,name,created_at) VALUES(?,?,?,?)`, p.ID, p.Path, p.Name, ts)
		return err
	case domain.ProjectRemoved:
		_, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, p.ID)
		return err
	case domain.ThreadCreated:
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO threads(id,project_id,title,agent,model,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
			p.ID, p.ProjectID, p.Title, p.Agent, p.Model, ts, ts)
		return err
	case domain.ThreadRenamed:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET title=?, updated_at=? WHERE id=?`, p.Title, ts, ev.ThreadID)
		return err
	case domain.ThreadDeleted:
		// The items go with the thread through the foreign key; the search
		// index has no such key, so its rows go first, while the items
		// still say which ones they are.
		if _, err := tx.ExecContext(ctx, `DELETE FROM items_fts WHERE rowid IN (SELECT seq FROM items WHERE thread_id=?)`, ev.ThreadID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE id=?`, ev.ThreadID)
		return err
	case domain.ThreadArchived:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET archived=1, updated_at=? WHERE id=?`, ts, ev.ThreadID)
		return err
	case domain.ThreadUnarchived:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET archived=0, updated_at=? WHERE id=?`, ts, ev.ThreadID)
		return err
	case domain.ThreadWorktreeSet:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET worktree=?, worktree_branch=? WHERE id=?`, p.Path, p.Branch, ev.ThreadID)
		return err
	case domain.ProjectSettingsChanged:
		_, err := tx.ExecContext(ctx, `UPDATE projects SET worktrees=? WHERE id=?`, p.Worktrees, p.ID)
		return err
	case domain.ThreadPinned:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET pinned=1 WHERE id=?`, ev.ThreadID)
		return err
	case domain.ThreadUnpinned:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET pinned=0 WHERE id=?`, ev.ThreadID)
		return err
	case domain.ThreadPRLinked:
		// A link is bookkeeping, not activity: no touch, so the thread does
		// not go unread over it.
		_, err := tx.ExecContext(ctx, `UPDATE threads SET pr_repo=?, pr_number=?, pr_url=? WHERE id=?`, p.Repo, p.Number, p.URL, ev.ThreadID)
		return err
	case domain.ThreadPRUnlinked:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET pr_repo='', pr_number=0, pr_url='' WHERE id=?`, ev.ThreadID)
		return err
	case domain.ThreadSettingsChanged:
		// A different agent cannot resume another agent's session. Another
		// model has another window, so the one the old model reported is
		// dropped and the catalog answers until the next turn; the count
		// stays, because the conversation is the same one.
		_, err := tx.ExecContext(ctx, `UPDATE threads SET model=?, effort=?, permission_mode=?, external_session_id=CASE WHEN agent=? THEN external_session_id ELSE '' END, context_window=CASE WHEN model=? THEN context_window ELSE 0 END, agent=?, updated_at=? WHERE id=?`,
			p.Model, p.Effort, p.PermissionMode, p.Agent, p.Model, p.Agent, ts, ev.ThreadID)
		return err
	case domain.AgentSessionBound:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET external_session_id=?, resolved_model=CASE WHEN ?='' THEN resolved_model ELSE ? END, updated_at=? WHERE id=?`,
			p.ExternalID, p.Model, p.Model, ts, ev.ThreadID)
		return err
	case domain.ThreadStatusChanged:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET status=?, status_detail=?, updated_at=? WHERE id=?`, p.Status, p.Detail, ts, ev.ThreadID)
		return err
	case domain.TurnStarted:
		return touch()
	case domain.TurnCompleted:
		return touch()
	case domain.ContextUsed:
		// Only the newest reading matters, and it is not activity: no touch,
		// so a thread does not go unread because its window filled up.
		_, err := tx.ExecContext(ctx, `UPDATE threads SET context_tokens=?, context_window=CASE WHEN ?=0 THEN context_window ELSE ? END WHERE id=?`,
			p.Tokens, p.Window, p.Window, ev.ThreadID)
		return err
	case domain.PromptQueued:
		_, err := tx.ExecContext(ctx, `INSERT INTO queued_prompts(id,thread_id,seq,body,created_at) VALUES(?,?,?,?,?)`,
			p.ID, ev.ThreadID, ev.Seq, p.Body, ts)
		if err != nil {
			return err
		}
		return touch()
	case domain.PromptDequeued:
		_, err := tx.ExecContext(ctx, `DELETE FROM queued_prompts WHERE thread_id=? AND id=?`, ev.ThreadID, p.ID)
		if err != nil {
			return err
		}
		return touch()
	case domain.ItemStarted:
		meta := string(p.Meta)
		if meta == "" {
			meta = "{}"
		}
		// A second start for the same id replaces the row and its seq, so
		// the index row under the old seq goes first.
		if err := unindexItem(ctx, tx, p.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO items(id,thread_id,seq,kind,tool_name,status,body,output,meta,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'',?,?,?)`,
			p.ID, ev.ThreadID, ev.Seq, p.Kind, p.ToolName, p.Status, p.Body, meta, ts, ts)
		if err != nil {
			return err
		}
		// Prompts arrive whole. A streamed reply is indexed when it
		// completes, so its tokens do not each rewrite the index.
		if p.Kind == domain.KindUser || (p.Status != domain.ItemRunning && p.Status != domain.ItemPending) {
			if err := indexItem(ctx, tx, p.ID); err != nil {
				return err
			}
		}
		return touch()
	case domain.ItemDelta:
		col := "body"
		if p.Field == "output" {
			col = "output"
		}
		_, err := tx.ExecContext(ctx, `UPDATE items SET `+col+` = `+col+` || ?, updated_at=? WHERE id=?`, p.Text, ts, p.ID)
		return err
	case domain.ItemCompleted:
		q := `UPDATE items SET updated_at=?`
		args := []any{ts}
		if p.Status != "" {
			q += `, status=?`
			args = append(args, p.Status)
		}
		if p.Body != "" {
			q += `, body=?`
			args = append(args, p.Body)
		}
		if len(p.Meta) > 0 {
			q += `, meta=?`
			args = append(args, string(p.Meta))
		}
		q += ` WHERE id=?`
		args = append(args, p.ID)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
		return indexItem(ctx, tx, p.ID)
	case domain.ApprovalRequested:
		in := string(p.Input)
		if in == "" {
			in = "{}"
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO approvals(id,thread_id,item_id,tool_name,description,input,decision,created_at) VALUES(?,?,?,?,?,?,'',?)`,
			p.ID, ev.ThreadID, p.ItemID, p.ToolName, p.Description, in, ts)
		return err
	case domain.ApprovalResolved:
		_, err := tx.ExecContext(ctx, `UPDATE approvals SET decision=?, resolved_at=? WHERE thread_id=? AND id=?`, p.Decision, ts, ev.ThreadID, p.ID)
		if err != nil || p.Decision != domain.DecisionAllowSession || p.Auto {
			return err
		}
		// A fresh "allow for session" answer becomes a rule of the thread.
		var tool, input string
		if err := tx.QueryRowContext(ctx, `SELECT tool_name, input FROM approvals WHERE thread_id=? AND id=?`, ev.ThreadID, p.ID).Scan(&tool, &input); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_rules(thread_id, key, created_at) VALUES(?,?,?)`, ev.ThreadID, domain.RuleKey(tool, json.RawMessage(input)), ts)
		return err
	case domain.RuleRevoked:
		_, err := tx.ExecContext(ctx, `DELETE FROM session_rules WHERE thread_id=? AND key=?`, ev.ThreadID, p.Key)
		return err
	case domain.GitChanged:
		return nil
	}
	return fmt.Errorf("no projection for %T", ev.Payload)
}

// unindexItem removes an item's row from the search index. The index row's
// rowid is the item's seq, so the delete is a key lookup; item_id is not
// indexed and a delete by it would read the whole table.
func unindexItem(ctx context.Context, tx execer, id string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM items_fts WHERE rowid = (SELECT seq FROM items WHERE id=?)`, id)
	return err
}

// indexItem writes an item's current body to the search index, replacing
// the row it had. Only prompts and replies with text are indexed; for any
// other item this removes nothing and inserts nothing.
func indexItem(ctx context.Context, tx execer, id string) error {
	if err := unindexItem(ctx, tx, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO items_fts(rowid, item_id, thread_id, body)
		SELECT seq, id, thread_id, body FROM items
		WHERE id=? AND kind IN ('user','assistant') AND body != ''`, id)
	return err
}

// deref lets apply switch on value types whether the payload was appended
// as a value or, after Decode, as a pointer.
func deref(p any) any {
	switch v := p.(type) {
	case *domain.ProjectAdded:
		return *v
	case *domain.ProjectRemoved:
		return *v
	case *domain.ThreadCreated:
		return *v
	case *domain.ThreadRenamed:
		return *v
	case *domain.ThreadDeleted:
		return *v
	case *domain.ThreadArchived:
		return *v
	case *domain.ThreadUnarchived:
		return *v
	case *domain.ThreadWorktreeSet:
		return *v
	case *domain.ProjectSettingsChanged:
		return *v
	case *domain.ThreadPinned:
		return *v
	case *domain.ThreadUnpinned:
		return *v
	case *domain.ThreadPRLinked:
		return *v
	case *domain.ThreadPRUnlinked:
		return *v
	case *domain.ThreadSettingsChanged:
		return *v
	case *domain.AgentSessionBound:
		return *v
	case *domain.ThreadStatusChanged:
		return *v
	case *domain.TurnStarted:
		return *v
	case *domain.TurnCompleted:
		return *v
	case *domain.ContextUsed:
		return *v
	case *domain.PromptQueued:
		return *v
	case *domain.PromptDequeued:
		return *v
	case *domain.ItemStarted:
		return *v
	case *domain.ItemDelta:
		return *v
	case *domain.ItemCompleted:
		return *v
	case *domain.ApprovalRequested:
		return *v
	case *domain.ApprovalResolved:
		return *v
	case *domain.RuleRevoked:
		return *v
	case *domain.GitChanged:
		return *v
	}
	return p
}

// runeIndex is strings.Index over rune slices.
func runeIndex(hay, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- readers ----

func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,path,name,created_at,worktrees FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var ts string
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &ts, &p.Worktrees); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(timeFmt, ts)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Project(ctx context.Context, id string) (Project, error) {
	var p Project
	var ts string
	err := s.db.QueryRowContext(ctx, `SELECT id,path,name,created_at,worktrees FROM projects WHERE id=?`, id).Scan(&p.ID, &p.Path, &p.Name, &ts, &p.Worktrees)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.CreatedAt, _ = time.Parse(timeFmt, ts)
	return p, err
}

const threadCols = `id,project_id,title,agent,model,effort,permission_mode,resolved_model,external_session_id,status,status_detail,archived,pinned,worktree,worktree_branch,context_tokens,context_window,pr_repo,pr_number,pr_url,created_at,updated_at`

func scanThread(sc interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var c, u string
	err := sc.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Agent, &t.Model, &t.Effort, &t.PermissionMode, &t.ResolvedModel, &t.ExternalSessionID, &t.Status, &t.StatusDetail, &t.Archived, &t.Pinned, &t.Worktree, &t.WorktreeBranch, &t.ContextTokens, &t.ContextWindow, &t.PRRepo, &t.PRNumber, &t.PRURL, &c, &u)
	t.CreatedAt, _ = time.Parse(timeFmt, c)
	t.UpdatedAt, _ = time.Parse(timeFmt, u)
	return t, err
}

// Threads lists every thread, newest activity first.
func (s *Store) Threads(ctx context.Context) ([]Thread, error) {
	// Pinned threads lead, so every list that walks this order (the
	// sidebar, the project cards) shows them first.
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadCols+` FROM threads ORDER BY pinned DESC, updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Usage returns completed turns at or after since. It reads the event log
// rather than a projection so existing databases immediately have history.
func (s *Store) Usage(ctx context.Context, since time.Time) ([]UsageEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT thread_id,type,payload,created_at FROM events
		WHERE thread_id IS NOT NULL AND type IN ('thread.created','thread.settings','thread.agent','thread.session_bound','turn.completed')
		ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type threadUsageState struct{ agent, model, sessionID string }
	states := make(map[string]threadUsageState)
	var out []UsageEntry
	for rows.Next() {
		var threadID, typ, raw, created string
		if err := rows.Scan(&threadID, &typ, &raw, &created); err != nil {
			return nil, err
		}
		payload, err := domain.Decode(typ, []byte(raw))
		if err != nil {
			return nil, err
		}
		state := states[threadID]
		switch p := deref(payload).(type) {
		case domain.ThreadCreated:
			state.agent, state.model = p.Agent, p.Model
		case domain.ThreadSettingsChanged:
			if state.agent != p.Agent {
				state.sessionID = ""
			}
			state.agent, state.model = p.Agent, p.Model
		case domain.AgentSessionBound:
			state.sessionID = p.ExternalID
			if p.Model != "" {
				state.model = p.Model
			}
		case domain.TurnCompleted:
			ts, _ := time.Parse(timeFmt, created)
			if since.IsZero() || !ts.Before(since) {
				model := state.model
				if model == "" {
					model = "default"
				}
				out = append(out, UsageEntry{ThreadID: threadID, SessionID: state.sessionID, Agent: state.agent, Model: model, CostUSD: p.CostUSD, InputTokens: p.InputTok, OutputTokens: p.OutputTok, CreatedAt: ts})
			}
		}
		states[threadID] = state
	}
	return out, rows.Err()
}

func (s *Store) Thread(ctx context.Context, id string) (Thread, error) {
	t, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadCols+` FROM threads WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Store) Items(ctx context.Context, threadID string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,thread_id,seq,kind,tool_name,status,body,output,meta,created_at,updated_at FROM items WHERE thread_id=? ORDER BY seq`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ItemsByParent returns the items a subagent made under tool call
// parentID (their meta names it), in order.
func (s *Store) ItemsByParent(ctx context.Context, threadID, parentID string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,thread_id,seq,kind,tool_name,status,body,output,meta,created_at,updated_at FROM items WHERE thread_id=? AND json_extract(meta, '$.parent') = ? ORDER BY seq`, threadID, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ItemsFrom returns the thread's items from item firstID on, in order.
// The work block header is redrawn on every tool event, and it only needs
// the block's own items, not the whole transcript.
func (s *Store) ItemsFrom(ctx context.Context, threadID, firstID string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,thread_id,seq,kind,tool_name,status,body,output,meta,created_at,updated_at FROM items WHERE thread_id=? AND seq >= (SELECT seq FROM items WHERE id=?) ORDER BY seq`, threadID, firstID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// QueuedPrompts returns follow-ups waiting behind the active turn, oldest
// first. They are projected separately from items to preserve transcript order.
func (s *Store) QueuedPrompts(ctx context.Context, threadID string) ([]QueuedPrompt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,thread_id,seq,body,created_at FROM queued_prompts WHERE thread_id=? ORDER BY seq`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedPrompt
	for rows.Next() {
		var q QueuedPrompt
		var created string
		if err := rows.Scan(&q.ID, &q.ThreadID, &q.Seq, &q.Body, &created); err != nil {
			return nil, err
		}
		q.CreatedAt, _ = time.Parse(timeFmt, created)
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) Item(ctx context.Context, id string) (Item, error) {
	it, err := scanItem(s.db.QueryRowContext(ctx, `SELECT id,thread_id,seq,kind,tool_name,status,body,output,meta,created_at,updated_at FROM items WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	return it, err
}

func scanItem(sc interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var meta, c, u string
	err := sc.Scan(&it.ID, &it.ThreadID, &it.Seq, &it.Kind, &it.ToolName, &it.Status, &it.Body, &it.Output, &meta, &c, &u)
	it.Meta = json.RawMessage(meta)
	it.CreatedAt, _ = time.Parse(timeFmt, c)
	it.UpdatedAt, _ = time.Parse(timeFmt, u)
	return it, err
}

const approvalCols = `id,thread_id,item_id,tool_name,description,input,decision,created_at,resolved_at`

// PendingApprovals returns unresolved approvals for a thread, oldest first.
func (s *Store) PendingApprovals(ctx context.Context, threadID string) ([]Approval, error) {
	return s.approvals(ctx, `SELECT `+approvalCols+` FROM approvals WHERE thread_id=? AND decision='' ORDER BY created_at`, threadID)
}

// Approvals returns every approval of a thread, answered or not, oldest
// first. The transcript renders the pending ones as cards and uses the
// answered ones to leave waiting time out of the "worked for" counts.
func (s *Store) Approvals(ctx context.Context, threadID string) ([]Approval, error) {
	return s.approvals(ctx, `SELECT `+approvalCols+` FROM approvals WHERE thread_id=? ORDER BY created_at`, threadID)
}

func (s *Store) approvals(ctx context.Context, q string, args ...any) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Approval(ctx context.Context, threadID, id string) (Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE thread_id=? AND id=?`, threadID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func scanApproval(sc interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var in, c, r string
	err := sc.Scan(&a.ID, &a.ThreadID, &a.ItemID, &a.ToolName, &a.Description, &in, &a.Decision, &c, &r)
	a.Input = json.RawMessage(in)
	a.CreatedAt, _ = time.Parse(timeFmt, c)
	if r != "" {
		a.ResolvedAt, _ = time.Parse(timeFmt, r)
	}
	return a, err
}

// SearchHit is one transcript message that matched a search: enough to
// list it under its thread and jump to the row.
type SearchHit struct {
	ThreadID    string
	ThreadTitle string
	ProjectID   string
	ItemID      string
	Kind        string
	Snippet     string
	CreatedAt   time.Time
}

// likePattern turns free text into a LIKE pattern that matches it anywhere,
// with the wildcard characters escaped (ESCAPE '\' in the query).
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// SearchThreads lists threads whose title contains q, newest activity
// first. An empty q lists the most recent ones.
func (s *Store) SearchThreads(ctx context.Context, q string, limit int) ([]Thread, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadCols+` FROM threads WHERE title LIKE ? ESCAPE '\' ORDER BY updated_at DESC LIMIT ?`, likePattern(q), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ftsQuery turns free text into an FTS5 query that cannot be read as
// query syntax: every word becomes a quoted string with its quotes
// doubled, followed by * so it matches as a prefix, and the words are
// joined with spaces, which FTS5 reads as AND. A word without a letter or
// digit holds no token for the tokenizer and is dropped, since it would
// match nothing and take the other words down with it. "" means there is
// nothing to search for.
func ftsQuery(q string) string {
	var terms []string
	for _, w := range strings.Fields(q) {
		if strings.IndexFunc(w, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0 {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}

// SearchItems finds finished prompts and replies that hold every word of
// q, each matched as a word prefix, newest first, with a snippet of text
// around the first match. It reads the items_fts index; the index rowid
// is the item's seq, so ordering by it is newest first without a sort
// over the bodies.
func (s *Store) SearchItems(ctx context.Context, q string, limit int) ([]SearchHit, error) {
	match := ftsQuery(q)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.thread_id, i.kind, i.body, i.created_at, t.title, t.project_id
		FROM items_fts f JOIN items i ON i.id = f.item_id JOIN threads t ON t.id = i.thread_id
		WHERE items_fts MATCH ?
		ORDER BY f.rowid DESC LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var body, created string
		if err := rows.Scan(&h.ItemID, &h.ThreadID, &h.Kind, &body, &created, &h.ThreadTitle, &h.ProjectID); err != nil {
			return nil, err
		}
		h.CreatedAt, _ = time.Parse(timeFmt, created)
		h.Snippet = snippet(body, q, 160)
		out = append(out, h)
	}
	return out, rows.Err()
}

// snippet returns about width characters of body around the first
// case-insensitive occurrence of q, on one line. The search matches word
// by word, so when q as a whole is not in the text its words are tried in
// turn, each trimmed to its letters and digits as the tokenizer does; with
// none found the snippet is the start of the body. Runes are folded one
// by one so the index found in the folded text is an index into r.
func snippet(body, q string, width int) string {
	text := strings.Join(strings.Fields(body), " ")
	r := []rune(text)
	fold := func(r []rune) []rune {
		out := make([]rune, len(r))
		for i, c := range r {
			out[i] = unicode.ToLower(c)
		}
		return out
	}
	notWord := func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) }
	needles := []string{strings.Join(strings.Fields(q), " ")}
	for _, w := range strings.Fields(q) {
		if w = strings.TrimFunc(w, notWord); w != "" {
			needles = append(needles, w)
		}
	}
	start := 0
	folded := fold(r)
	for _, n := range needles {
		at := runeIndex(folded, fold([]rune(n)))
		if at < 0 {
			continue
		}
		if start = at - width/3; start < 0 {
			start = 0
		}
		break
	}
	end := start + width
	if end > len(r) {
		end = len(r)
		start = end - width
		if start < 0 {
			start = 0
		}
	}
	out := string(r[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(r) {
		out += "…"
	}
	return out
}

// SessionRules lists a thread's "allow for session" rule keys, sorted.
func (s *Store) SessionRules(ctx context.Context, threadID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM session_rules WHERE thread_id=? ORDER BY key`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ---- scratch state: drafts and seen marks ----
//
// These are not events. A draft changes on every pause in typing and a
// seen mark on every look; neither is history worth replaying, and both
// belong to the reader rather than the thread.

// SaveDraft keeps the composer text under key (a thread id, or "home");
// an empty body removes it.
func (s *Store) SaveDraft(ctx context.Context, key, body string) error {
	if body == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM drafts WHERE key=?`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO drafts(key, body, updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET body=excluded.body, updated_at=excluded.updated_at`,
		key, body, time.Now().UTC().Format(timeFmt))
	return err
}

// Draft returns the saved text for key, or "".
func (s *Store) Draft(ctx context.Context, key string) (string, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM drafts WHERE key=?`, key).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return body, err
}

// MarkSeen records that threadID was on screen at t. Never moves back.
// changed reports whether the mark took an unread row (an idle thread
// with activity since the last look) to read, so the caller knows when
// other pages need a redraw; during a turn nothing is marked unread, so
// nothing changes.
func (s *Store) MarkSeen(ctx context.Context, threadID string, t time.Time) (changed bool, err error) {
	ts := t.UTC().Format(timeFmt)
	var status, updated string
	var prev sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT t.status, t.updated_at, s.seen_at FROM threads t LEFT JOIN seen s ON s.thread_id=t.id WHERE t.id=?`, threadID).Scan(&status, &updated, &prev)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Already marked since the thread last changed: the row says read and
	// would keep saying so, so skip the write. Every event on an open
	// thread lands here, so this is most calls.
	if prev.Valid && prev.String >= updated {
		return false, nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO seen(thread_id, seen_at) VALUES(?,?) ON CONFLICT(thread_id) DO UPDATE SET seen_at=max(seen_at, excluded.seen_at)`, threadID, ts)
	idle := status != domain.StatusRunning && status != domain.StatusAwaitingApproval
	return err == nil && idle && (!prev.Valid || prev.String < updated) && ts >= updated, err
}

// Seen maps thread ids to when they were last on screen. A thread with
// no entry was never opened.
func (s *Store) Seen(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT thread_id, seen_at FROM seen`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, ts string
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		out[id], _ = time.Parse(timeFmt, ts)
	}
	return out, rows.Err()
}

// ---- settings ----
//
// Instance-wide preferences from the settings page. Like drafts and seen
// marks they are reader state, not events: a change applies from now on
// and nothing replays it.

// Setting returns the value stored under key, or def when there is none.
func (s *Store) Setting(ctx context.Context, key, def string) string {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

// SetSetting stores value under key, replacing what was there.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// SettleMerged reports whether a thread archives on its own when the poll
// sees its pull request merged (setting settle_merged, on by default).
func (s *Store) SettleMerged(ctx context.Context) bool {
	return s.Setting(ctx, "settle_merged", "1") != "0"
}

// SettleIdleDays is how many days without activity archive a thread on
// its own (setting settle_idle_days, 3 by default); 0 means never.
func (s *Store) SettleIdleDays(ctx context.Context) int {
	n, err := strconv.Atoi(s.Setting(ctx, "settle_idle_days", "3"))
	if err != nil || n < 0 {
		return 3
	}
	return n
}

// ---- compaction ----

// SavePRStates records what a poll found for linked pull requests.
func (s *Store) SavePRStates(ctx context.Context, states []PRState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, st := range states {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO pr_state(repo,number,title,url,state,review,checks,draft,head,checked_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			st.Repo, st.Number, st.Title, st.URL, st.State, st.Review, st.Checks, st.Draft, st.Head, st.CheckedAt.UTC().Format(timeFmt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PRStates is the last known state of every pull request polled so far,
// keyed by repository and number.
func (s *Store) PRStates(ctx context.Context) (map[PR]PRState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo,number,title,url,state,review,checks,draft,head,checked_at FROM pr_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[PR]PRState{}
	for rows.Next() {
		var st PRState
		var at string
		if err := rows.Scan(&st.Repo, &st.Number, &st.Title, &st.URL, &st.State, &st.Review, &st.Checks, &st.Draft, &st.Head, &at); err != nil {
			return nil, err
		}
		st.CheckedAt, _ = time.Parse(timeFmt, at)
		out[st.PR] = st
	}
	return out, rows.Err()
}

// compactBatch is how many delta groups Compact folds in one transaction.
// A variable so a test can make a small log span several batches.
var compactBatch = 200

// Compact folds each item's streamed deltas into one event per field.
// Streaming writes a row per token; once a turn is over only the sum
// matters, and replay reads the same projection from one row as from a
// thousand. threadID "" compacts every thread. Returns the rows removed.
//
// A group is the deltas of one item and one field. The first pass lists
// the groups with more than one row, which costs a few numbers per group
// and no text. The groups are then folded compactBatch at a time, each
// batch in its own transaction, so memory holds the text of one batch and
// the one connection is free for appends between batches.
func (s *Store) Compact(ctx context.Context, threadID string) (int, error) {
	type group struct {
		key         string
		first, last int64
	}
	q := `SELECT json_extract(payload, '$.id'), COALESCE(json_extract(payload, '$.field'), ''), min(seq), max(seq)
		FROM events WHERE type='item.delta'`
	var args []any
	if threadID != "" {
		q += ` AND thread_id=?`
		args = append(args, threadID)
	}
	rows, err := s.db.QueryContext(ctx, q+` GROUP BY 1, 2 HAVING count(*) > 1 ORDER BY min(seq)`, args...)
	if err != nil {
		return 0, err
	}
	var groups []group
	for rows.Next() {
		var id sql.NullString
		var field string
		var g group
		if err := rows.Scan(&id, &field, &g.first, &g.last); err != nil {
			rows.Close()
			return 0, err
		}
		g.key = id.String + "\x00" + field
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	removed := 0
	for i := 0; i < len(groups); i += compactBatch {
		batch := groups[i:min(i+compactBatch, len(groups))]
		lo, hi := batch[0].first, batch[0].last
		want := make(map[string]bool, len(batch))
		for _, g := range batch {
			lo, hi = min(lo, g.first), max(hi, g.last)
			want[g.key] = true
		}
		n, err := s.compactRange(ctx, threadID, lo, hi, want)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// compactRange folds the delta groups named in want, in one transaction.
// It reads the events from seq lo to hi, which the caller set to span the
// groups; the groups are listed in the order they first appear, so the
// span of a batch is a stretch of the log and not all of it. Deltas of
// other groups inside the span are skipped. Reading inside the
// transaction means no append lands between the read and the write.
func (s *Store) compactRange(ctx context.Context, threadID string, lo, hi int64, want map[string]bool) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := `SELECT seq, payload FROM events WHERE seq BETWEEN ? AND ? AND type='item.delta'`
	args := []any{lo, hi}
	if threadID != "" {
		q += ` AND thread_id=?`
		args = append(args, threadID)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY seq`, args...)
	if err != nil {
		return 0, err
	}
	type fold struct {
		first  int64
		delta  domain.ItemDelta
		text   strings.Builder
		others []int64
	}
	var order []*fold
	folds := map[string]*fold{}
	for rows.Next() {
		var seq int64
		var raw string
		if err := rows.Scan(&seq, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		var d domain.ItemDelta
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			rows.Close()
			return 0, err
		}
		k := d.ID + "\x00" + d.Field
		if !want[k] {
			continue
		}
		f := folds[k]
		if f == nil {
			f = &fold{first: seq, delta: d}
			folds[k] = f
			order = append(order, f)
		} else {
			f.others = append(f.others, seq)
		}
		f.text.WriteString(d.Text)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	removed := 0
	for _, f := range order {
		if len(f.others) == 0 {
			continue
		}
		f.delta.Text = f.text.String()
		raw, err := json.Marshal(f.delta)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET payload=? WHERE seq=?`, string(raw), f.first); err != nil {
			return 0, err
		}
		for i := 0; i < len(f.others); i += 500 {
			part := f.others[i:min(i+500, len(f.others))]
			ph := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
			seqs := make([]any, 0, len(part))
			for _, sq := range part {
				seqs = append(seqs, sq)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE seq IN (`+ph+`)`, seqs...); err != nil {
				return 0, err
			}
		}
		removed += len(f.others)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}
