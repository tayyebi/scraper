package auth

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/store/sqlite"
)

func newService(t *testing.T, opts Options) (*Service, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	opts.Store = st
	return New(opts), st
}

// --------------------------------------------------------- RFC 6070 vectors

// PBKDF2 is hand-written to avoid a second module dependency. That is only
// acceptable because it is checked against the published vectors, so these are
// load-bearing rather than decorative.
//
// RFC 6070 specifies vectors for PBKDF2-HMAC-SHA1; the SHA-256 values below are
// the widely published counterparts for the same inputs.
func TestPBKDF2KnownVectors(t *testing.T) {
	cases := []struct {
		password   string
		salt       string
		iterations int
		keyLen     int
		want       string
	}{
		{
			password: "password", salt: "salt", iterations: 1, keyLen: 32,
			want: "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b",
		},
		{
			password: "password", salt: "salt", iterations: 2, keyLen: 32,
			want: "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43",
		},
		{
			password: "password", salt: "salt", iterations: 4096, keyLen: 32,
			want: "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a",
		},
		{
			password: "passwordPASSWORDpassword", salt: "saltSALTsaltSALTsaltSALTsaltSALTsalt",
			iterations: 4096, keyLen: 40,
			want: "348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9",
		},
	}

	for _, c := range cases {
		got := pbkdf2SHA256([]byte(c.password), []byte(c.salt), c.iterations, c.keyLen)
		want, err := hex.DecodeString(c.want)
		if err != nil {
			t.Fatalf("bad test vector: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("pbkdf2(%q, %q, %d, %d) =\n %x\nwant %x",
				c.password, c.salt, c.iterations, c.keyLen, got, want)
		}
	}
}

// A key longer than one hash block exercises the multi-block loop, which is
// where a hand-written PBKDF2 usually goes wrong.
func TestPBKDF2MultiBlockOutput(t *testing.T) {
	long := pbkdf2SHA256([]byte("password"), []byte("salt"), 100, 100)
	if len(long) != 100 {
		t.Fatalf("key length = %d, want 100", len(long))
	}
	first := pbkdf2SHA256([]byte("password"), []byte("salt"), 100, 32)
	if !bytes.Equal(long[:32], first) {
		t.Error("the first block of a long key differs from a short key with the same inputs")
	}
}

// ------------------------------------------------------------------ password

func TestPasswordRoundTrip(t *testing.T) {
	// A low iteration count keeps the test fast; the format is what is under
	// test, not the work factor.
	encoded, err := hashPasswordWith("correct horse battery staple", 1000)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := VerifyPassword(encoded, "correct horse battery staple"); err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
	if err := VerifyPassword(encoded, "wrong"); !errors.Is(err, ErrBadPassword) {
		t.Errorf("err = %v, want ErrBadPassword", err)
	}
}

// The work factor travels with the hash, so it can be raised later without
// invalidating passwords that were hashed under the old one.
func TestPasswordHashCarriesItsParameters(t *testing.T) {
	encoded, err := hashPasswordWith("hunter2", 2048)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		t.Fatalf("encoded form = %q, want four $-separated fields", encoded)
	}
	if parts[0] != "pbkdf2-sha256" {
		t.Errorf("scheme = %q", parts[0])
	}
	if parts[1] != "2048" {
		t.Errorf("iterations = %q, want 2048", parts[1])
	}
	if err := VerifyPassword(encoded, "hunter2"); err != nil {
		t.Errorf("verifying against the recorded iteration count failed: %v", err)
	}
}

func TestPasswordSaltIsPerHash(t *testing.T) {
	a, _ := hashPasswordWith("same password", 1000)
	b, _ := hashPasswordWith("same password", 1000)
	if a == b {
		t.Error("two hashes of the same password are identical, so the salt is not random")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		"pbkdf2-sha256$notanumber$c2FsdA$a2V5",
		"pbkdf2-sha256$0$c2FsdA$a2V5",
		"md5$1000$c2FsdA$a2V5",
		"pbkdf2-sha256$1000$!!!$a2V5",
		"pbkdf2-sha256$1000$c2FsdA",
	} {
		if err := VerifyPassword(bad, "anything"); err == nil {
			t.Errorf("VerifyPassword accepted malformed hash %q", bad)
		}
	}
}

// ---------------------------------------------------------------- enrollment

