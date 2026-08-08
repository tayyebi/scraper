package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must upgrade, not recreate: an existing hub's data has to
	// survive a restart.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	var version int
	if err := s2.DB().QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("schema version = %d, want %d", version, len(migrations))
	}
}

func TestAgentRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	want := core.Agent{
		ID:     core.NewID(core.PrefixAgent),
		Name:   "laptop chrome",
		Labels: map[string]string{"env": "prod", "owner": "mo"},
		Capabilities: core.Capabilities{
			Capture:    core.CaptureFetchPatch,
			Screenshot: true,
			Attach:     true,
			Ops:        []string{core.OpNavigate, core.OpClick},
		},
		Status:       core.AgentOnline,
		Browser:      "chrome",
		BrowserVer:   "131.0",
		Platform:     "macOS",
		UserAgent:    "Mozilla/5.0",
		AgentVersion: "0.1.0",
		EnrolledAt:   time.Now().UTC().Truncate(time.Millisecond),
		LastSeenAt:   time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.PutAgent(ctx, want); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}

	got, err := s.GetAgent(ctx, want.ID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Name != want.Name || got.Browser != want.Browser || got.Status != want.Status {
		t.Errorf("scalar fields differ: %+v", got)
	}
	if got.Labels["env"] != "prod" || got.Labels["owner"] != "mo" {
		t.Errorf("labels = %v", got.Labels)
	}
	if got.Capabilities.Capture != core.CaptureFetchPatch || !got.Capabilities.Screenshot {
		t.Errorf("capabilities = %+v", got.Capabilities)
	}
	if !got.Capabilities.Supports(core.OpNavigate) {
		t.Error("ops did not survive the round trip")
	}
	if !got.EnrolledAt.Equal(want.EnrolledAt) {
		t.Errorf("EnrolledAt = %s, want %s", got.EnrolledAt, want.EnrolledAt)
	}
}

func TestGetMissingAgentIsNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetAgent(context.Background(), "a_nope")
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Re-enrolling the same agent must update it rather than fail, and must not
// reset the enrollment time.
func TestPutAgentUpserts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	enrolled := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Millisecond)
	a := core.Agent{ID: "a_1", Name: "first", Status: core.AgentOffline, EnrolledAt: enrolled, LastSeenAt: enrolled}
	if err := s.PutAgent(ctx, a); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}

	a.Name = "renamed"
	a.Status = core.AgentOnline
	a.EnrolledAt = time.Now().UTC()
	a.LastSeenAt = time.Now().UTC().Truncate(time.Millisecond)
	if err := s.PutAgent(ctx, a); err != nil {
		t.Fatalf("PutAgent (update): %v", err)
	}

	got, err := s.GetAgent(ctx, "a_1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Name != "renamed" {
		t.Errorf("Name = %q, want renamed", got.Name)
	}
	if !got.EnrolledAt.Equal(enrolled) {
		t.Errorf("EnrolledAt = %s, want the original %s: re-enrolling must not rewrite history", got.EnrolledAt, enrolled)
	}
}

func TestSessionFiltering(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	sessions := []core.Session{
		{ID: "s_1", AgentID: "a_1", Origin: core.OriginManaged, State: core.SessionOpen, CreatedAt: now},
		{ID: "s_2", AgentID: "a_1", Origin: core.OriginAttached, State: core.SessionOpen, CreatedAt: now.Add(time.Second)},
		{ID: "s_3", AgentID: "a_2", Origin: core.OriginManaged, State: core.SessionClosed, CreatedAt: now.Add(2 * time.Second)},
	}
	for _, sess := range sessions {
		if err := s.PutSession(ctx, sess); err != nil {
			t.Fatalf("PutSession: %v", err)
		}
	}

	cases := []struct {
		name   string
		filter core.SessionFilter
		want   int
	}{
		{"all", core.SessionFilter{}, 3},
		{"by agent", core.SessionFilter{AgentID: "a_1"}, 2},
		{"by state", core.SessionFilter{State: core.SessionOpen}, 2},
		{"by origin", core.SessionFilter{Origin: core.OriginAttached}, 1},
		{"agent and state", core.SessionFilter{AgentID: "a_1", State: core.SessionOpen}, 2},
		{"limit", core.SessionFilter{Limit: 1}, 1},
		{"no match", core.SessionFilter{AgentID: "a_missing"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.ListSessions(ctx, c.filter)
			if err != nil {
				t.Fatalf("ListSessions: %v", err)
			}
			if len(got) != c.want {
				t.Errorf("got %d sessions, want %d", len(got), c.want)
			}
		})
	}
}

