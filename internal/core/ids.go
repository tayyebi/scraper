package core

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

// idEnc is Crockford base32. Two properties matter here, and both are why this
// alphabet rather than hex or standard base32:
//
//   - It is in ascending ASCII order, so encoding preserves byte ordering:
//     sorting ids lexicographically sorts them chronologically, which is what
//     makes them usable as pagination cursors.
//   - It omits I, L, O and U, so an id read aloud or retyped from a log does
//     not turn into a different id.
var idEnc = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// Id prefixes. These are load-bearing in logs and in error messages: an id
// carries its own type, so a session id pasted where a command id belongs is
// visibly wrong rather than a confusing 404.
const (
	PrefixAgent      = "a"
	PrefixSession    = "s"
	PrefixCommand    = "c"
	PrefixAPIKey     = "k"
	PrefixEnrollment = "e"
	PrefixEvent      = "v"
)

// NewID returns a time-ordered unique id: 48 bits of millisecond timestamp
// followed by 80 bits of randomness, encoded as 26 Crockford base32 characters
// behind a type prefix -- the ULID layout.
//
// Ids are generated hub-side even for objects an agent proposes, so a
// compromised agent cannot choose ids that collide with another agent's.
func NewID(prefix string) string {
	return newIDAt(prefix, time.Now())
}

func newIDAt(prefix string, t time.Time) string {
	var b [16]byte

	ms := uint64(t.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand failing means the OS entropy source is gone. There is no
		// sane fallback: a predictable id here is a security bug, not a
		// degraded mode.
		panic("core: crypto/rand unavailable: " + err.Error())
	}

	return prefix + "_" + idEnc.EncodeToString(b[:])
}

// IDPrefix returns the type prefix of an id, or "" if it has none.
func IDPrefix(id string) string {
	i := strings.IndexByte(id, '_')
	if i <= 0 {
		return ""
	}
	return id[:i]
}

// HasPrefix reports whether id is well-formed and of the given type. Handlers
// use it to reject a wrong-typed id with a 400 instead of looking it up.
func HasPrefix(id, prefix string) bool {
	if IDPrefix(id) != prefix {
		return false
	}
	rest := id[len(prefix)+1:]
	if len(rest) != 26 {
		return false
	}
	_, err := idEnc.DecodeString(rest)
	return err == nil
}

// IDTime recovers the creation time encoded in an id. Useful for retention
// sweeps and for reading a log without a second lookup.
func IDTime(id string) (time.Time, bool) {
	i := strings.IndexByte(id, '_')
	if i < 0 || len(id)-i-1 != 26 {
		return time.Time{}, false
	}
	b, err := idEnc.DecodeString(id[i+1:])
	if err != nil || len(b) != 16 {
		return time.Time{}, false
	}
	ms := uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(b[2])<<24 |
		uint64(b[3])<<16 | uint64(b[4])<<8 | uint64(b[5])
	return time.UnixMilli(int64(ms)).UTC(), true
}
