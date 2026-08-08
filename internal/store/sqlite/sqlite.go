// Package sqlite implements the metadata store.
//
// SQLite because the hub is a single binary that people will run on a laptop or
// a small VM, and a separate database server would be the largest operational
// cost in a system whose entire point is being easy to stand up. modernc.org's
// pure-Go driver keeps CGO off, so the binary stays static.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/tayyebi/scraper/internal/core"

	_ "modernc.org/sqlite"
)

// Store implements core.Store and core.AuthStore.
type Store struct {
	db *sql.DB
}

// Open prepares the database at path, applying any pending migrations.
func Open(path string) (*Store, error) {
	// Pragmas go through the DSN because they must apply to every connection in
	// the pool, and a pool connection opened later would otherwise miss them.
	//
	//   WAL          readers do not block the writer, which matters because SSE
	//                and the request log are read while agents are writing.
	//   busy_timeout retry instead of failing instantly on a held lock.
	//   foreign_keys SQLite disables them per-connection by default.
	//   synchronous  NORMAL is the right trade under WAL: durable across process
	//                crashes, which is the failure we actually have.
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// One connection, so all writes serialize in Go rather than colliding in
	// SQLite and producing SQLITE_BUSY under load. The hub's query volume is
	// small and its writes are frequent, which is exactly the shape where a
	// single writer beats a pool.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version    INTEGER PRIMARY KEY,
        applied_at INTEGER NOT NULL
    )`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		version := i + 1
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recording migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", version, err)
		}
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests and for the console's storage panel.
func (s *Store) DB() *sql.DB { return s.db }

// ------------------------------------------------------- time and JSON helpers

// Times are stored as Unix milliseconds. Integers sort and range-scan without a
// collation, and milliseconds match the precision of every timestamp an agent
// can produce from JavaScript, so nothing is rounded on the way in.
func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func msPtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func at(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func atPtr(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 == 0 {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func encodeJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeJSON(s string, dst any) {
	if s == "" {
		return
	}
	_ = json.Unmarshal([]byte(s), dst)
}

func rawOrNil(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

func rawFrom(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "" {
		return nil
	}
	return json.RawMessage(s.String)
}

// notFound wraps a sql.ErrNoRows into the domain vocabulary, so adapters map one
// error to 404 rather than each knowing about database/sql.
func notFound(err error, what, id string) error {
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id == "" {
		return fmt.Errorf("%w: %s", core.ErrNotFound, what)
	}
	return fmt.Errorf("%w: %s %s", core.ErrNotFound, what, id)
}

// --------------------------------------------------------------------- agents

func (s *Store) PutAgent(ctx context.Context, a core.Agent) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO agents (id, name, labels, capabilities, status, browser, browser_version,
                            platform, user_agent, agent_version, enrolled_at, last_seen_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            name            = excluded.name,
            labels          = excluded.labels,
            capabilities    = excluded.capabilities,
            status          = excluded.status,
            browser         = excluded.browser,
            browser_version = excluded.browser_version,
            platform        = excluded.platform,
            user_agent      = excluded.user_agent,
            agent_version   = excluded.agent_version,
            last_seen_at    = excluded.last_seen_at`,
		a.ID, a.Name, encodeJSON(a.Labels), encodeJSON(a.Capabilities), string(a.Status),
		a.Browser, a.BrowserVer, a.Platform, a.UserAgent, a.AgentVersion,
		ms(a.EnrolledAt), ms(a.LastSeenAt))
	return err
}

const agentColumns = `id, name, labels, capabilities, status, browser, browser_version,
                      platform, user_agent, agent_version, enrolled_at, last_seen_at`

func scanAgent(sc interface{ Scan(...any) error }) (core.Agent, error) {
	var (
		a            core.Agent
		labels, caps string
		status       string
		enrolled     int64
		lastSeen     int64
	)
	err := sc.Scan(&a.ID, &a.Name, &labels, &caps, &status, &a.Browser, &a.BrowserVer,
		&a.Platform, &a.UserAgent, &a.AgentVersion, &enrolled, &lastSeen)
	if err != nil {
		return a, err
	}
	decodeJSON(labels, &a.Labels)
	decodeJSON(caps, &a.Capabilities)
	a.Status = core.AgentStatus(status)
	a.EnrolledAt = at(enrolled)
	a.LastSeenAt = at(lastSeen)
	return a, nil
}

