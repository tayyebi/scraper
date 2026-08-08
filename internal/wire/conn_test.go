package wire

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The example from RFC 6455 section 1.3. If this value is wrong, no browser
// will ever complete a handshake with this hub, and nothing else in the package
// matters.
func TestAcceptKeyRFCExample(t *testing.T) {
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := acceptKey(key); got != want {
		t.Errorf("acceptKey(%q) = %q, want %q", key, got, want)
	}
}

// upgradeServer starts an httptest server that upgrades every request and hands
// the connection to fn.
func upgradeServer(t *testing.T, opts UpgradeOptions, fn func(*Conn)) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, opts)
		if err != nil {
			close(done)
			return
		}
		fn(c)
		close(done)
	}))
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	return srv
}

// dial performs a client-side handshake by hand and returns a client Conn.
// There is no client implementation in the package -- the hub is always the
// server -- so the test writes the request itself.
func dial(t *testing.T, serverURL string, header http.Header) (*Conn, *http.Response) {
	t.Helper()

	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse %q: %v", serverURL, err)
	}

	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	req, err := http.NewRequest(http.MethodGet, serverURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Upgrade") == "" {
		req.Header.Set("Upgrade", "websocket")
	}
	if req.Header.Get("Connection") == "" {
		req.Header.Set("Connection", "Upgrade")
	}
	if req.Header.Get("Sec-WebSocket-Version") == "" {
		req.Header.Set("Sec-WebSocket-Version", "13")
	}
	if req.Header.Get("Sec-WebSocket-Key") == "" {
		req.Header.Set("Sec-WebSocket-Key", key)
	}

	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, resp
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), acceptKey(req.Header.Get("Sec-WebSocket-Key")); got != want {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
	// The same bufio.Reader is reused: it may already hold frame bytes that
	// arrived in the same TCP segment as the response.
	return newConn(conn, br, false, 0), resp
}

