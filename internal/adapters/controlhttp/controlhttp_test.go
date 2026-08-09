package controlhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tayyebi/scraper/internal/auth"
	"github.com/tayyebi/scraper/internal/bus"
	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/mirror"
	"github.com/tayyebi/scraper/internal/registry"
	"github.com/tayyebi/scraper/internal/store/blob"
	"github.com/tayyebi/scraper/internal/store/sqlite"
)

type hub struct {
	srv      *httptest.Server
	store    *sqlite.Store
	blobs    *blob.Store
	auth     *auth.Service
	registry *registry.Registry
	mirrors  *mirror.Manager

	adminKey string
	readKey  string
	steerKey string
}

func newHub(t *testing.T) *hub {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	st, err := sqlite.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	blobs, err := blob.New(dir)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	b := bus.New()
	t.Cleanup(b.Close)

	mirrors := mirror.NewManager(0)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := registry.New(registry.Options{Store: st, Blobs: blobs, Bus: b, Documents: mirrors, Logger: log})
	authSvc := auth.New(auth.Options{Store: st})

	h := New(Options{Registry: reg, Store: st, Blobs: blobs, Auth: authSvc, Logger: log, Version: "test"})
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	hb := &hub{srv: srv, store: st, blobs: blobs, auth: authSvc, registry: reg, mirrors: mirrors}
	for _, spec := range []struct {
		scope core.Scope
		dst   *string
	}{
		{core.ScopeAdmin, &hb.adminKey},
		{core.ScopeSteer, &hb.steerKey},
		{core.ScopeRead, &hb.readKey},
	} {
		_, secret, err := authSvc.MintAPIKey(ctx, string(spec.scope)+" key", spec.scope)
		if err != nil {
			t.Fatalf("MintAPIKey: %v", err)
		}
		*spec.dst = secret
	}
	return hb
}

func (h *hub) do(t *testing.T, method, path, key string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// fakeLink lets these tests drive commands without a browser. The Control Plane
// is being tested here, not the Agent Plane.
type fakeLink struct {
	id      string
	caps    core.Capabilities
	respond func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error)
}

func (f *fakeLink) AgentID() string                 { return f.id }
func (f *fakeLink) Capabilities() core.Capabilities { return f.caps }
func (f *fakeLink) Close(string) error              { return nil }
func (f *fakeLink) Call(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
	if f.respond != nil {
		return f.respond(ctx, sessionID, op, params)
	}
	return json.RawMessage(`{}`), nil
}