func TestEnrollmentTokenExchangesForACredential(t *testing.T) {
	s, _ := newService(t, Options{})
	ctx := context.Background()

	token, plain, err := s.MintEnrollment(ctx, map[string]string{"team": "growth"}, 0)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}
	if !strings.HasPrefix(plain, PrefixEnrollment+"_") {
		t.Errorf("token %q lacks its type prefix", plain)
	}
	// The plaintext must never be recoverable from what was stored.
	if strings.Contains(token.Hash, plain) || token.Hash == plain {
		t.Error("the stored record contains the plaintext token")
	}

	agentID := core.NewID(core.PrefixAgent)
	redeemed, err := s.RedeemEnrollment(ctx, plain, agentID)
	if err != nil {
		t.Fatalf("RedeemEnrollment: %v", err)
	}
	credential, err := s.IssueCredential(ctx, agentID)
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if redeemed.Labels["team"] != "growth" {
		t.Errorf("labels did not survive: %v", redeemed.Labels)
	}
	if !strings.HasPrefix(credential, PrefixAgentCred+"_") {
		t.Errorf("credential %q lacks its type prefix", credential)
	}
	if credential == plain {
		t.Fatal("the credential is the enrollment token: the two secrets did not separate")
	}

	got, err := s.AuthenticateAgent(ctx, credential)
	if err != nil {
		t.Fatalf("AuthenticateAgent: %v", err)
	}
	if got != agentID {
		t.Errorf("credential resolved to %q, want %q", got, agentID)
	}
}

// The property the one-time design exists for.
func TestEnrollmentTokenCannotBeReused(t *testing.T) {
	s, _ := newService(t, Options{})
	ctx := context.Background()

	_, plain, err := s.MintEnrollment(ctx, nil, 0)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}
	if _, err := s.RedeemEnrollment(ctx, plain, "a_1"); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if _, err := s.RedeemEnrollment(ctx, plain, "a_2"); err == nil {
		t.Fatal("a one-time enrollment token was redeemed twice")
	}
}

func TestExpiredEnrollmentTokenRejected(t *testing.T) {
	s, _ := newService(t, Options{})
	ctx := context.Background()

	_, plain, err := s.MintEnrollment(ctx, nil, time.Nanosecond)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := s.RedeemEnrollment(ctx, plain, "a_1"); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

// An unknown token and a spent one are different to us but must look the same
// to somebody guessing.
func TestUnknownEnrollmentTokenLooksLikeASpentOne(t *testing.T) {
	s, _ := newService(t, Options{})
	ctx := context.Background()

	_, err := s.RedeemEnrollment(ctx, newSecret(PrefixEnrollment), "a_1")
	if !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
	if errors.Is(err, core.ErrNotFound) {
		t.Error("the error distinguishes an unknown token from a spent one")
	}
}

func TestWrongSecretTypeIsRejectedWithoutALookup(t *testing.T) {
	s, _ := newService(t, Options{})
	ctx := context.Background()

	// An API key pasted into the enrollment field.
	if _, err := s.RedeemEnrollment(ctx, newSecret(PrefixAPIKey), "a_1"); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
	// An enrollment token presented as a channel credential.
	if _, err := s.AuthenticateAgent(ctx, newSecret(PrefixEnrollment)); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
	// An agent credential presented as an API key.
	if _, err := s.AuthenticateAPIKey(ctx, newSecret(PrefixAgentCred)); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestRevokedAgentCredentialStopsWorking(t *testing.T) {
	s, _ := newService(t, Options{})
	ctx := context.Background()

	_, plain, _ := s.MintEnrollment(ctx, nil, 0)
	if _, err := s.RedeemEnrollment(ctx, plain, "a_1"); err != nil {
		t.Fatalf("RedeemEnrollment: %v", err)
	}
	credential, err := s.IssueCredential(ctx, "a_1")
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if _, err := s.AuthenticateAgent(ctx, credential); err != nil {
		t.Fatalf("AuthenticateAgent: %v", err)
	}

	if err := s.RevokeAgent(ctx, "a_1"); err != nil {
		t.Fatalf("RevokeAgent: %v", err)
	}
	if _, err := s.AuthenticateAgent(ctx, credential); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized after revocation", err)
	}
}

// Revoking one device must not touch another, which is the reason credentials
// are per-agent rather than one shared pairing secret.
func TestRevocationIsPerAgent(t *testing.T) {
	s, _ := newService(t, Options{})
	ctx := context.Background()

	credentials := map[string]string{}
	for _, id := range []string{"a_1", "a_2"} {
		_, plain, _ := s.MintEnrollment(ctx, nil, 0)
		if _, err := s.RedeemEnrollment(ctx, plain, id); err != nil {
			t.Fatalf("RedeemEnrollment(%s): %v", id, err)
		}
		cred, err := s.IssueCredential(ctx, id)
		if err != nil {
			t.Fatalf("IssueCredential(%s): %v", id, err)
		}
		credentials[id] = cred
	}

	if err := s.RevokeAgent(ctx, "a_1"); err != nil {
		t.Fatalf("RevokeAgent: %v", err)
	}
	if _, err := s.AuthenticateAgent(ctx, credentials["a_2"]); err != nil {
		t.Errorf("revoking one agent broke another: %v", err)
	}
}

// ------------------------------------------------------------------ API keys

func TestAPIKeyLifecycle(t *testing.T) {
	s, _ := newService(t, Options{})
	ctx := context.Background()

	key, plain, err := s.MintAPIKey(ctx, "ci", core.ScopeSteer)
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
	if !strings.HasPrefix(plain, PrefixAPIKey+"_") {
		t.Errorf("key %q lacks its type prefix", plain)
	}

	got, err := s.AuthenticateAPIKey(ctx, plain)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey: %v", err)
	}
	if got.ID != key.ID || got.Scope != core.ScopeSteer {
		t.Errorf("resolved to %+v", got)
	}

	// Using a key records that it was used, which is how an operator spots one
	// that should be revoked.
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].LastUsed == nil {
		t.Errorf("last-used was not recorded: %+v", keys)
	}

	if err := s.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, err := s.AuthenticateAPIKey(ctx, plain); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized for a revoked key", err)
	}
}

