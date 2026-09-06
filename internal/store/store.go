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
	"sort"
	"strings"
	"time"

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
	CreatedAt         time.Time
	UpdatedAt         time.Time
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
	for _, t := range []string{"queued_prompts", "approvals", "items", "threads", "projects"} {
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
		_, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE id=?`, ev.ThreadID)
		return err
	case domain.ThreadArchived:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET archived=1, updated_at=? WHERE id=?`, ts, ev.ThreadID)
		return err
	case domain.ThreadUnarchived:
		_, err := tx.ExecContext(ctx, `UPDATE threads SET archived=0, updated_at=? WHERE id=?`, ts, ev.ThreadID)
		return err
	case domain.ThreadSettingsChanged:
		// A different agent cannot resume another agent's session.
		_, err := tx.ExecContext(ctx, `UPDATE threads SET model=?, effort=?, permission_mode=?, external_session_id=CASE WHEN agent=? THEN external_session_id ELSE '' END, agent=?, updated_at=? WHERE id=?`,
			p.Model, p.Effort, p.PermissionMode, p.Agent, p.Agent, ts, ev.ThreadID)
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
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO items(id,thread_id,seq,kind,tool_name,status,body,output,meta,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'',?,?,?)`,
			p.ID, ev.ThreadID, ev.Seq, p.Kind, p.ToolName, p.Status, p.Body, meta, ts, ts)
		if err != nil {
			return err
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
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	case domain.ApprovalRequested:
		in := string(p.Input)
		if in == "" {
			in = "{}"
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO approvals(id,thread_id,item_id,tool_name,description,input,decision,created_at) VALUES(?,?,?,?,?,?,'',?)`,
			p.ID, ev.ThreadID, p.ItemID, p.ToolName, p.Description, in, ts)
		return err
	case domain.ApprovalResolved:
		_, err := tx.ExecContext(ctx, `UPDATE approvals SET decision=? WHERE thread_id=? AND id=?`, p.Decision, ev.ThreadID, p.ID)
		return err
	case domain.GitChanged:
		return nil
	}
	return fmt.Errorf("no projection for %T", ev.Payload)
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
	case *domain.GitChanged:
		return *v
	}
	return p
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- readers ----

func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,path,name,created_at FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var ts string
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &ts); err != nil {
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
	err := s.db.QueryRowContext(ctx, `SELECT id,path,name,created_at FROM projects WHERE id=?`, id).Scan(&p.ID, &p.Path, &p.Name, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.CreatedAt, _ = time.Parse(timeFmt, ts)
	return p, err
}

const threadCols = `id,project_id,title,agent,model,effort,permission_mode,resolved_model,external_session_id,status,status_detail,archived,created_at,updated_at`

func scanThread(sc interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var c, u string
	err := sc.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Agent, &t.Model, &t.Effort, &t.PermissionMode, &t.ResolvedModel, &t.ExternalSessionID, &t.Status, &t.StatusDetail, &t.Archived, &c, &u)
	t.CreatedAt, _ = time.Parse(timeFmt, c)
	t.UpdatedAt, _ = time.Parse(timeFmt, u)
	return t, err
}

// Threads lists every thread, newest activity first.
func (s *Store) Threads(ctx context.Context) ([]Thread, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadCols+` FROM threads ORDER BY updated_at DESC`)
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

// PendingApprovals returns unresolved approvals for a thread, oldest first.
func (s *Store) PendingApprovals(ctx context.Context, threadID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,thread_id,item_id,tool_name,description,input,decision,created_at FROM approvals WHERE thread_id=? AND decision='' ORDER BY created_at`, threadID)
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
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT id,thread_id,item_id,tool_name,description,input,decision,created_at FROM approvals WHERE thread_id=? AND id=?`, threadID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func scanApproval(sc interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var in, c string
	err := sc.Scan(&a.ID, &a.ThreadID, &a.ItemID, &a.ToolName, &a.Description, &in, &a.Decision, &c)
	a.Input = json.RawMessage(in)
	a.CreatedAt, _ = time.Parse(timeFmt, c)
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

// SearchItems finds prompts and replies containing q, newest first, with a
// snippet of text around the first match.
func (s *Store) SearchItems(ctx context.Context, q string, limit int) ([]SearchHit, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.thread_id, i.kind, i.body, i.created_at, t.title, t.project_id
		FROM items i JOIN threads t ON t.id = i.thread_id
		WHERE i.kind IN ('user','assistant') AND i.body LIKE ? ESCAPE '\'
		ORDER BY i.seq DESC LIMIT ?`, likePattern(q), limit)
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
// case-insensitive occurrence of q, on one line.
func snippet(body, q string, width int) string {
	text := strings.Join(strings.Fields(body), " ")
	r := []rune(text)
	at := strings.Index(strings.ToLower(text), strings.ToLower(q))
	start := 0
	if at > 0 {
		start = len([]rune(text[:at]))
		start -= width / 3
		if start < 0 {
			start = 0
		}
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
