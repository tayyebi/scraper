package wire

import (
	"bufio"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf8"
)

// DefaultMaxMessageBytes bounds a single channel message.
//
// It is small on purpose. Bulk bytes never cross this channel -- captured
// bodies are uploaded over HTTP and referenced by digest -- so anything
// approaching this size is a bug or an attack, not legitimate traffic.
const DefaultMaxMessageBytes = 8 << 20 // 8 MiB

// Conn is one WebSocket connection.
//
// Concurrency contract: one goroutine may read at a time, any number may write.
// Writes are serialized by a mutex because the read loop itself writes -- it
// answers pings and echoes closes -- and those must not interleave with an
// application frame.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer

	isServer   bool
	maxMessage int64

	wmu       sync.Mutex
	closeSent bool

	rmu sync.Mutex

	closeOnce sync.Once

	// onPong, when set, is called from the read loop for each pong received.
	// Used for liveness tracking; the read loop is the only caller, so it needs
	// no lock of its own.
	onPong func([]byte)
}

func newConn(c net.Conn, br *bufio.Reader, isServer bool, maxMessage int64) *Conn {
	if maxMessage <= 0 {
		maxMessage = DefaultMaxMessageBytes
	}
	if br == nil {
		br = bufio.NewReader(c)
	}
	return &Conn{
		conn:       c,
		br:         br,
		bw:         bufio.NewWriter(c),
		isServer:   isServer,
		maxMessage: maxMessage,
	}
}

// RemoteAddr reports the peer address, for logging.
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// SetOnPong installs a pong handler. Call it before the read loop starts.
func (c *Conn) SetOnPong(f func([]byte)) { c.onPong = f }

// SetReadDeadline bounds how long a read may block. The agent plane sets this
// to slightly more than the ping interval: a channel that stops answering pings
// is indistinguishable from a browser that went to sleep, and both should drop.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// SetWriteDeadline bounds how long a write may block.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// ReadMessage reads one complete application message, reassembling fragments
// and transparently handling ping, pong, and close.
//
// It returns a *CloseError when the peer closes, which callers should treat as
// end-of-stream rather than as a failure.
func (c *Conn) ReadMessage() (Opcode, []byte, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()

	var (
		msgOp      Opcode
		buf        []byte
		fragmented bool
	)

	for {
		h, err := readHeader(c.br)
		if err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) {
				return 0, nil, c.fail(pe.Code, pe.Reason)
			}
			return 0, nil, err
		}

		// No extensions are negotiated, so any reserved bit set means the peer
		// is speaking a protocol we did not agree to.
		if h.rsv != 0 {
			return 0, nil, c.fail(CloseProtocolError, "reserved bits set without a negotiated extension")
		}
		if c.isServer && !h.masked {
			return 0, nil, c.fail(CloseProtocolError, "client-to-server frame must be masked")
		}
		if !c.isServer && h.masked {
			return 0, nil, c.fail(CloseProtocolError, "server-to-client frame must not be masked")
		}
		if h.opcode.IsControl() {
			if h.length > 125 {
				return 0, nil, c.fail(CloseProtocolError, "control frame payload exceeds 125 bytes")
			}
			if !h.fin {
				return 0, nil, c.fail(CloseProtocolError, "control frame must not be fragmented")
			}
		}
		if h.length > c.maxMessage || int64(len(buf))+h.length > c.maxMessage {
			return 0, nil, c.fail(CloseMessageTooBig, "message exceeds the channel limit")
		}

		payload := make([]byte, h.length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
		if h.masked {
			maskBytes(h.maskKey, 0, payload)
		}

		switch h.opcode {
		case OpPing:
			if err := c.writeControl(OpPong, payload); err != nil {
				return 0, nil, err
			}
			continue

		case OpPong:
			if c.onPong != nil {
				c.onPong(payload)
			}
			continue

		case OpClose:
			code, reason, perr := parseClose(payload)
			if perr != nil {
				return 0, nil, c.fail(CloseProtocolError, "malformed close payload")
			}
			if reason != "" && !utf8.ValidString(reason) {
				return 0, nil, c.fail(CloseInvalidPayload, "close reason is not valid UTF-8")
			}
			// Echo the close, then stop. A code of 1005 means the peer sent no
			// code, and 1005 is not transmissible, so echo a normal close.
			echo := code
			if echo == CloseNoStatus {
				echo = CloseNormal
			}
			_ = c.writeControl(OpClose, closePayload(echo, ""))
			c.closeUnderlying()
			return 0, nil, &CloseError{Code: code, Reason: reason}

		case OpText, OpBinary:
			if fragmented {
				return 0, nil, c.fail(CloseProtocolError, "expected a continuation frame")
			}
			msgOp = h.opcode
			buf = payload
			if !h.fin {
				fragmented = true
				continue
			}

		case OpContinuation:
			if !fragmented {
				return 0, nil, c.fail(CloseProtocolError, "continuation frame without a started message")
			}
			buf = append(buf, payload...)
			if !h.fin {
				continue
			}
			fragmented = false

		default:
			return 0, nil, c.fail(CloseProtocolError, "unknown opcode "+h.opcode.String())
		}

		// Validation happens on the reassembled message, not per fragment: a
		// multi-byte rune is allowed to straddle a fragment boundary.
		if msgOp == OpText && !utf8.Valid(buf) {
			return 0, nil, c.fail(CloseInvalidPayload, "text message is not valid UTF-8")
		}
		return msgOp, buf, nil
	}
}