func (h *hub) attachAgent(t *testing.T, id string, caps core.Capabilities) *fakeLink {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := h.store.PutAgent(ctx, core.Agent{
		ID: id, Name: id, Capabilities: caps, Status: core.AgentOffline, EnrolledAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}
	link := &fakeLink{id: id, caps: caps, respond: func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"tabId":1}`), nil
	}}
	h.registry.Attach(ctx, link)
	return link
}

func fullCaps() core.Capabilities {
	return core.Capabilities{
		Capture: core.CaptureDebugger, OpenTabs: true, Attach: true, Mirror: true, Ops: core.KnownOps,
	}
}

// ------------------------------------------------------------------- auth

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	h := newHub(t)
	resp := h.do(t, http.MethodGet, "/v1/agents", "", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("no WWW-Authenticate header, so a caller cannot tell what credential to present")
	}
}

// Scopes nest: admin implies steer implies read. The point of the split is that
// a scraper's key cannot mint enrollment tokens.
func TestScopesAreEnforced(t *testing.T) {
	h := newHub(t)
	h.attachAgent(t, "a_1", fullCaps())

	cases := []struct {
		name   string
		method string
		path   string
		key    string
		body   any
		want   int
	}{
		{"read key reads agents", http.MethodGet, "/v1/agents", h.readKey, nil, http.StatusOK},
		{"read key cannot steer", http.MethodPost, "/v1/agents/a_1/sessions", h.readKey, map[string]string{"url": "https://example.test"}, http.StatusForbidden},
		{"steer key can open a session", http.MethodPost, "/v1/agents/a_1/sessions", h.steerKey, map[string]string{"url": "https://example.test"}, http.StatusCreated},
		{"steer key cannot mint enrollments", http.MethodPost, "/v1/enrollments", h.steerKey, map[string]any{}, http.StatusForbidden},
		{"admin key can mint enrollments", http.MethodPost, "/v1/enrollments", h.adminKey, map[string]any{}, http.StatusCreated},
		{"steer key can read", http.MethodGet, "/v1/agents", h.steerKey, nil, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := h.do(t, c.method, c.path, c.key, c.body)
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				msg, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, c.want, msg)
			}
		})
	}
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()

	key, secret, err := h.auth.MintAPIKey(ctx, "temporary", core.ScopeRead)
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
	resp := h.do(t, http.MethodGet, "/v1/agents", secret, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d before revocation", resp.StatusCode)
	}

	revoke := h.do(t, http.MethodDelete, "/v1/apikeys/"+key.ID, h.adminKey, nil)
	_ = revoke.Body.Close()
	if revoke.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d", revoke.StatusCode)
	}

	after := h.do(t, http.MethodGet, "/v1/agents", secret, nil)
	defer after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d after revocation, want 401", after.StatusCode)
	}
}

// ----------------------------------------------------------------- agents

func TestEnrollmentTokenIsReturnedOnce(t *testing.T) {
	h := newHub(t)
	resp := h.do(t, http.MethodPost, "/v1/enrollments", h.adminKey,
		map[string]any{"labels": map[string]string{"team": "growth"}, "ttlSeconds": 600})

	var out enrollmentResponse
	decode(t, resp, &out)

	if out.Token == "" || out.ID == "" {
		t.Fatalf("response = %+v", out)
	}
	if out.Labels["team"] != "growth" {
		t.Errorf("labels = %v", out.Labels)
	}
	if out.ExpiresAt.Before(time.Now()) {
		t.Error("the token is already expired")
	}
	if out.Note == "" {
		t.Error("no note explaining that the token is shown once")
	}
}

// A caller must learn what a capture mode cannot see before they trust a log.
func TestAgentListDeclaresCaptureGaps(t *testing.T) {
	h := newHub(t)
	caps := fullCaps()
	caps.Capture = core.CaptureFetchPatch
	h.attachAgent(t, "a_partial", caps)

	resp := h.do(t, http.MethodGet, "/v1/agents", h.readKey, nil)
	var out struct {
		Agents []struct {
			ID           string   `json:"id"`
			Status       string   `json:"status"`
			CaptureGaps  []string `json:"captureGaps"`
			FullFidelity bool     `json:"fullFidelityCapture"`
		} `json:"agents"`
	}
	decode(t, resp, &out)

	if len(out.Agents) != 1 {
		t.Fatalf("got %d agents", len(out.Agents))
	}
	a := out.Agents[0]
	if a.Status != string(core.AgentOnline) {
		t.Errorf("status = %q, want online", a.Status)
	}
	if a.FullFidelity {
		t.Error("fetch-patch was reported as full fidelity")
	}
	if len(a.CaptureGaps) == 0 {
		t.Error("a partial capture mode declared no gaps, so a caller would trust an incomplete log")
	}
}

func TestOpenSessionRequiresAURL(t *testing.T) {
	h := newHub(t)
	h.attachAgent(t, "a_1", fullCaps())

	resp := h.do(t, http.MethodPost, "/v1/agents/a_1/sessions", h.steerKey, map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOfflineAgentIsServiceUnavailable(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := h.store.PutAgent(ctx, core.Agent{ID: "a_gone", EnrolledAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}

	resp := h.do(t, http.MethodPost, "/v1/agents/a_gone/sessions", h.steerKey, map[string]string{"url": "https://example.test"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After: an offline agent is expected back, and the caller should be told to retry")
	}
}

func TestUnknownAgentIsNotFound(t *testing.T) {
	h := newHub(t)
	resp := h.do(t, http.MethodGet, "/v1/agents/a_nope", h.readKey, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------- commands

func (h *hub) openSession(t *testing.T) core.Session {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/v1/agents/a_1/sessions", h.steerKey, map[string]string{"url": "https://example.test"})
	var sess core.Session
	decode(t, resp, &sess)
	if sess.ID == "" {
		t.Fatal("no session created")
	}
	return sess
}

func TestSynchronousCommand(t *testing.T) {
	h := newHub(t)
	link := h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)

	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"title":"Example Domain"}`), nil
	}

	resp := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/commands?wait=5s", h.steerKey,
		map[string]any{"op": core.OpNavigate, "params": map[string]string{"url": "https://example.test"}})

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d: %s", resp.StatusCode, msg)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/v1/commands/") {
		t.Errorf("Location = %q", loc)
	}

	var cmd core.Command
	decode(t, resp, &cmd)
	if cmd.State != core.CommandDone {
		t.Errorf("state = %q, want done", cmd.State)
	}
	if !strings.Contains(string(cmd.Result), "Example Domain") {
		t.Errorf("result = %s", cmd.Result)
	}
}