// A dropped channel must close the agent's sessions, or they linger looking
// steerable and fail one command timeout at a time.
func TestCloseSessionsForAgent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, sess := range []core.Session{
		{ID: "s_1", AgentID: "a_1", State: core.SessionOpen, Origin: core.OriginManaged, CreatedAt: now},
		{ID: "s_2", AgentID: "a_1", State: core.SessionOpen, Origin: core.OriginAttached, CreatedAt: now},
		{ID: "s_3", AgentID: "a_2", State: core.SessionOpen, Origin: core.OriginManaged, CreatedAt: now},
	} {
		if err := s.PutSession(ctx, sess); err != nil {
			t.Fatalf("PutSession: %v", err)
		}
	}

	if err := s.CloseSessionsForAgent(ctx, "a_1", now); err != nil {
		t.Fatalf("CloseSessionsForAgent: %v", err)
	}

	open, err := s.ListSessions(ctx, core.SessionFilter{State: core.SessionOpen})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(open) != 1 || open[0].ID != "s_3" {
		t.Errorf("open sessions = %v, want only s_3", open)
	}

	closed, err := s.GetSession(ctx, "s_1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if closed.ClosedAt == nil {
		t.Error("closed session has no ClosedAt timestamp")
	}
}

func TestCommandLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	cmd := core.Command{
		ID:         core.NewID(core.PrefixCommand),
		SessionID:  "s_1",
		AgentID:    "a_1",
		Op:         core.OpNavigate,
		Params:     json.RawMessage(`{"url":"https://example.test"}`),
		State:      core.CommandPending,
		CreatedAt:  time.Now().UTC().Truncate(time.Millisecond),
		DeadlineAt: time.Now().Add(30 * time.Second).UTC().Truncate(time.Millisecond),
	}
	if err := s.PutCommand(ctx, cmd); err != nil {
		t.Fatalf("PutCommand: %v", err)
	}

	got, err := s.GetCommand(ctx, cmd.ID)
	if err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if got.State != core.CommandPending || string(got.Params) != `{"url":"https://example.test"}` {
		t.Errorf("round trip = %+v", got)
	}

	done := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.CompleteCommand(ctx, cmd.ID, core.CommandDone, json.RawMessage(`{"ok":true}`), "", done); err != nil {
		t.Fatalf("CompleteCommand: %v", err)
	}

	got, err = s.GetCommand(ctx, cmd.ID)
	if err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if got.State != core.CommandDone {
		t.Errorf("State = %q, want done", got.State)
	}
	if string(got.Result) != `{"ok":true}` {
		t.Errorf("Result = %s", got.Result)
	}
	if got.DoneAt == nil || !got.DoneAt.Equal(done) {
		t.Errorf("DoneAt = %v, want %s", got.DoneAt, done)
	}
}

