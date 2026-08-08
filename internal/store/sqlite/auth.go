package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

// Nothing in this file stores a secret. Every column named `hash` holds the
// output of the hashing in internal/auth, and the plaintext exists only in the
// response that first returned it. A database leak therefore yields nothing
// that can be replayed.

// ---------------------------------------------------------- enrollment tokens

func (s *Store) PutEnrollmentToken(ctx context.Context, t core.EnrollmentToken) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO enrollment_tokens (id, hash, labels, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.Hash, encodeJSON(t.Labels), ms(t.CreatedAt), ms(t.ExpiresAt))
	return err
}

// SpendEnrollmentToken marks a token used and returns it, atomically.
//
// The UPDATE ... WHERE used_at IS NULL is the whole point of the method. Two
// agents pasting the same token at the same moment is precisely the race a
// one-time token exists to lose, and a read-then-write would let both win.
func (s *Store) SpendEnrollmentToken(ctx context.Context, hash, agentID string, now time.Time) (core.EnrollmentToken, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.EnrollmentToken{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		t         core.EnrollmentToken
		labels    string
		created   int64
		expires   int64
		usedAt    sql.NullInt64
		usedBy    string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, labels, created_at, expires_at, used_at, used_by FROM enrollment_tokens WHERE hash = ?`,
		hash).Scan(&t.ID, &labels, &created, &expires, &usedAt, &usedBy)
	if err != nil {
		return core.EnrollmentToken{}, notFound(err, "enrollment token", "")
	}

	decodeJSON(labels, &t.Labels)
	t.Hash = hash
	t.CreatedAt = at(created)
	t.ExpiresAt = at(expires)
	t.UsedAt = atPtr(usedAt)
	t.UsedBy = usedBy

	if t.UsedAt != nil {
		return core.EnrollmentToken{}, fmt.Errorf("%w: this enrollment token was already used, at %s", core.ErrConflict, t.UsedAt.Format(time.RFC3339))
	}
	if now.After(t.ExpiresAt) {
		return core.EnrollmentToken{}, fmt.Errorf("%w: this enrollment token expired at %s", core.ErrUnauthorized, t.ExpiresAt.Format(time.RFC3339))
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE enrollment_tokens SET used_at = ?, used_by = ? WHERE hash = ? AND used_at IS NULL`,
		ms(now), agentID, hash)
	if err != nil {
		return core.EnrollmentToken{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.EnrollmentToken{}, fmt.Errorf("%w: this enrollment token was already used", core.ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return core.EnrollmentToken{}, err
	}

	stamp := now.UTC()
	t.UsedAt = &stamp
	t.UsedBy = agentID
	return t, nil
}

// ---------------------------------------------------------- agent credentials

func (s *Store) PutAgentCredential(ctx context.Context, agentID, hash string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO agent_credentials (agent_id, hash, created_at) VALUES (?, ?, ?)
        ON CONFLICT(agent_id) DO UPDATE SET hash = excluded.hash, created_at = excluded.created_at`,
		agentID, hash, ms(now))
	return err
}

func (s *Store) AgentByCredential(ctx context.Context, hash string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT agent_id FROM agent_credentials WHERE hash = ?`, hash).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", core.ErrUnauthorized
		}
		return "", err
	}
	return id, nil
}

// DeleteAgentCredential revokes an agent. The agent row survives so the console
// can still show what was revoked and when it was last seen.
func (s *Store) DeleteAgentCredential(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_credentials WHERE agent_id = ?`, agentID)
	return err
}

// ------------------------------------------------------------------- API keys

func (s *Store) PutAPIKey(ctx context.Context, k core.APIKey, hash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, hash, scope, created_at) VALUES (?, ?, ?, ?, ?)`,
		k.ID, k.Name, hash, string(k.Scope), ms(k.CreatedAt))
	return err
}

func (s *Store) APIKeyByHash(ctx context.Context, hash string) (core.APIKey, error) {
	var (
		k        core.APIKey
		scope    string
		created  int64
		lastUsed sql.NullInt64
		revoked  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, scope, created_at, last_used_at, revoked_at FROM api_keys WHERE hash = ?`,
		hash).Scan(&k.ID, &k.Name, &scope, &created, &lastUsed, &revoked)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.APIKey{}, core.ErrUnauthorized
		}
		return core.APIKey{}, err
	}
	k.Scope = core.Scope(scope)
	k.CreatedAt = at(created)
	k.LastUsed = atPtr(lastUsed)
	k.RevokedAt = atPtr(revoked)
	if k.RevokedAt != nil {
		return core.APIKey{}, fmt.Errorf("%w: this API key was revoked at %s", core.ErrUnauthorized, k.RevokedAt.Format(time.RFC3339))
	}
	return k, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]core.APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, scope, created_at, last_used_at, revoked_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []core.APIKey{}
	for rows.Next() {
		var (
			k        core.APIKey
			scope    string
			created  int64
			lastUsed sql.NullInt64
			revoked  sql.NullInt64
		)
		if err := rows.Scan(&k.ID, &k.Name, &scope, &created, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		k.Scope = core.Scope(scope)
		k.CreatedAt = at(created)
		k.LastUsed = atPtr(lastUsed)
		k.RevokedAt = atPtr(revoked)
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string, t time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, ms(t), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: API key %s", core.ErrNotFound, id)
	}
	return nil
}

// TouchAPIKey records last use. It is best-effort: failing to record a
// timestamp must never fail the request that was actually authorized.
func (s *Store) TouchAPIKey(ctx context.Context, id string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, ms(t), id)
	return err
}

// ------------------------------------------------------------ console sessions

func (s *Store) PutConsoleSession(ctx context.Context, cs core.ConsoleSession) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO console_sessions (id, hash, username, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		cs.ID, cs.Hash, cs.User, ms(cs.CreatedAt), ms(cs.ExpiresAt))
	return err
}

func (s *Store) ConsoleSessionByHash(ctx context.Context, hash string) (core.ConsoleSession, error) {
	var (
		cs      core.ConsoleSession
		created int64
		expires int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, created_at, expires_at FROM console_sessions WHERE hash = ?`,
		hash).Scan(&cs.ID, &cs.User, &created, &expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.ConsoleSession{}, core.ErrUnauthorized
		}
		return core.ConsoleSession{}, err
	}
	cs.Hash = hash
	cs.CreatedAt = at(created)
	cs.ExpiresAt = at(expires)
	if time.Now().After(cs.ExpiresAt) {
		// Expired rows are deleted lazily, on the read that notices. A sweeper
		// for this would be a goroutine that exists to tidy a table nobody
		// queries by anything but hash.
		_ = s.DeleteConsoleSession(ctx, cs.ID)
		return core.ConsoleSession{}, core.ErrUnauthorized
	}
	return cs, nil
}

func (s *Store) DeleteConsoleSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions WHERE id = ?`, id)
	return err
}