// Without ?wait the command is accepted and pollable. Both paths write the same
// row, which is what lets a caller recover a result their wait outlived.
func TestAsynchronousCommandIsPollable(t *testing.T) {
	h := newHub(t)
	link := h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)

	release := make(chan struct{})
	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		if op == core.OpClick {
			<-release
		}
		return json.RawMessage(`{"clicked":true}`), nil
	}

	resp := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/commands", h.steerKey,
		map[string]any{"op": core.OpClick})
	if resp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 202: %s", resp.StatusCode, msg)
	}
	var queued core.Command
	decode(t, resp, &queued)
	if queued.State != core.CommandPending {
		t.Errorf("state = %q, want pending", queued.State)
	}

	close(release)

	deadline := time.Now().Add(3 * time.Second)
	for {
		poll := h.do(t, http.MethodGet, "/v1/commands/"+queued.ID, h.readKey, nil)
		var cmd core.Command
		decode(t, poll, &cmd)
		if cmd.State.Terminal() {
			if cmd.State != core.CommandDone {
				t.Fatalf("state = %q (%s), want done", cmd.State, cmd.Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the queued command never reached a terminal state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCommandWaitTimeoutIsGatewayTimeout(t *testing.T) {
	h := newHub(t)
	link := h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)

	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		if op == core.OpWaitFor {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return json.RawMessage(`{}`), nil
	}

	resp := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/commands?wait=100ms", h.steerKey,
		map[string]any{"op": core.OpWaitFor})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
	var cmd core.Command
	if err := json.NewDecoder(resp.Body).Decode(&cmd); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cmd.ID == "" {
		t.Error("a timed-out wait returned no command id, so the result is unrecoverable")
	}
}

func TestUnsupportedOpIsUnprocessable(t *testing.T) {
	h := newHub(t)
	caps := fullCaps()
	caps.Ops = []string{core.OpNavigate} // no eval
	h.attachAgent(t, "a_1", caps)
	sess := h.openSession(t)

	resp := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/commands?wait=2s", h.steerKey,
		map[string]any{"op": core.OpEval, "params": map[string]string{"expression": "1+1"}})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

func TestUnknownOpIsBadRequest(t *testing.T) {
	h := newHub(t)
	h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)

	resp := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/commands?wait=2s", h.steerKey,
		map[string]any{"op": "rm -rf /"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestWaitParam(t *testing.T) {
	cases := map[string]time.Duration{
		"":      0,
		"30s":   30 * time.Second,
		"30":    30 * time.Second,
		"1m":    time.Minute,
		"100ms": 100 * time.Millisecond,
	}
	for raw, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/sessions/s_1/commands?wait="+raw, nil)
		got, err := waitParam(r, DefaultMaxWait)
		if err != nil {
			t.Errorf("wait=%q: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("wait=%q = %s, want %s", raw, got, want)
		}
	}

	// Clamped rather than honoured: an hour-long hold gets dropped by every
	// proxy in between anyway.
	r := httptest.NewRequest(http.MethodPost, "/x?wait=1h", nil)
	if got, _ := waitParam(r, DefaultMaxWait); got != DefaultMaxWait {
		t.Errorf("wait=1h = %s, want it clamped to %s", got, DefaultMaxWait)
	}

	bad := httptest.NewRequest(http.MethodPost, "/x?wait=soon", nil)
	if _, err := waitParam(bad, DefaultMaxWait); err == nil {
		t.Error("wait=soon was accepted")
	}
}

// --------------------------------------------------------------------- DOM

func (h *hub) snapshot(t *testing.T, sessionID string) {
	t.Helper()
	h.mirrors.For(sessionID).ApplySnapshot(mirror.MainFrame, core.Document{
		SessionID: sessionID,
		URL:       "https://example.test",
		Title:     "Example",
		Root: &core.Node{ID: 1, Type: core.NodeElement, Name: "html", Kids: []*core.Node{
			{ID: 2, Type: core.NodeElement, Name: "body", Kids: []*core.Node{
				{ID: 3, Type: core.NodeElement, Name: "h1", Kids: []*core.Node{
					{ID: 4, Type: core.NodeText, Value: "Hello & welcome"},
				}},
			}},
		}},
	}, 5)
}

func TestDOMFormats(t *testing.T) {
	h := newHub(t)
	h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)
	h.snapshot(t, sess.ID)

	t.Run("json", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/dom", h.readKey, nil)
		var doc core.Document
		decode(t, resp, &doc)
		if doc.Seq != 5 || doc.Root == nil {
			t.Errorf("document = %+v", doc)
		}
	})

	t.Run("html", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/dom?format=html", h.readKey, nil)
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q", ct)
		}
		if resp.Header.Get("X-Hub-Mirror-Seq") != "5" {
			t.Errorf("X-Hub-Mirror-Seq = %q, want 5", resp.Header.Get("X-Hub-Mirror-Seq"))
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "<h1>Hello &amp; welcome</h1>") {
			t.Errorf("html = %s", body)
		}
	})

	t.Run("text", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/dom?format=text", h.readKey, nil)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "Hello & welcome") {
			t.Errorf("text = %q", body)
		}
	})
}

