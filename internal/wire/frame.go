// Package wire is the command channel: the envelope codec and a hand-rolled
// RFC 6455 implementation.
//
// Hand-rolled because the whole binary has exactly one direct dependency, and
// because the subset actually needed here is small: server side only, no
// extensions, no compression. What that subset must still get exactly right is
// framing, masking, fragmentation, and close semantics -- so those are isolated
// in this file and tested against the vectors in RFC 6455 section 5.7.
package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Opcode is the RFC 6455 frame opcode.
type Opcode byte

const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xA
)

// IsControl reports whether the opcode is a control frame. Control frames may
// be injected between the fragments of a data message, which is why the read
// loop dispatches on this before touching fragmentation state.
func (o Opcode) IsControl() bool { return o&0x08 != 0 }

func (o Opcode) String() string {
	switch o {
	case OpContinuation:
		return "continuation"
	case OpText:
		return "text"
	case OpBinary:
		return "binary"
	case OpClose:
		return "close"
	case OpPing:
		return "ping"
	case OpPong:
		return "pong"
	default:
		return fmt.Sprintf("opcode(0x%x)", byte(o))
	}
}

// RFC 6455 section 7.4.1 status codes.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseUnsupportedData = 1003
	// CloseNoStatus and CloseAbnormal never appear on the wire; they are what
	// the application sees when a peer closed without a code, or dropped.
	CloseNoStatus       = 1005
	CloseAbnormal       = 1006
	CloseInvalidPayload = 1007
	ClosePolicyViolation = 1008
	CloseMessageTooBig  = 1009
	CloseMandatoryExt   = 1010
	CloseInternalError  = 1011
)

// ProtocolError is a violation of RFC 6455 by the peer. It always results in
// the connection being failed rather than recovered: there is no meaningful way
// to resynchronize a framed stream once a frame has been misread.
type ProtocolError struct {
	Code   int
	Reason string
}

func (e *ProtocolError) Error() string { return "websocket protocol error: " + e.Reason }

func protoErr(code int, reason string) *ProtocolError {
	return &ProtocolError{Code: code, Reason: reason}
}

// CloseError reports that the connection closed, cleanly or otherwise.
type CloseError struct {
	Code   int
	Reason string
	// Local is true when this side initiated the close.
	Local bool
}

func (e *CloseError) Error() string {
	who := "peer"
	if e.Local {
		who = "local"
	}
	if e.Reason == "" {
		return fmt.Sprintf("websocket closed by %s: %d", who, e.Code)
	}
	return fmt.Sprintf("websocket closed by %s: %d %s", who, e.Code, e.Reason)
}

// IsCleanClose reports whether err is an ordinary shutdown rather than a fault.
// Callers use it to avoid logging a stack trace every time a laptop lid closes.
func IsCleanClose(err error) bool {
	var ce *CloseError
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Code == CloseNormal || ce.Code == CloseGoingAway || ce.Code == CloseNoStatus
}

type frameHeader struct {
	fin     bool
	rsv     byte
	opcode  Opcode
	masked  bool
	length  int64
	maskKey [4]byte
}

// readHeader reads one frame header, including the extended length and the
// masking key when present.
func readHeader(br *bufio.Reader) (frameHeader, error) {
	var h frameHeader

	b0, err := br.ReadByte()
	if err != nil {
		return h, err
	}
	b1, err := br.ReadByte()
	if err != nil {
		return h, err
	}

	h.fin = b0&0x80 != 0
	h.rsv = (b0 >> 4) & 0x07
	h.opcode = Opcode(b0 & 0x0F)
	h.masked = b1&0x80 != 0

	n := int64(b1 & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return h, err
		}
		n = int64(binary.BigEndian.Uint16(ext[:]))
		// RFC 6455 5.2: the minimal number of bytes must be used. Accepting a
		// non-minimal encoding would let a peer smuggle two readings of the
		// same stream past a proxy that disagrees with us about length.
		if n < 126 {
			return h, protoErr(CloseProtocolError, "non-minimal 16-bit length")
		}
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return h, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u > math.MaxInt64 {
			return h, protoErr(CloseProtocolError, "payload length overflows int64")
		}
		n = int64(u)
		if n <= math.MaxUint16 {
			return h, protoErr(CloseProtocolError, "non-minimal 64-bit length")
		}
	}
	h.length = n

	if h.masked {
		if _, err := io.ReadFull(br, h.maskKey[:]); err != nil {
			return h, err
		}
	}
	return h, nil
}

// writeFrame writes one complete frame. A nil mask means "do not mask", which
// is what a server always does: RFC 6455 requires clients to mask and servers
// not to.
func writeFrame(bw *bufio.Writer, fin bool, op Opcode, mask *[4]byte, payload []byte) error {
	var b0 byte
	if fin {
		b0 |= 0x80
	}
	b0 |= byte(op) & 0x0F
	if err := bw.WriteByte(b0); err != nil {
		return err
	}

	var b1 byte
	if mask != nil {
		b1 |= 0x80
	}
	n := len(payload)
	switch {
	case n < 126:
		if err := bw.WriteByte(b1 | byte(n)); err != nil {
			return err
		}
	case n <= math.MaxUint16:
		if err := bw.WriteByte(b1 | 126); err != nil {
			return err
		}
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		if _, err := bw.Write(ext[:]); err != nil {
			return err
		}
	default:
		if err := bw.WriteByte(b1 | 127); err != nil {
			return err
		}
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		if _, err := bw.Write(ext[:]); err != nil {
			return err
		}
	}

	if mask != nil {
		if _, err := bw.Write(mask[:]); err != nil {
			return err
		}
		// Mask a copy: the caller's buffer must not be mutated, and callers do
		// reuse buffers.
		masked := make([]byte, n)
		copy(masked, payload)
		maskBytes(*mask, 0, masked)
		payload = masked
	}

	if _, err := bw.Write(payload); err != nil {
		return err
	}
	return bw.Flush()
}

// maskBytes XORs b in place with the running masking key, returning the key
// offset to resume from. Masking is symmetric, so this both masks and unmasks.
func maskBytes(key [4]byte, pos int, b []byte) int {
	for i := range b {
		b[i] ^= key[pos&3]
		pos++
	}
	return pos & 3
}

// closePayload builds a close frame body. Code 1005 and 1006 are not
// transmissible -- they are what the application sees, not what goes on the
// wire -- so they degrade to an empty payload.
func closePayload(code int, reason string) []byte {
	if code == CloseNoStatus || code == CloseAbnormal || code == 0 {
		return nil
	}
	// A close reason shares the 125-byte control frame budget with the code.
	if len(reason) > 123 {
		reason = reason[:123]
	}
	buf := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(buf[:2], uint16(code))
	copy(buf[2:], reason)
	return buf
}

// parseClose decodes a close frame body.
func parseClose(payload []byte) (code int, reason string, err error) {
	switch len(payload) {
	case 0:
		return CloseNoStatus, "", nil
	case 1:
		return 0, "", protoErr(CloseProtocolError, "close payload of 1 byte")
	}
	code = int(binary.BigEndian.Uint16(payload[:2]))
	reason = string(payload[2:])
	if !validCloseCode(code) {
		return 0, "", protoErr(CloseProtocolError, fmt.Sprintf("invalid close code %d", code))
	}
	return code, reason, nil
}

func validCloseCode(code int) bool {
	switch {
	case code >= 1000 && code <= 1003:
		return true
	case code >= 1007 && code <= 1011:
		return true
	case code >= 3000 && code <= 4999:
		// 3000-3999 registered, 4000-4999 private use.
		return true
	default:
		return false
	}
}