// A late result and an expired deadline race. Whichever lands first wins, and
// the other must not rewrite it -- a caller that already read "timeout" must
// never see the command change afterwards.
func TestCompleteCommandIsWriteOnce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	cmd := core.Command{
		ID: "c_1", SessionID: "s_1", AgentID: "a_1", Op: core.OpClick,
		State: core.CommandDispatched, CreatedAt: time.Now().UTC(),
	}
	if err := s.PutCommand(ctx, cmd); err != nil {
		t.Fatalf("PutCommand: %v", err)
	}

	now := time.Now().UTC()
	if err := s.CompleteCommand(ctx, "c_1", core.CommandTimeout, nil, "deadline exceeded", now); err != nil {
		t.Fatalf("first CompleteCommand: %v", err)
	}
	if err := s.CompleteCommand(ctx, "c_1", core.CommandDone, json.RawMessage(`{"late":true}`), "", now.Add(time.Second)); err != nil {
		t.Fatalf("second CompleteCommand: %v", err)
	}

	got, err := s.GetCommand(ctx, "c_1")
	if err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if got.State != core.CommandTimeout {
		t.Errorf("State = %q, want timeout: a late result overwrote a recorded terminal state", got.State)
	}
	if got.Result != nil {
		t.Errorf("Result = %s, want nil", got.Result)
	}
}