func TestMintAPIKeyRejectsUnknownScope(t *testing.T) {
	s, _ := newService(t, Options{})
	if _, _, err := s.MintAPIKey(context.Background(), "bad", core.Scope("root")); !errors.Is(err, core.ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
}

func TestAuthorize(t *testing.T) {
	cases := []struct {
		have    core.Scope
		want    core.Scope
		allowed bool
	}{
		{core.ScopeRead, core.ScopeRead, true},
		{core.ScopeRead, core.ScopeSteer, false},
		{core.ScopeRead, core.ScopeAdmin, false},
		{core.ScopeSteer, core.ScopeRead, true},
		{core.ScopeSteer, core.ScopeAdmin, false},
		{core.ScopeAdmin, core.ScopeAdmin, true},
	}
	for _, c := range cases {
		err := Authorize(core.APIKey{Scope: c.have}, c.want)
		if c.allowed && err != nil {
			t.Errorf("%s should grant %s: %v", c.have, c.want, err)
		}
		if !c.allowed {
			if err == nil {
				t.Errorf("%s must not grant %s", c.have, c.want)
			} else if !errors.Is(err, core.ErrForbidden) {
				t.Errorf("err = %v, want ErrForbidden", err)
			}
		}
	}
}

// ------------------------------------------------------------------- console

func consoleOpts(t *testing.T) Options {
	t.Helper()
	hash, err := hashPasswordWith("s3cret", 1000)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return Options{ConsoleUser: "operator", ConsolePasswordHash: hash}
}

func TestConsoleLoginAndLogout(t *testing.T) {
	s, _ := newService(t, consoleOpts(t))
	ctx := context.Background()

	if !s.ConsoleEnabled() {
		t.Fatal("console reported disabled despite configured credentials")
	}

	session, cookie, err := s.Login(ctx, "operator", "s3cret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !strings.HasPrefix(cookie, PrefixConsole+"_") {
		t.Errorf("cookie %q lacks its type prefix", cookie)
	}
	if session.Hash == cookie {
		t.Error("the stored session hash is the cookie value")
	}

	got, err := s.AuthenticateConsole(ctx, cookie)
	if err != nil {
		t.Fatalf("AuthenticateConsole: %v", err)
	}
	if got.User != "operator" {
		t.Errorf("user = %q", got.User)
	}

	if err := s.Logout(ctx, cookie); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := s.AuthenticateConsole(ctx, cookie); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized after logout", err)
	}
	// Logging out twice is the desired end state, not an error.
	if err := s.Logout(ctx, cookie); err != nil {
		t.Errorf("second Logout: %v", err)
	}
}

func TestConsoleLoginRejectsBadCredentials(t *testing.T) {
	s, _ := newService(t, consoleOpts(t))
	ctx := context.Background()

	for _, c := range []struct{ user, pass string }{
		{"operator", "wrong"},
		{"someone-else", "s3cret"},
		{"", ""},
	} {
		if _, _, err := s.Login(ctx, c.user, c.pass); !errors.Is(err, core.ErrUnauthorized) {
			t.Errorf("Login(%q, %q) err = %v, want ErrUnauthorized", c.user, c.pass, err)
		}
	}
}

// An unconfigured console must refuse logins rather than accept anything.
func TestConsoleDisabledWhenUnconfigured(t *testing.T) {
	s, _ := newService(t, Options{})
	if s.ConsoleEnabled() {
		t.Fatal("console reported enabled with no credentials configured")
	}
	if _, _, err := s.Login(context.Background(), "", ""); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer hub_abc":   "hub_abc",
		"bearer hub_abc":   "hub_abc",
		"BEARER  hub_abc ": "hub_abc",
		"hub_abc":          "",
		"Basic hub_abc":    "",
		"":                 "",
		"Bearer":           "",
	}
	for header, want := range cases {
		if got := BearerToken(header); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestSecretsAreUnique(t *testing.T) {
	seen := make(map[string]bool, 500)
	for range 500 {
		s := newSecret(PrefixAPIKey)
		if seen[s] {
			t.Fatal("newSecret repeated a value")
		}
		seen[s] = true
	}
}

func TestHashSecretIsStableAndOpaque(t *testing.T) {
	secret := newSecret(PrefixAPIKey)
	a, b := hashSecret(secret), hashSecret(secret)
	if a != b {
		t.Error("hashSecret is not deterministic, so a stored hash could never be matched")
	}
	if strings.Contains(a, secret) {
		t.Error("the hash contains the plaintext")
	}
	if len(a) != 64 {
		t.Errorf("hash length = %d, want 64 hex characters", len(a))
	}
}