func (s *Store) GetAgent(ctx context.Context, id string) (core.Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = ?`, id)
	a, err := scanAgent(row)
	if err != nil {
		return core.Agent{}, notFound(err, "agent", id)
	}
	return a, nil
}

func (s *Store) ListAgents(ctx context.Context) ([]core.Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentColumns+` FROM agents ORDER BY status = 'online' DESC, last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agents := []core.Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (s *Store) SetAgentStatus(ctx context.Context, id string, status core.AgentStatus, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET status = ?, last_seen_at = ? WHERE id = ?`, string(status), ms(t), id)
	return err
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: agent %s", core.ErrNotFound, id)
	}
	return nil
}

// ------------------------------------------------------------------- sessions

func (s *Store) PutSession(ctx context.Context, sess core.Session) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO sessions (id, agent_id, tab_id, url, title, origin, state, created_at, closed_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            tab_id    = excluded.tab_id,
            url       = excluded.url,
            title     = excluded.title,
            state     = excluded.state,
            closed_at = excluded.closed_at`,
		sess.ID, sess.AgentID, sess.TabID, sess.URL, sess.Title,
		string(sess.Origin), string(sess.State), ms(sess.CreatedAt), msPtr(sess.ClosedAt))
	return err
}

const sessionColumns = `id, agent_id, tab_id, url, title, origin, state, created_at, closed_at`

func scanSession(sc interface{ Scan(...any) error }) (core.Session, error) {
	var (
		s              core.Session
		origin, state  string
		created        int64
		closed         sql.NullInt64
	)
	err := sc.Scan(&s.ID, &s.AgentID, &s.TabID, &s.URL, &s.Title, &origin, &state, &created, &closed)
	if err != nil {
		return s, err
	}
	s.Origin = core.SessionOrigin(origin)
	s.State = core.SessionState(state)
	s.CreatedAt = at(created)
	s.ClosedAt = atPtr(closed)
	return s, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (core.Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if err != nil {
		return core.Session{}, notFound(err, "session", id)
	}
	return sess, nil
}

func (s *Store) ListSessions(ctx context.Context, f core.SessionFilter) ([]core.Session, error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE 1 = 1`
	var args []any
	if f.AgentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, f.AgentID)
	}
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, string(f.State))
	}
	if f.Origin != "" {
		q += ` AND origin = ?`
		args = append(args, string(f.Origin))
	}
	q += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []core.Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// CloseSessionsForAgent marks every open session closed when an agent's channel
// drops. Without this a disconnected agent leaves sessions that look steerable
// and time out one command at a time.
func (s *Store) CloseSessionsForAgent(ctx context.Context, agentID string, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, closed_at = ? WHERE agent_id = ? AND state = ?`,
		string(core.SessionClosed), ms(t), agentID, string(core.SessionOpen))
	return err
}

// ------------------------------------------------------------------- commands

func (s *Store) PutCommand(ctx context.Context, c core.Command) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO commands (id, session_id, agent_id, op, params, state, result, error,
                              created_at, deadline_at, done_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            state   = excluded.state,
            result  = excluded.result,
            error   = excluded.error,
            done_at = excluded.done_at`,
		c.ID, c.SessionID, c.AgentID, c.Op, rawOrNil(c.Params), string(c.State),
		rawOrNil(c.Result), c.Error, ms(c.CreatedAt), ms(c.DeadlineAt), msPtr(c.DoneAt))
	return err
}

const commandColumns = `id, session_id, agent_id, op, params, state, result, error,
                        created_at, deadline_at, done_at`

func scanCommand(sc interface{ Scan(...any) error }) (core.Command, error) {
	var (
		c              core.Command
		params, result sql.NullString
		state          string
		created        int64
		deadline       int64
		done           sql.NullInt64
	)
	err := sc.Scan(&c.ID, &c.SessionID, &c.AgentID, &c.Op, &params, &state, &result,
		&c.Error, &created, &deadline, &done)
	if err != nil {
		return c, err
	}
	c.Params = rawFrom(params)
	c.Result = rawFrom(result)
	c.State = core.CommandState(state)
	c.CreatedAt = at(created)
	c.DeadlineAt = at(deadline)
	c.DoneAt = atPtr(done)
	return c, nil
}

func (s *Store) GetCommand(ctx context.Context, id string) (core.Command, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM commands WHERE id = ?`, id)
	c, err := scanCommand(row)
	if err != nil {
		return core.Command{}, notFound(err, "command", id)
	}
	return c, nil
}

// CompleteCommand records a terminal state, but only for a command that is not
// already terminal.
//
// The guard matters: a result and a deadline can race, and without it a late
// answer would overwrite a recorded timeout, so a caller that already read
// "timeout" would see the command change state afterwards.
func (s *Store) CompleteCommand(ctx context.Context, id string, state core.CommandState, result json.RawMessage, errMsg string, t time.Time) error {
	res, err := s.db.ExecContext(ctx, `
        UPDATE commands
           SET state = ?, result = ?, error = ?, done_at = ?
         WHERE id = ? AND state NOT IN ('done', 'error', 'timeout')`,
		string(state), rawOrNil(result), errMsg, ms(t), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the command does not exist or it already finished. Both are
		// benign for the caller, which is racing by nature.
		return nil
	}
	return nil
}

// --------------------------------------------------------------------- events

func (s *Store) AppendEvent(ctx context.Context, e core.Event) (core.Event, error) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (agent_id, session_id, type, seq, body, at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.AgentID, e.SessionID, e.Type, e.Seq, rawOrNil(e.Body), ms(e.At))
	if err != nil {
		return e, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return e, err
	}
	e.ID = id
	return e, nil
}

