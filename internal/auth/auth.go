package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

// Defaults chosen to make the common path safe rather than convenient.
const (
	// DefaultEnrollmentTTL is short because the token's whole job is to survive
	// the trip from an operator's clipboard to an extension's settings field.
	DefaultEnrollmentTTL = 15 * time.Minute

	// DefaultConsoleSessionTTL bounds a console login.
	DefaultConsoleSessionTTL = 12 * time.Hour
)

// Service issues and verifies credentials.
type Service struct {
	store core.AuthStore

	enrollmentTTL time.Duration
	consoleTTL    time.Duration

	// consoleUser and consolePasswordHash are the single operator login. A hub
	// is a personal or small-team tool; a user table would be ceremony around a
	// row count of one.
	consoleUser         string
	consolePasswordHash string
}

// Options configures a Service.
type Options struct {
	Store               core.AuthStore
	EnrollmentTTL       time.Duration
	ConsoleSessionTTL   time.Duration
	ConsoleUser         string
	ConsolePasswordHash string
}

// New builds a Service.
func New(opts Options) *Service {
	s := &Service{
		store:               opts.Store,
		enrollmentTTL:       opts.EnrollmentTTL,
		consoleTTL:          opts.ConsoleSessionTTL,
		consoleUser:         opts.ConsoleUser,
		consolePasswordHash: opts.ConsolePasswordHash,
	}
	if s.enrollmentTTL <= 0 {
		s.enrollmentTTL = DefaultEnrollmentTTL
	}
	if s.consoleTTL <= 0 {
		s.consoleTTL = DefaultConsoleSessionTTL
	}
	return s
}

// --------------------------------------------------------------- enrollment

