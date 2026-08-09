package wire

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Dial performs the client half of the RFC 6455 handshake.
//
// The hub is always the server -- agents dial in, because they sit behind NAT.
// A client exists here anyway for two reasons: the Agent Plane's tests need to
// drive a real channel rather than a mock, and a Go-side agent (a headless
// worker, a bridge) is a natural thing to build against this package.
//
// The returned response is non-nil on a rejected handshake, so a caller can
// report the server's actual reason instead of "handshake failed".
func Dial(ctx context.Context, rawURL string, header http.Header, opts UpgradeOptions) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("websocket: bad URL: %w", err)
	}

	var (
		secure  bool
		address = u.Host
	)
	switch strings.ToLower(u.Scheme) {
	case "ws", "http":
		if u.Port() == "" {
			address = net.JoinHostPort(u.Hostname(), "80")
		}
	case "wss", "https":
		secure = true
		if u.Port() == "" {
			address = net.JoinHostPort(u.Hostname(), "443")
		}
	default:
		return nil, nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}

	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("websocket: dial: %w", err)
	}
	if secure {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: u.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("websocket: TLS: %w", err)
		}
		conn = tlsConn
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("websocket: crypto/rand unavailable: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	httpURL := *u
	switch strings.ToLower(u.Scheme) {
	case "ws":
		httpURL.Scheme = "http"
	case "wss":
		httpURL.Scheme = "https"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpURL.String(), nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	if len(opts.Subprotocols) > 0 {
		req.Header.Set("Sec-WebSocket-Protocol", strings.Join(opts.Subprotocols, ", "))
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("websocket: writing handshake: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("websocket: reading handshake response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, resp, &HandshakeError{Status: resp.StatusCode, Reason: resp.Status}
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(key) {
		_ = conn.Close()
		// A wrong accept value means whatever answered did not actually
		// implement WebSocket -- a cache or a proxy replaying a 101.
		return nil, resp, fmt.Errorf("websocket: Sec-WebSocket-Accept mismatch; the peer did not complete a real handshake")
	}

	// Clear the handshake deadline: from here, deadlines are the caller's to set.
	_ = conn.SetDeadline(time.Time{})

	return newConn(conn, br, false, opts.MaxMessageBytes), resp, nil
}