// ------------------------------------------------------------------ events

func TestEventStreamSSE(t *testing.T) {
	h := newHub(t)
	h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/sessions/"+sess.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+h.readKey)
	req.Header.Set("Accept", "text/event-stream")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	// Without this, nginx buffers the stream into one long silence.
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering is not set, so this stream will be buffered by a reverse proxy")
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = h.registry.RecordEvent(context.Background(), core.Event{
			SessionID: sess.ID, AgentID: "a_1", Type: core.EventNavigated,
			Body: json.RawMessage(`{"url":"https://example.test/next"}`), At: time.Now().UTC(),
		})
	}()

	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			var e core.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
				t.Fatalf("event body: %v", err)
			}
			if e.Type == core.EventNavigated {
				return
			}
		}
	}
	t.Fatal("no navigated event arrived on the SSE stream")
}

func TestEventStreamNDJSON(t *testing.T) {
	h := newHub(t)
	h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/sessions/"+sess.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+h.readKey)
	req.Header.Set("Accept", "application/x-ndjson")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", ct)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = h.registry.RecordEvent(context.Background(), core.Event{
			SessionID: sess.ID, Type: core.EventConsole, At: time.Now().UTC(),
		})
	}()

	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// NDJSON must be exactly one JSON object per line, with no SSE framing.
		if strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "event:") {
			t.Fatalf("SSE framing leaked into the NDJSON stream: %q", line)
		}
		var e core.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not a JSON object: %q", line)
		}
		if e.Type == core.EventConsole {
			return
		}
	}
	t.Fatal("no event arrived on the NDJSON stream")
}

// ------------------------------------------------------------- request log

func (h *hub) recordExchange(t *testing.T, sessionID, digest string, body []byte) {
	t.Helper()
	ctx := context.Background()
	if body != nil {
		if _, err := h.blobs.Put(ctx, digest, bytes.NewReader(body), "application/json", 0); err != nil {
			t.Fatalf("Put blob: %v", err)
		}
	}
	if err := h.registry.RecordExchange(ctx, core.Exchange{
		SessionID: sessionID, AgentID: "a_1", Method: "GET",
		URL: "https://api.example.test/items?page=2", Status: 200, StatusText: "OK",
		MimeType: "application/json", ResBody: digest, ResBodySize: int64(len(body)),
		ReqHeaders: map[string]string{"accept": "application/json"},
		ResHeaders: map[string]string{"content-type": "application/json"},
		StartedAt:  time.Now().UTC(), DurationMs: 42,
	}); err != nil {
		t.Fatalf("RecordExchange: %v", err)
	}
}

func TestRequestLogCarriesCaptureGaps(t *testing.T) {
	h := newHub(t)
	caps := fullCaps()
	caps.Capture = core.CaptureFetchPatch
	h.attachAgent(t, "a_1", caps)
	sess := h.openSession(t)

	body := []byte(`{"items":[1,2,3]}`)
	h.recordExchange(t, sess.ID, blob.Digest(body), body)

	resp := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/requests", h.readKey, nil)
	var out struct {
		Exchanges   []core.Exchange `json:"exchanges"`
		CaptureGaps []string        `json:"captureGaps"`
	}
	decode(t, resp, &out)

	if len(out.Exchanges) != 1 {
		t.Fatalf("got %d exchanges", len(out.Exchanges))
	}
	if len(out.CaptureGaps) == 0 {
		t.Error("the request log of a partial-capture agent declared no gaps")
	}
}

