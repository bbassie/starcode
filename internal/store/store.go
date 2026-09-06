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
	for _, t := range []string{"session_rules", "queued_prompts", "approvals", "items", "threads", "projects"} {
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
// case-insensitive occurrence of q, on one line. Runes are folded one by
// one so the index found in the folded text is an index into r.
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
	start := 0
	if at := runeIndex(fold(r), fold([]rune(q))); at > 0 {
		start = at - width/3
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

// ---- compaction ----

// Compact folds each item's streamed deltas into one event per field.
// Streaming writes a row per token; once a turn is over only the sum
// matters, and replay reads the same projection from one row as from a
// thousand. threadID "" compacts every thread. Returns the rows removed.
func (s *Store) Compact(ctx context.Context, threadID string) (int, error) {
	q := `SELECT seq, payload FROM events WHERE type='item.delta'`
	var args []any
	if threadID != "" {
		q += ` AND thread_id=?`
		args = append(args, threadID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY seq`, args...)
	if err != nil {
		return 0, err
	}
	type group struct {
		first  int64
		delta  domain.ItemDelta
		text   strings.Builder
		others []int64
	}
	var order []string
	groups := map[string]*group{}
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
		g := groups[k]
		if g == nil {
			g = &group{first: seq, delta: d}
			groups[k] = g
			order = append(order, k)
		} else {
			g.others = append(g.others, seq)
		}
		g.text.WriteString(d.Text)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	removed := 0
	for _, k := range order {
		g := groups[k]
		if len(g.others) == 0 {
			continue
		}
		g.delta.Text = g.text.String()
		raw, err := json.Marshal(g.delta)
		if err != nil {
			return removed, err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return removed, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET payload=? WHERE seq=?`, string(raw), g.first); err != nil {
			tx.Rollback()
			return removed, err
		}
		for i := 0; i < len(g.others); i += 500 {
			end := min(i+500, len(g.others))
			ph := strings.TrimSuffix(strings.Repeat("?,", end-i), ",")
			seqs := make([]any, 0, end-i)
			for _, sq := range g.others[i:end] {
				seqs = append(seqs, sq)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE seq IN (`+ph+`)`, seqs...); err != nil {
				tx.Rollback()
				return removed, err
			}
		}
		if err := tx.Commit(); err != nil {
			return removed, err
		}
		removed += len(g.others)
	}
	return removed, nil
}