// ListEvents returns events after afterID, oldest first. The cursor is the row
// id rather than a timestamp because two events can share a millisecond, and a
// timestamp cursor would either skip or repeat them.
func (s *Store) ListEvents(ctx context.Context, sessionID string, afterID int64, limit int) ([]core.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT id, agent_id, session_id, type, seq, body, at FROM events WHERE id > ?`
	args := []any{afterID}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []core.Event{}
	for rows.Next() {
		var (
			e    core.Event
			body sql.NullString
			when int64
		)
		if err := rows.Scan(&e.ID, &e.AgentID, &e.SessionID, &e.Type, &e.Seq, &body, &when); err != nil {
			return nil, err
		}
		e.Body = rawFrom(body)
		e.At = at(when)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- request log

func (s *Store) AppendExchange(ctx context.Context, x core.Exchange) (core.Exchange, error) {
	if x.StartedAt.IsZero() {
		x.StartedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
        INSERT INTO exchanges (session_id, agent_id, request_id, method, url, status, status_text,
                               mime_type, resource_type, initiator, req_headers, res_headers,
                               req_body_digest, res_body_digest, req_body_size, res_body_size,
                               from_cache, truncated, started_at, duration_ms)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		x.SessionID, x.AgentID, x.RequestID, x.Method, x.URL, x.Status, x.StatusText,
		x.MimeType, x.ResourceType, x.Initiator, encodeJSON(x.ReqHeaders), encodeJSON(x.ResHeaders),
		x.ReqBody, x.ResBody, x.ReqBodySize, x.ResBodySize,
		x.FromCache, x.Truncated, ms(x.StartedAt), x.DurationMs)
	if err != nil {
		return x, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return x, err
	}
	x.ID = id
	return x, nil
}

func (s *Store) ListExchanges(ctx context.Context, sessionID string, limit int) ([]core.Exchange, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, session_id, agent_id, request_id, method, url, status, status_text,
               mime_type, resource_type, initiator, req_headers, res_headers,
               req_body_digest, res_body_digest, req_body_size, res_body_size,
               from_cache, truncated, started_at, duration_ms
          FROM exchanges
         WHERE session_id = ?
         ORDER BY id ASC
         LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []core.Exchange{}
	for rows.Next() {
		var (
			x                    core.Exchange
			reqHeaders           sql.NullString
			resHeaders           sql.NullString
			started              int64
		)
		if err := rows.Scan(&x.ID, &x.SessionID, &x.AgentID, &x.RequestID, &x.Method, &x.URL,
			&x.Status, &x.StatusText, &x.MimeType, &x.ResourceType, &x.Initiator,
			&reqHeaders, &resHeaders, &x.ReqBody, &x.ResBody, &x.ReqBodySize, &x.ResBodySize,
			&x.FromCache, &x.Truncated, &started, &x.DurationMs); err != nil {
			return nil, err
		}
		if reqHeaders.Valid {
			decodeJSON(reqHeaders.String, &x.ReqHeaders)
		}
		if resHeaders.Valid {
			decodeJSON(resHeaders.String, &x.ResHeaders)
		}
		x.StartedAt = at(started)
		out = append(out, x)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ retention

// Retention deletes events and exchanges older than before, and returns the
// digests that no surviving exchange references any more.
//
// It returns candidates rather than deleting blobs itself: the blob store owns
// its files, and a store that reached across the boundary would make the two
// halves impossible to test apart.
func (s *Store) Retention(ctx context.Context, before time.Time) ([]string, error) {
	cutoff := ms(before)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Collect the digests referenced by the rows about to disappear, so the
	// caller only has to check those rather than rescanning the whole store.
	rows, err := tx.QueryContext(ctx, `
        SELECT DISTINCT digest FROM (
            SELECT req_body_digest AS digest FROM exchanges WHERE started_at < ? AND req_body_digest <> ''
            UNION
            SELECT res_body_digest AS digest FROM exchanges WHERE started_at < ? AND res_body_digest <> ''
        )`, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	if _, err := tx.ExecContext(ctx, `DELETE FROM exchanges WHERE started_at < ?`, cutoff); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE at < ?`, cutoff); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commands WHERE created_at < ?`, cutoff); err != nil {
		return nil, err
	}

	// Of the candidates, keep only those nothing references any more.
	unreferenced := []string{}
	for _, d := range candidates {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM exchanges WHERE req_body_digest = ? OR res_body_digest = ?`,
			d, d).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			unreferenced = append(unreferenced, d)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return unreferenced, nil
}

// Referenced reports whether any exchange still points at a digest. The blob
// store's sweep uses it as its keep predicate.
func (s *Store) Referenced(ctx context.Context, digest string) bool {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM exchanges WHERE req_body_digest = ? OR res_body_digest = ?`,
		digest, digest).Scan(&n)
	return err == nil && n > 0
}