func TestHARExport(t *testing.T) {
	h := newHub(t)
	h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)

	body := []byte(`{"items":[1,2,3]}`)
	digest := blob.Digest(body)
	h.recordExchange(t, sess.ID, digest, body)

	resp := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/har", h.readKey, nil)
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, ".har") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	var har harLog
	decode(t, resp, &har)

	if har.Log.Version != "1.2" {
		t.Errorf("HAR version = %q, want 1.2", har.Log.Version)
	}
	if len(har.Log.Entries) != 1 {
		t.Fatalf("got %d entries", len(har.Log.Entries))
	}
	e := har.Log.Entries[0]
	if e.Request.Method != "GET" || e.Response.Status != 200 {
		t.Errorf("entry = %+v", e)
	}
	if e.Response.Content.Digest != digest {
		t.Errorf("content digest = %q, want %q", e.Response.Content.Digest, digest)
	}
	if e.Response.Content.Text != "" {
		t.Error("the body was inlined without ?bodies=1")
	}
	// Query strings must be broken out; that is what HAR readers display.
	if len(e.Request.QueryString) != 1 || e.Request.QueryString[0].Name != "page" {
		t.Errorf("queryString = %+v", e.Request.QueryString)
	}
	if len(e.Request.Headers) == 0 {
		t.Error("request headers were dropped")
	}
}

func TestHARInlinesBodiesOnRequest(t *testing.T) {
	h := newHub(t)
	h.attachAgent(t, "a_1", fullCaps())
	sess := h.openSession(t)

	body := []byte(`{"items":[1,2,3]}`)
	h.recordExchange(t, sess.ID, blob.Digest(body), body)

	resp := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/har?bodies=1", h.readKey, nil)
	var har harLog
	decode(t, resp, &har)

	if len(har.Log.Entries) != 1 {
		t.Fatalf("got %d entries", len(har.Log.Entries))
	}
	if got := har.Log.Entries[0].Response.Content.Text; got != string(body) {
		t.Errorf("inlined body = %q, want %q", got, body)
	}
}

// A HAR that silently omits navigations looks like a page that made none. The
// warning has to survive the file being downloaded and mailed around.
func TestHARDeclaresIncompleteCapture(t *testing.T) {
	h := newHub(t)
	caps := fullCaps()
	caps.Capture = core.CaptureFetchPatch
	h.attachAgent(t, "a_1", caps)
	sess := h.openSession(t)

	resp := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/har", h.readKey, nil)
	var har harLog
	decode(t, resp, &har)

	if !strings.Contains(har.Log.Comment, "not complete") {
		t.Errorf("HAR comment = %q, want it to declare the capture incomplete", har.Log.Comment)
	}
}

// -------------------------------------------------------------- artifacts

func TestArtifactIsServedSafely(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()

	// Captured bytes are somebody else's HTML.
	body := []byte(`<script>alert(document.domain)</script>`)
	digest := blob.Digest(body)
	if _, err := h.blobs.Put(ctx, digest, bytes.NewReader(body), "text/html", 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	resp := h.do(t, http.MethodGet, "/v1/artifacts/"+digest, h.readKey, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q", got)
	}

	// Serving captured HTML inline from the hub's origin would let a captured
	// page script the console.
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("Content-Security-Policy = %q, want a sandbox", csp)
	}
	// Content addressing makes immutability trivially true.
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestArtifactRejectsMalformedDigest(t *testing.T) {
	h := newHub(t)
	resp := h.do(t, http.MethodGet, "/v1/artifacts/not-a-digest", h.readKey, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMissingArtifactIsNotFound(t *testing.T) {
	h := newHub(t)
	resp := h.do(t, http.MethodGet, "/v1/artifacts/"+blob.Digest([]byte("never stored")), h.readKey, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// -------------------------------------------------------------------- meta

func TestMetaDescribesTheHub(t *testing.T) {
	h := newHub(t)
	resp := h.do(t, http.MethodGet, "/v1/meta", h.readKey, nil)

	var out struct {
		Version  string   `json:"version"`
		Commands []string `json:"commands"`
		Identity struct {
			Kind  string `json:"kind"`
			Scope string `json:"scope"`
		} `json:"identity"`
	}
	decode(t, resp, &out)

	if out.Version != "test" {
		t.Errorf("version = %q", out.Version)
	}
	if len(out.Commands) != len(core.KnownOps) {
		t.Errorf("advertised %d commands, want %d", len(out.Commands), len(core.KnownOps))
	}
	if out.Identity.Kind != "apikey" || out.Identity.Scope != string(core.ScopeRead) {
		t.Errorf("identity = %+v", out.Identity)
	}
}
