package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ProtocolVersion is the envelope version. It is checked on every frame, not
// just at handshake, because an agent can be updated underneath a live channel.
const ProtocolVersion = 1

// Envelope kinds.
const (
	// KindCmd is hub to agent: do this.
	KindCmd = "cmd"
	// KindRes is agent to hub: the result of the cmd with this id.
	KindRes = "res"
	// KindEvt is agent to hub, unsolicited: something happened.
	KindEvt = "evt"
	// KindErr is either direction: the request with this id failed.
	KindErr = "err"
)

// Envelope is the single message shape on the command channel.
//
// One shape rather than several: multiplexing many sessions over one socket
// means every frame must be routable without knowing what it is, so id, sid and
// kind sit at a fixed place and the payload stays opaque until dispatch.
type Envelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id,omitempty"`
	Kind string          `json:"t"`
	TS   int64           `json:"ts"`
	SID  string          `json:"sid,omitempty"`
	Op   string          `json:"op,omitempty"`
	Seq  int64           `json:"seq,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
	Err  *EnvelopeError  `json:"err,omitempty"`
}

// EnvelopeError is a failure reported inside an envelope, as distinct from a
// transport failure. Code is machine-readable and stable; Message is for humans.
type EnvelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *EnvelopeError) Error() string { return e.Code + ": " + e.Message }

// Stable error codes an agent may report.
const (
	CodeUnsupported  = "unsupported"
	CodeNoSuchTab    = "no_such_tab"
	CodeTimeout      = "timeout"
	CodeNavigation   = "navigation_failed"
	CodeNotFound     = "not_found"
	CodeBadParams    = "bad_params"
	CodeDenied       = "denied"
	CodeInternal     = "internal"
	CodeDetached     = "detached"
	CodeMirrorGap    = "mirror_gap"
)

var errBadEnvelope = errors.New("malformed envelope")

// NewCmd builds a hub-to-agent command envelope.
func NewCmd(id, sessionID, op string, body json.RawMessage) Envelope {
	return Envelope{
		V:    ProtocolVersion,
		ID:   id,
		Kind: KindCmd,
		TS:   nowMillis(),
		SID:  sessionID,
		Op:   op,
		Body: body,
	}
}

// NewRes builds a result envelope correlated to a command id.
func NewRes(id, sessionID string, body json.RawMessage) Envelope {
	return Envelope{
		V:    ProtocolVersion,
		ID:   id,
		Kind: KindRes,
		TS:   nowMillis(),
		SID:  sessionID,
		Body: body,
	}
}

// NewErr builds an error envelope correlated to a command id.
func NewErr(id, sessionID, code, message string) Envelope {
	return Envelope{
		V:    ProtocolVersion,
		ID:   id,
		Kind: KindErr,
		TS:   nowMillis(),
		SID:  sessionID,
		Err:  &EnvelopeError{Code: code, Message: message},
	}
}

// NewEvt builds an unsolicited event envelope.
func NewEvt(sessionID, op string, seq int64, body json.RawMessage) Envelope {
	return Envelope{
		V:    ProtocolVersion,
		Kind: KindEvt,
		TS:   nowMillis(),
		SID:  sessionID,
		Op:   op,
		Seq:  seq,
		Body: body,
	}
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// Time returns the envelope timestamp.
func (e Envelope) Time() time.Time { return time.UnixMilli(e.TS).UTC() }

// Validate rejects envelopes that cannot be routed.
//
// This runs on every inbound frame. An agent is a browser extension that a
// hostile page may have influenced, so nothing about an envelope is trusted
// because it arrived on an authenticated channel -- authentication says who
// sent it, not that what they sent is well-formed.
func (e Envelope) Validate() error {
	if e.V != ProtocolVersion {
		return fmt.Errorf("%w: version %d, this hub speaks %d", errBadEnvelope, e.V, ProtocolVersion)
	}
	switch e.Kind {
	case KindCmd:
		if e.ID == "" {
			return fmt.Errorf("%w: cmd needs an id to correlate against", errBadEnvelope)
		}
		if e.Op == "" {
			return fmt.Errorf("%w: cmd needs an op", errBadEnvelope)
		}
	case KindRes:
		if e.ID == "" {
			return fmt.Errorf("%w: res needs the id of the cmd it answers", errBadEnvelope)
		}
	case KindErr:
		if e.ID == "" {
			return fmt.Errorf("%w: err needs the id of the cmd it answers", errBadEnvelope)
		}
		if e.Err == nil || e.Err.Code == "" {
			return fmt.Errorf("%w: err needs a code", errBadEnvelope)
		}
	case KindEvt:
		if e.Op == "" {
			return fmt.Errorf("%w: evt needs an op naming what happened", errBadEnvelope)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", errBadEnvelope, e.Kind)
	}
	return nil
}

// IsBadEnvelope reports whether err came from Validate or Decode.
func IsBadEnvelope(err error) bool { return errors.Is(err, errBadEnvelope) }

// Encode serializes an envelope for a text frame.
func Encode(e Envelope) ([]byte, error) { return json.Marshal(e) }

// Decode parses and validates an inbound frame.
func Decode(b []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("%w: %v", errBadEnvelope, err)
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	return e, nil
}

// WriteEnvelope encodes and writes an envelope as a text frame.
func (c *Conn) WriteEnvelope(e Envelope) error {
	b, err := Encode(e)
	if err != nil {
		return err
	}
	return c.WriteText(b)
}

// ReadEnvelope reads the next text frame and decodes it.
//
// Binary frames are a protocol violation on this channel: bulk bytes go over
// HTTP to the artifact endpoint precisely so they cannot head-of-line block the
// commands sharing this socket.
func (c *Conn) ReadEnvelope() (Envelope, error) {
	op, data, err := c.ReadMessage()
	if err != nil {
		return Envelope{}, err
	}
	if op != OpText {
		return Envelope{}, fmt.Errorf("%w: %s frame on the command channel; upload bytes to /agent/v1/artifacts instead", errBadEnvelope, op)
	}
	return Decode(data)
}