// WriteMessage writes one complete application message.
func (c *Conn) WriteMessage(op Opcode, data []byte) error {
	if op.IsControl() {
		return protoErr(CloseInternalError, "WriteMessage used for a control frame")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return &CloseError{Code: CloseNormal, Local: true}
	}
	return writeFrame(c.bw, true, op, c.maskKey(), data)
}

// WriteText is the common case: a JSON envelope.
func (c *Conn) WriteText(data []byte) error { return c.WriteMessage(OpText, data) }

// Ping sends a ping frame.
//
// MV3 service workers are evicted after ~30s idle, and Chrome counts WebSocket
// traffic as activity, so this doubles as the agent's keepalive. See
// docs/protocol.md.
func (c *Conn) Ping(data []byte) error { return c.writeControl(OpPing, data) }

// Pong sends an unsolicited pong, which RFC 6455 permits as a unidirectional
// heartbeat.
func (c *Conn) Pong(data []byte) error { return c.writeControl(OpPong, data) }

func (c *Conn) writeControl(op Opcode, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent && op == OpClose {
		return nil
	}
	if op == OpClose {
		c.closeSent = true
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := writeFrame(c.bw, true, op, c.maskKey(), payload)
	_ = c.conn.SetWriteDeadline(time.Time{})
	return err
}

// maskKey returns a fresh masking key for clients and nil for servers.
//
// The key must be unpredictable, not merely unique: masking exists to stop a
// client from steering the exact bytes on the wire past a caching proxy, and a
// guessable key defeats that entirely.
func (c *Conn) maskKey() *[4]byte {
	if c.isServer {
		return nil
	}
	var k [4]byte
	if _, err := rand.Read(k[:]); err != nil {
		panic("wire: crypto/rand unavailable: " + err.Error())
	}
	return &k
}

// Close sends a close frame with the given code and reason, then closes the
// underlying connection.
func (c *Conn) Close(code int, reason string) error {
	err := c.writeControl(OpClose, closePayload(code, reason))
	c.closeUnderlying()
	return err
}

// fail closes the connection because the peer violated the protocol, and
// returns the error to hand back to the caller.
func (c *Conn) fail(code int, reason string) error {
	_ = c.writeControl(OpClose, closePayload(code, reason))
	c.closeUnderlying()
	return &CloseError{Code: code, Reason: reason, Local: true}
}

func (c *Conn) closeUnderlying() {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
	})
}
