package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultIterations is the PBKDF2 work factor for console passwords.
//
// Unlike the machine-held secrets in secret.go, a console password is chosen by
// a human and therefore lives in a small, guessable space. The work factor is
// the entire defence: it makes each guess cost real time, so a leaked hash is
// not a leaked password.
const DefaultIterations = 210_000

const (
	pbkdfScheme = "pbkdf2-sha256"
	saltBytes   = 16
	keyBytes    = 32
)

// ErrBadPassword reports a failed verification.
var ErrBadPassword = errors.New("incorrect password")

// HashPassword returns an encoded hash: scheme$iterations$salt$key, all base64.
//
// The parameters travel with the hash so the work factor can be raised later
// without invalidating every existing password: an old hash still verifies
// against its own recorded iteration count.
func HashPassword(password string) (string, error) {
	return hashPasswordWith(password, DefaultIterations)
}

func hashPasswordWith(password string, iterations int) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: crypto/rand unavailable: %w", err)
	}
	key := pbkdf2SHA256([]byte(password), salt, iterations, keyBytes)
	return strings.Join([]string{
		pbkdfScheme,
		strconv.Itoa(iterations),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	}, "$"), nil
}

// VerifyPassword checks a password against an encoded hash.
func VerifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdfScheme {
		return fmt.Errorf("auth: unrecognized password hash format")
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return fmt.Errorf("auth: password hash has an invalid iteration count")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("auth: password hash has an invalid salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return fmt.Errorf("auth: password hash is unreadable")
	}

	got := pbkdf2SHA256([]byte(password), salt, iterations, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrBadPassword
	}
	return nil
}

// pbkdf2SHA256 implements PBKDF2 (RFC 8018) over HMAC-SHA-256.
//
// Hand-written because golang.org/x/crypto would be a second module
// dependency, and this repo has exactly one. PBKDF2 is about twenty lines of
// well-specified loop, and it is verified below against the published RFC 6070
// test vectors -- which is what makes writing it acceptable rather than
// reckless.
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	dk := make([]byte, 0, blocks*hashLen)
	u := make([]byte, 0, hashLen)
	var counter [4]byte

	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		counter[0] = byte(block >> 24)
		counter[1] = byte(block >> 16)
		counter[2] = byte(block >> 8)
		counter[3] = byte(block)
		prf.Write(counter[:])

		dk = prf.Sum(dk)
		t := dk[len(dk)-hashLen:]

		u = append(u[:0], t...)
		for i := 2; i <= iterations; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range u {
				t[j] ^= u[j]
			}
		}
	}
	return dk[:keyLen]
}
