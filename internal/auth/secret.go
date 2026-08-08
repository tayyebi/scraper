// Package auth mints and verifies every credential in the hub.
//
// Three kinds of secret, three different threat models:
//
//   - Enrollment tokens are one-time and short-lived. They are pasted between
//     applications by humans, so they are the most likely to leak, and they are
//     worth nothing once spent.
//   - Agent credentials and API keys are long-lived and machine-held. They are
//     256 bits of randomness, so they are hashed with a plain SHA-256 rather
//     than a password KDF -- see hashSecret for why that is the correct call
//     and not a shortcut.
//   - Console passwords are chosen by humans and therefore guessable, so they
//     get PBKDF2 with a real work factor. See password.go.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Secret prefixes. A prefix makes a leaked secret identifiable on sight -- in a
// log, a bug report, or a public repository -- which is what lets scanners find
// them and lets a human know what they just pasted.
const (
	PrefixEnrollment = "enr"
	PrefixAgentCred  = "agt"
	PrefixAPIKey     = "hub"
	PrefixConsole    = "ses"
)

// secretBytes is the entropy in every machine-held secret.
//
// 256 bits is chosen so that hashSecret's reasoning holds: no offline attack
// against the stored hash is better than guessing, at any conceivable scale.
const secretBytes = 32

var secretEnc = base64.RawURLEncoding

// newSecret returns a fresh secret with a type prefix.
func newSecret(prefix string) string {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		// No sane fallback: a predictable credential is a security hole, not a
		// degraded mode.
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + secretEnc.EncodeToString(b)
}

// hashSecret is what gets stored. The plaintext exists only in the response
// that first returned it.
//
// SHA-256 with no salt and no work factor is deliberate here, and it is not the
// same mistake as hashing a password this way. A password KDF exists to make
// guessing expensive, and guessing is only a threat when the secret comes from
// a small space. These secrets are 256 uniformly random bits; there is nothing
// to guess. Salting would also break the lookup: credentials are found *by*
// their hash, and a per-row salt would force a table scan and a comparison
// against every row on every authenticated request.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// hasPrefix reports whether a presented secret is even the right kind, so a
// pasted API key is rejected as an enrollment token before it is hashed and
// looked up.
func hasPrefix(secret, prefix string) bool {
	return strings.HasPrefix(secret, prefix+"_")
}

// secretsEqual compares in constant time. Used where a comparison is against a
// value rather than a lookup, since a byte-at-a-time comparison leaks the
// position of the first difference through timing.
func secretsEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