func TestUpgradeAndEcho(t *testing.T) {
	srv := upgradeServer(t, UpgradeOptions{}, func(c *Conn) {
		for {
			op, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(op, data); err != nil {
				return
			}
		}
	})

	c, _ := dial(t, srv.URL, nil)
	if c == nil {
		t.Fatal("handshake did not produce a connection")
	}

	for _, msg := range []string{"hello", "", strings.Repeat("x", 1000), "unicode: ☃ é 🙂"} {
		if err := c.WriteText([]byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		op, got, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if op != OpText {
			t.Errorf("opcode = %s, want text", op)
		}
		if string(got) != msg {
			t.Errorf("echo = %q, want %q", got, msg)
		}
	}
}

// Messages larger than 64 KiB exercise the 64-bit length path over a real
// socket, where a short read is possible and a naive implementation breaks.
func TestLargeMessageRoundTrip(t *testing.T) {
	srv := upgradeServer(t, UpgradeOptions{}, func(c *Conn) {
		op, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteMessage(op, data)
	})

	c, _ := dial(t, srv.URL, nil)
	payload := make([]byte, 200*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	go func() { _ = c.WriteMessage(OpBinary, payload) }()

	op, got, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != OpBinary {
		t.Errorf("opcode = %s, want binary", op)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload round trip corrupted (%d bytes back, %d sent)", len(got), len(payload))
	}
}

// A ping must be answered automatically from inside the read loop, without the
// application seeing it.
func TestPingIsAnsweredAutomatically(t *testing.T) {
	srv := upgradeServer(t, UpgradeOptions{}, func(c *Conn) {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})

	c, _ := dial(t, srv.URL, nil)

	got := make(chan []byte, 1)
	c.SetOnPong(func(b []byte) {
		select {
		case got <- append([]byte(nil), b...):
		default:
		}
	})

	if err := c.Ping([]byte("are you there")); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// The pong arrives as a control frame; ReadMessage dispatches it to the
	// handler and keeps waiting, so read in the background.
	go func() { _, _, _ = c.ReadMessage() }()

	select {
	case b := <-got:
		if string(b) != "are you there" {
			t.Errorf("pong payload = %q, want the ping payload echoed", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pong within 2s: the read loop is not answering pings")
	}
}

// RFC 6455 requires clients to mask. An unmasked client frame is a protocol
// violation and must fail the connection with 1002, not be quietly accepted.
func TestUnmaskedClientFrameIsRejected(t *testing.T) {
	closed := make(chan error, 1)
	srv := upgradeServer(t, UpgradeOptions{}, func(c *Conn) {
		_, _, err := c.ReadMessage()
		closed <- err
	})

	c, _ := dial(t, srv.URL, nil)

	// Write an unmasked text frame by going under the Conn API, which would
	// never produce one from a client.
	bw := bufio.NewWriter(c.conn)
	if err := writeFrame(bw, true, OpText, nil, []byte("unmasked")); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	select {
	case err := <-closed:
		var ce *CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want a CloseError", err)
		}
		if ce.Code != CloseProtocolError {
			t.Errorf("close code = %d, want %d", ce.Code, CloseProtocolError)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server accepted an unmasked client frame")
	}
}

func TestMessageSizeLimitIsEnforced(t *testing.T) {
	closed := make(chan error, 1)
	srv := upgradeServer(t, UpgradeOptions{MaxMessageBytes: 1024}, func(c *Conn) {
		_, _, err := c.ReadMessage()
		closed <- err
	})

	c, _ := dial(t, srv.URL, nil)
	go func() { _ = c.WriteMessage(OpBinary, make([]byte, 4096)) }()

	select {
	case err := <-closed:
		var ce *CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want a CloseError", err)
		}
		if ce.Code != CloseMessageTooBig {
			t.Errorf("close code = %d, want %d", ce.Code, CloseMessageTooBig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oversized message was not rejected")
	}
}

func TestCloseHandshake(t *testing.T) {
	serverErr := make(chan error, 1)
	srv := upgradeServer(t, UpgradeOptions{}, func(c *Conn) {
		_, _, err := c.ReadMessage()
		serverErr <- err
	})

	c, _ := dial(t, srv.URL, nil)
	if err := c.Close(CloseGoingAway, "bye"); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case err := <-serverErr:
		var ce *CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want a CloseError", err)
		}
		if ce.Code != CloseGoingAway {
			t.Errorf("close code = %d, want %d", ce.Code, CloseGoingAway)
		}
		if ce.Reason != "bye" {
			t.Errorf("close reason = %q, want %q", ce.Reason, "bye")
		}
		if !IsCleanClose(err) {
			t.Error("1001 should read as a clean close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed the close")
	}
}

func TestUpgradeRejectsBadHandshakes(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		status int
	}{
		{
			name:   "wrong version",
			header: http.Header{"Sec-WebSocket-Version": {"8"}},
			status: http.StatusUpgradeRequired,
		},
		{
			name:   "key is not 16 bytes",
			header: http.Header{"Sec-WebSocket-Key": {base64.StdEncoding.EncodeToString([]byte("short"))}},
			status: http.StatusBadRequest,
		},
		{
			name:   "key is not base64",
			header: http.Header{"Sec-WebSocket-Key": {"!!!not base64!!!"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "upgrade header is not websocket",
			header: http.Header{"Upgrade": {"h2c"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "connection header lacks upgrade",
			header: http.Header{"Connection": {"keep-alive"}},
			status: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := upgradeServer(t, UpgradeOptions{}, func(c *Conn) {
				t.Error("handshake should not have succeeded")
			})
			conn, resp := dial(t, srv.URL, tc.header)
			if conn != nil {
				t.Fatal("a malformed handshake produced a connection")
			}
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

// Proxies legitimately append to Connection, so token matching must be a list
// search rather than an equality check.
func TestConnectionHeaderTokenList(t *testing.T) {
	srv := upgradeServer(t, UpgradeOptions{}, func(c *Conn) {
		_ = c.WriteText([]byte("ok"))
		_, _, _ = c.ReadMessage()
	})
	c, resp := dial(t, srv.URL, http.Header{"Connection": {"keep-alive, Upgrade"}})
	if c == nil {
		t.Fatalf("handshake rejected a valid token list: status %d", resp.StatusCode)
	}
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("got %q, want %q", data, "ok")
	}
}

func TestSubprotocolNegotiationPrefersServerOrder(t *testing.T) {
	srv := upgradeServer(t, UpgradeOptions{Subprotocols: []string{"hub.v1", "hub.v0"}}, func(c *Conn) {
		_, _, _ = c.ReadMessage()
	})
	_, resp := dial(t, srv.URL, http.Header{"Sec-WebSocket-Protocol": {"hub.v0, hub.v1"}})
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "hub.v1" {
		t.Errorf("negotiated %q, want hub.v1: server preference must win", got)
	}
}

func TestNegotiate(t *testing.T) {
	if got := negotiate([]string{"a", "b"}, []string{"b"}); got != "b" {
		t.Errorf("negotiate = %q, want b", got)
	}
	if got := negotiate([]string{"a", "b"}, []string{"c"}); got != "" {
		t.Errorf("negotiate = %q, want empty when nothing overlaps", got)
	}
	if got := negotiate(nil, []string{"a"}); got != "" {
		t.Errorf("negotiate = %q, want empty when the server offers nothing", got)
	}
}
