package wire

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// websocketGUID is the fixed string from RFC 6455 section 1.3. It exists so a
// server's 101 response cannot be produced by anything that did not deliberately
// implement WebSocket -- it proves the handshake was understood, not merely
// echoed by a cache.
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// acceptKey computes the Sec-WebSocket-Accept response value.
//
// SHA-1 here is not a security choice and is not a weakness: the value is a
// handshake proof, not an authenticator. RFC 6455 fixes the algorithm.
func acceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key)
	_, _ = io.WriteString(h, websocketGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// UpgradeOptions configures the server handshake.
type UpgradeOptions struct {
	// MaxMessageBytes bounds one reassembled message. Zero means the default.
	MaxMessageBytes int64

	// Subprotocols this server accepts, most preferred first. The first one the
	// client also offers is selected.
	Subprotocols []string

	// HandshakeTimeout bounds writing the 101 response. Zero means 10s.
	HandshakeTimeout time.Duration
}

// HandshakeError describes a rejected upgrade. The HTTP response has already
// been written when this is returned, so a handler should just log and return.
type HandshakeError struct {
	Status int
	Reason string
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("websocket handshake failed (%d): %s", e.Status, e.Reason)
}

// Upgrade performs the RFC 6455 server handshake and takes over the connection.
//
// It requires http.Hijacker, which does not exist under HTTP/2 -- see the
// TLSNextProto note in cmd/hubd. That is the single most likely way this fails
// in production, so the error says so explicitly rather than reporting a bare
// type assertion failure.
func Upgrade(w http.ResponseWriter, r *http.Request, opts UpgradeOptions) (*Conn, error) {
	if r.Method != http.MethodGet {
		return nil, reject(w, http.StatusMethodNotAllowed, "the WebSocket handshake must be a GET")
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		return nil, reject(w, http.StatusBadRequest, "the Connection header must contain 'upgrade'")
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return nil, reject(w, http.StatusBadRequest, "the Upgrade header must be 'websocket'")
	}
	if v := r.Header.Get("Sec-WebSocket-Version"); v != "13" {
		// RFC 6455 requires advertising the supported version when refusing.
		w.Header().Set("Sec-WebSocket-Version", "13")
		return nil, reject(w, http.StatusUpgradeRequired, "unsupported WebSocket version "+v+"; this hub speaks 13")
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if !validKey(key) {
		return nil, reject(w, http.StatusBadRequest, "Sec-WebSocket-Key must be 16 base64-encoded bytes")
	}

	var chosen string
	if len(opts.Subprotocols) > 0 {
		chosen = negotiate(opts.Subprotocols, headerTokens(r.Header, "Sec-WebSocket-Protocol"))
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		// Almost always means the listener negotiated HTTP/2.
		return nil, reject(w, http.StatusInternalServerError,
			"connection cannot be hijacked, so this is probably HTTP/2; the hub must serve the agent channel over HTTP/1.1")
	}

	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, reject(w, http.StatusInternalServerError, "hijack failed: "+err.Error())
	}

	// A client that sent frames before seeing the 101 has violated the
	// handshake, and worse, those bytes may have been chosen by something other
	// than the client (request smuggling). Refuse rather than parse them.
	if brw.Reader.Buffered() > 0 {
		_ = netConn.Close()
		return nil, errors.New("websocket: client sent data before the handshake completed")
	}

	var resp bytes.Buffer
	resp.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	resp.WriteString("Upgrade: websocket\r\n")
	resp.WriteString("Connection: Upgrade\r\n")
	resp.WriteString("Sec-WebSocket-Accept: ")
	resp.WriteString(acceptKey(key))
	resp.WriteString("\r\n")
	if chosen != "" {
		resp.WriteString("Sec-WebSocket-Protocol: ")
		resp.WriteString(chosen)
		resp.WriteString("\r\n")
	}
	resp.WriteString("\r\n")

	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	_ = netConn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := netConn.Write(resp.Bytes()); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("websocket: writing handshake response: %w", err)
	}
	_ = netConn.SetWriteDeadline(time.Time{})

	// brw.Reader is reused rather than wrapped: net/http may have already read
	// bytes into it, and a second bufio.Reader would strand them.
	return newConn(netConn, brw.Reader, true, opts.MaxMessageBytes), nil
}

// Subprotocol negotiation: server preference wins, which keeps the choice a
// server-side policy decision rather than something a client can steer.
func negotiate(server, client []string) string {
	for _, s := range server {
		for _, c := range client {
			if strings.EqualFold(s, c) {
				return s
			}
		}
	}
	return ""
}

func reject(w http.ResponseWriter, status int, reason string) error {
	http.Error(w, "websocket: "+reason, status)
	return &HandshakeError{Status: status, Reason: reason}
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(key)
	return err == nil && len(b) == 16
}

// headerTokens splits a comma-separated header into trimmed, non-empty tokens.
func headerTokens(h http.Header, name string) []string {
	var out []string
	for _, v := range h.Values(name) {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out = append(out, tok)
			}
		}
	}
	return out
}

// headerHasToken reports whether a comma-separated header contains a token,
// case-insensitively. Connection is a list header -- proxies legitimately
// append to it -- so a plain equality check against "Upgrade" would break
// behind, for example, "keep-alive, Upgrade".
func headerHasToken(h http.Header, name, token string) bool {
	for _, tok := range headerTokens(h, name) {
		if strings.EqualFold(tok, token) {
			return true
		}
	}
	return false
}