// MintEnrollment issues a one-time enrollment token.
//
// The plaintext is returned once and never stored. An operator who loses it
// mints another; there is no recovery path, because a recoverable one-time
// secret is not one.
func (s *Service) MintEnrollment(ctx context.Context, labels map[string]string, ttl time.Duration) (core.EnrollmentToken, string, error) {
	if ttl <= 0 {
		ttl = s.enrollmentTTL
	}
	secret := newSecret(PrefixEnrollment)
	now := time.Now().UTC()

	token := core.EnrollmentToken{
		ID:        core.NewID(core.PrefixEnrollment),
		Hash:      hashSecret(secret),
		Labels:    labels,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	if err := s.store.PutEnrollmentToken(ctx, token); err != nil {
		return core.EnrollmentToken{}, "", err
	}
	return token, secret, nil
}

// RedeemEnrollment spends a one-time token on behalf of agentID.
//
// It deliberately does not issue the credential. The credential belongs to an
// agent record, and that record does not exist yet at this point -- the caller
// creates it only once redemption has succeeded, so a stream of bad tokens
// cannot litter the database with agents that never enrolled. IssueCredential
// is the second half.
func (s *Service) RedeemEnrollment(ctx context.Context, tokenPlain, agentID string) (core.EnrollmentToken, error) {
	tokenPlain = strings.TrimSpace(tokenPlain)
	if !hasPrefix(tokenPlain, PrefixEnrollment) {
		return core.EnrollmentToken{}, fmt.Errorf("%w: that does not look like an enrollment token (they start with %s_)", core.ErrUnauthorized, PrefixEnrollment)
	}

	now := time.Now().UTC()
	token, err := s.store.SpendEnrollmentToken(ctx, hashSecret(tokenPlain), agentID, now)
	if err != nil {
		// A token that does not exist and a token that was already spent are
		// different to us but must look the same to a caller guessing tokens.
		if errors.Is(err, core.ErrNotFound) {
			return core.EnrollmentToken{}, fmt.Errorf("%w: unknown or already-used enrollment token", core.ErrUnauthorized)
		}
		return core.EnrollmentToken{}, err
	}
	return token, nil
}

// IssueCredential mints an agent's long-lived channel credential.
//
// This is the moment the two secrets separate. From here the agent
// authenticates with something only it holds, so the enrollment token can leak
// without granting anything, and this specific device can be revoked without
// touching any other.
//
// The plaintext is returned once and never stored.
func (s *Service) IssueCredential(ctx context.Context, agentID string) (string, error) {
	credential := newSecret(PrefixAgentCred)
	if err := s.store.PutAgentCredential(ctx, agentID, hashSecret(credential), time.Now().UTC()); err != nil {
		return "", err
	}
	return credential, nil
}

// AuthenticateAgent resolves a channel credential to an agent id.
func (s *Service) AuthenticateAgent(ctx context.Context, credential string) (string, error) {
	credential = strings.TrimSpace(credential)
	if !hasPrefix(credential, PrefixAgentCred) {
		return "", core.ErrUnauthorized
	}
	return s.store.AgentByCredential(ctx, hashSecret(credential))
}

// RevokeAgent destroys an agent's credential.
func (s *Service) RevokeAgent(ctx context.Context, agentID string) error {
	return s.store.DeleteAgentCredential(ctx, agentID)
}

// ----------------------------------------------------------------- API keys

// MintAPIKey issues a Control API key. The plaintext is shown once.
func (s *Service) MintAPIKey(ctx context.Context, name string, scope core.Scope) (core.APIKey, string, error) {
	switch scope {
	case core.ScopeRead, core.ScopeSteer, core.ScopeAdmin:
	default:
		return core.APIKey{}, "", fmt.Errorf("%w: scope must be read, steer or admin", core.ErrBadRequest)
	}

	secret := newSecret(PrefixAPIKey)
	key := core.APIKey{
		ID:        core.NewID(core.PrefixAPIKey),
		Name:      name,
		Scope:     scope,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.PutAPIKey(ctx, key, hashSecret(secret)); err != nil {
		return core.APIKey{}, "", err
	}
	return key, secret, nil
}

// AuthenticateAPIKey resolves a bearer token to a key, and records its use.
func (s *Service) AuthenticateAPIKey(ctx context.Context, presented string) (core.APIKey, error) {
	presented = strings.TrimSpace(presented)
	if !hasPrefix(presented, PrefixAPIKey) {
		return core.APIKey{}, core.ErrUnauthorized
	}

	key, err := s.store.APIKeyByHash(ctx, hashSecret(presented))
	if err != nil {
		return core.APIKey{}, err
	}

	// Best effort: failing to record a timestamp must not fail a request that
	// was legitimately authorized.
	_ = s.store.TouchAPIKey(ctx, key.ID, time.Now().UTC())
	return key, nil
}

// ListAPIKeys returns every key, including revoked ones -- an operator needs to
// see what was revoked as much as what is live.
func (s *Service) ListAPIKeys(ctx context.Context) ([]core.APIKey, error) {
	return s.store.ListAPIKeys(ctx)
}

// RevokeAPIKey revokes a key by id.
func (s *Service) RevokeAPIKey(ctx context.Context, id string) error {
	return s.store.RevokeAPIKey(ctx, id, time.Now().UTC())
}

// Authorize checks that a key carries at least the required scope.
func Authorize(key core.APIKey, required core.Scope) error {
	if !key.Scope.Implies(required) {
		return fmt.Errorf("%w: this key has scope %q, which does not grant %q", core.ErrForbidden, key.Scope, required)
	}
	return nil
}

// ------------------------------------------------------------------ console

// ConsoleEnabled reports whether a console login has been configured.
func (s *Service) ConsoleEnabled() bool {
	return s.consoleUser != "" && s.consolePasswordHash != ""
}

// Login verifies operator credentials and opens a console session.
//
// The returned string is the cookie value; only its hash is stored, so the
// session table is useless to anyone who reads it.
func (s *Service) Login(ctx context.Context, user, password string) (core.ConsoleSession, string, error) {
	if !s.ConsoleEnabled() {
		return core.ConsoleSession{}, "", fmt.Errorf("%w: no console login is configured on this hub", core.ErrForbidden)
	}

	// Verify the password even when the username is wrong, so the response time
	// does not reveal which half was incorrect.
	userOK := secretsEqual(user, s.consoleUser)
	passErr := VerifyPassword(s.consolePasswordHash, password)
	if !userOK || passErr != nil {
		return core.ConsoleSession{}, "", fmt.Errorf("%w: incorrect username or password", core.ErrUnauthorized)
	}

	secret := newSecret(PrefixConsole)
	now := time.Now().UTC()
	session := core.ConsoleSession{
		ID:        core.NewID("cs"),
		Hash:      hashSecret(secret),
		User:      user,
		CreatedAt: now,
		ExpiresAt: now.Add(s.consoleTTL),
	}
	if err := s.store.PutConsoleSession(ctx, session); err != nil {
		return core.ConsoleSession{}, "", err
	}
	return session, secret, nil
}

// AuthenticateConsole resolves a session cookie.
func (s *Service) AuthenticateConsole(ctx context.Context, cookie string) (core.ConsoleSession, error) {
	cookie = strings.TrimSpace(cookie)
	if !hasPrefix(cookie, PrefixConsole) {
		return core.ConsoleSession{}, core.ErrUnauthorized
	}
	return s.store.ConsoleSessionByHash(ctx, hashSecret(cookie))
}

// Logout ends a console session.
func (s *Service) Logout(ctx context.Context, cookie string) error {
	session, err := s.AuthenticateConsole(ctx, cookie)
	if err != nil {
		// Logging out of a session that is already gone is the desired end
		// state, not an error.
		return nil
	}
	return s.store.DeleteConsoleSession(ctx, session.ID)
}

// ConsoleSessionTTL reports how long a console session lasts, so the cookie's
// Max-Age can match the server's view of it.
func (s *Service) ConsoleSessionTTL() time.Duration { return s.consoleTTL }

// -------------------------------------------------------------------- utils

// BearerToken extracts a token from an Authorization header value.
func BearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