func TestEventCursorPagination(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Same millisecond on purpose: a timestamp cursor would skip or repeat
	// these, which is why the cursor is the row id.
	same := time.Now().UTC().Truncate(time.Millisecond)
	for i := range 5 {
		if _, err := s.AppendEvent(ctx, core.Event{
			SessionID: "s_1",
			Type:      core.EventMirrorMutation,
			Seq:       int64(i),
			At:        same,
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	first, err := s.ListEvents(ctx, "s_1", 0, 2)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(first) != 2 || first[0].Seq != 0 || first[1].Seq != 1 {
		t.Fatalf("first page = %+v", first)
	}

	second, err := s.ListEvents(ctx, "s_1", first[len(first)-1].ID, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(second) != 3 {
		t.Fatalf("second page has %d events, want 3", len(second))
	}
	if second[0].Seq != 2 {
		t.Errorf("second page starts at seq %d, want 2", second[0].Seq)
	}
}

func TestExchangeRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	x := core.Exchange{
		SessionID:   "s_1",
		AgentID:     "a_1",
		RequestID:   "req-7",
		Method:      "POST",
		URL:         "https://api.example.test/graphql",
		Status:      200,
		StatusText:  "OK",
		MimeType:    "application/json",
		ReqHeaders:  map[string]string{"content-type": "application/json"},
		ResHeaders:  map[string]string{"cache-control": "no-store"},
		ResBody:     "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		ResBodySize: 1234,
		FromCache:   false,
		Truncated:   true,
		StartedAt:   time.Now().UTC().Truncate(time.Millisecond),
		DurationMs:  42,
	}
	if _, err := s.AppendExchange(ctx, x); err != nil {
		t.Fatalf("AppendExchange: %v", err)
	}

	got, err := s.ListExchanges(ctx, "s_1", 0)
	if err != nil {
		t.Fatalf("ListExchanges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d exchanges, want 1", len(got))
	}
	e := got[0]
	if e.Method != "POST" || e.Status != 200 || e.MimeType != "application/json" {
		t.Errorf("scalars differ: %+v", e)
	}
	if e.ReqHeaders["content-type"] != "application/json" {
		t.Errorf("request headers = %v", e.ReqHeaders)
	}
	if e.ResHeaders["cache-control"] != "no-store" {
		t.Errorf("response headers = %v", e.ResHeaders)
	}
	if !e.Truncated {
		t.Error("Truncated did not survive the round trip")
	}
	if e.FromCache {
		t.Error("FromCache did not survive the round trip")
	}
	if !e.StartedAt.Equal(x.StartedAt) {
		t.Errorf("StartedAt = %s, want %s", e.StartedAt, x.StartedAt)
	}
}

// Retention must only surrender digests nothing points at any more, or the
// request log ends up referencing artifacts that were deleted underneath it.
func TestRetentionReturnsOnlyUnreferencedDigests(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	oldTime := time.Now().Add(-48 * time.Hour).UTC()
	newTime := time.Now().UTC()

	shared := "1111111111111111111111111111111111111111111111111111111111111111"
	orphan := "2222222222222222222222222222222222222222222222222222222222222222"

	// An old exchange referencing both digests.
	if _, err := s.AppendExchange(ctx, core.Exchange{
		SessionID: "s_1", URL: "https://old.test", ResBody: orphan, ReqBody: shared, StartedAt: oldTime,
	}); err != nil {
		t.Fatalf("AppendExchange: %v", err)
	}
	// A recent exchange that keeps `shared` alive.
	if _, err := s.AppendExchange(ctx, core.Exchange{
		SessionID: "s_1", URL: "https://new.test", ResBody: shared, StartedAt: newTime,
	}); err != nil {
		t.Fatalf("AppendExchange: %v", err)
	}

	unreferenced, err := s.Retention(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}

	if len(unreferenced) != 1 || unreferenced[0] != orphan {
		t.Fatalf("unreferenced = %v, want exactly [%s]", unreferenced, orphan)
	}
	if !s.Referenced(ctx, shared) {
		t.Error("Referenced = false for a digest a surviving exchange still points at")
	}
	if s.Referenced(ctx, orphan) {
		t.Error("Referenced = true for a digest whose only exchange was deleted")
	}

	remaining, err := s.ListExchanges(ctx, "s_1", 0)
	if err != nil {
		t.Fatalf("ListExchanges: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("%d exchanges survived retention, want 1", len(remaining))
	}
}

// ------------------------------------------------------------------ auth store

// The one-time token is the security property. Two agents racing the same token
// is exactly the case it exists to prevent, so exactly one must win.
func TestEnrollmentTokenIsSpentExactlyOnce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	hash := "hashed-enrollment-token"
	if err := s.PutEnrollmentToken(ctx, core.EnrollmentToken{
		ID:        core.NewID(core.PrefixEnrollment),
		Hash:      hash,
		Labels:    map[string]string{"team": "growth"},
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("PutEnrollmentToken: %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := s.SpendEnrollmentToken(ctx, hash, core.NewID(core.PrefixAgent), time.Now())
			if err == nil {
				mu.Lock()
				winners = append(winners, tok.UsedBy)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("%d agents redeemed the same one-time token, want exactly 1", len(winners))
	}

	// And it stays spent afterwards.
	if _, err := s.SpendEnrollmentToken(ctx, hash, "a_late", time.Now()); !errors.Is(err, core.ErrConflict) {
		t.Errorf("err = %v, want ErrConflict on a spent token", err)
	}
}

func TestExpiredEnrollmentTokenRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	hash := "expired"
	if err := s.PutEnrollmentToken(ctx, core.EnrollmentToken{
		ID: "e_1", Hash: hash,
		CreatedAt: time.Now().Add(-2 * time.Hour).UTC(),
		ExpiresAt: time.Now().Add(-time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("PutEnrollmentToken: %v", err)
	}
	if _, err := s.SpendEnrollmentToken(ctx, hash, "a_1", time.Now()); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestUnknownEnrollmentTokenIsNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.SpendEnrollmentToken(context.Background(), "never minted", "a_1", time.Now())
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAgentCredentialLookupAndRevocation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.PutAgent(ctx, core.Agent{ID: "a_1", Status: core.AgentOffline, EnrolledAt: time.Now(), LastSeenAt: time.Now()}); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}
	if err := s.PutAgentCredential(ctx, "a_1", "cred-hash", time.Now()); err != nil {
		t.Fatalf("PutAgentCredential: %v", err)
	}

	id, err := s.AgentByCredential(ctx, "cred-hash")
	if err != nil {
		t.Fatalf("AgentByCredential: %v", err)
	}
	if id != "a_1" {
		t.Errorf("agent = %q, want a_1", id)
	}

	if err := s.DeleteAgentCredential(ctx, "a_1"); err != nil {
		t.Fatalf("DeleteAgentCredential: %v", err)
	}
	if _, err := s.AgentByCredential(ctx, "cred-hash"); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized after revocation", err)
	}

	// The agent row survives revocation so the console can still show it.
	if _, err := s.GetAgent(ctx, "a_1"); err != nil {
		t.Errorf("agent row disappeared when its credential was revoked: %v", err)
	}
}

// Deleting an agent must take its credential with it, or a revoked device keeps
// a working secret.
func TestDeletingAgentCascadesToCredential(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.PutAgent(ctx, core.Agent{ID: "a_1", Status: core.AgentOffline, EnrolledAt: time.Now(), LastSeenAt: time.Now()}); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}
	if err := s.PutAgentCredential(ctx, "a_1", "cred", time.Now()); err != nil {
		t.Fatalf("PutAgentCredential: %v", err)
	}
	if err := s.DeleteAgent(ctx, "a_1"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if _, err := s.AgentByCredential(ctx, "cred"); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v: the credential outlived the agent it belonged to", err)
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	key := core.APIKey{
		ID:        core.NewID(core.PrefixAPIKey),
		Name:      "ci scraper",
		Scope:     core.ScopeSteer,
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.PutAPIKey(ctx, key, "key-hash"); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}

	got, err := s.APIKeyByHash(ctx, "key-hash")
	if err != nil {
		t.Fatalf("APIKeyByHash: %v", err)
	}
	if got.Scope != core.ScopeSteer || got.Name != "ci scraper" {
		t.Errorf("key = %+v", got)
	}

	used := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.TouchAPIKey(ctx, key.ID, used); err != nil {
		t.Fatalf("TouchAPIKey: %v", err)
	}
	got, err = s.APIKeyByHash(ctx, "key-hash")
	if err != nil {
		t.Fatalf("APIKeyByHash: %v", err)
	}
	if got.LastUsed == nil || !got.LastUsed.Equal(used) {
		t.Errorf("LastUsed = %v, want %s", got.LastUsed, used)
	}

	if err := s.RevokeAPIKey(ctx, key.ID, time.Now()); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, err := s.APIKeyByHash(ctx, "key-hash"); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized for a revoked key", err)
	}

	// A revoked key stays listable: an operator needs to see what was revoked.
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Errorf("ListAPIKeys = %+v, want one revoked key", keys)
	}
}

func TestUnknownAPIKeyIsUnauthorized(t *testing.T) {
	s := newStore(t)
	if _, err := s.APIKeyByHash(context.Background(), "nope"); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestConsoleSessionExpiry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	live := core.ConsoleSession{
		ID: "cs_live", Hash: "live-hash", User: "operator",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
	expired := core.ConsoleSession{
		ID: "cs_dead", Hash: "dead-hash", User: "operator",
		CreatedAt: time.Now().Add(-2 * time.Hour).UTC(), ExpiresAt: time.Now().Add(-time.Hour).UTC(),
	}
	for _, cs := range []core.ConsoleSession{live, expired} {
		if err := s.PutConsoleSession(ctx, cs); err != nil {
			t.Fatalf("PutConsoleSession: %v", err)
		}
	}

	if _, err := s.ConsoleSessionByHash(ctx, "live-hash"); err != nil {
		t.Errorf("live session rejected: %v", err)
	}
	if _, err := s.ConsoleSessionByHash(ctx, "dead-hash"); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized for an expired session", err)
	}
	// Reading an expired session should have swept the row.
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM console_sessions WHERE id = 'cs_dead'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Error("an expired console session row was left behind")
	}

	if err := s.DeleteConsoleSession(ctx, "cs_live"); err != nil {
		t.Fatalf("DeleteConsoleSession: %v", err)
	}
	if _, err := s.ConsoleSessionByHash(ctx, "live-hash"); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized after logout", err)
	}
}

// The store is written to from many goroutines: every agent channel, every
// Control API request. A single writer connection is the mechanism; this
// asserts the outcome.
func TestConcurrentWrites(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.AppendEvent(ctx, core.Event{
				SessionID: "s_1", Type: core.EventConsole, Seq: int64(i), At: time.Now(),
			}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent AppendEvent: %v", err)
	}

	events, err := s.ListEvents(ctx, "s_1", 0, 1000)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 50 {
		t.Errorf("stored %d events, want 50", len(events))
	}
}
