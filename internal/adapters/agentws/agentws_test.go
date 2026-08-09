package agentws

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tayyebi/scraper/internal/auth"
	"github.com/tayyebi/scraper/internal/bus"
	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/mirror"
	"github.com/tayyebi/scraper/internal/registry"
	"github.com/tayyebi/scraper/internal/store/blob"
	"github.com/tayyebi/scraper/internal/store/sqlite"
	"github.com/tayyebi/scraper/internal/wire"
)

type hub struct {
	srv      *httptest.Server
	store    *sqlite.Store
	blobs    *blob.Store
	auth     *auth.Service
	registry *registry.Registry
	mirrors  *mirror.Manager
}

func newHub(t *testing.T) *hub {
	t.Helper()
	dir := t.TempDir()

	st, err := sqlite.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	blobs, err := blob.New(dir)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}

	b := bus.New()
	t.Cleanup(b.Close)

	mirrors := mirror.NewManager(0)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := registry.New(registry.Options{
		Store: st, Blobs: blobs, Bus: b, Documents: mirrors, Logger: log,
	})
	authSvc := auth.New(auth.Options{Store: st})

	h := New(Options{
		Registry: reg, Auth: authSvc, Store: st, Blobs: blobs, Mirrors: mirrors,
		Logger: log, PingInterval: time.Second, ReadTimeout: 10 * time.Second,
		MaxArtifactBytes: 4096,
	})

	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &hub{srv: srv, store: st, blobs: blobs, auth: authSvc, registry: reg, mirrors: mirrors}
}

// mintToken creates an enrollment token the way an operator would.
func (h *hub) mintToken(t *testing.T, labels map[string]string) string {
	t.Helper()
	_, secret, err := h.auth.MintEnrollment(context.Background(), labels, time.Hour)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}
	return secret
}

func (h *hub) enroll(t *testing.T, token string, caps core.Capabilities) enrollResponse {
	t.Helper()
	body, _ := json.Marshal(enrollRequest{
		Token: token, Name: "test agent", Browser: "chrome",
		BrowserVer: "131", Platform: "linux", AgentVersion: "0.1.0",
		Capabilities: caps,
	})
	resp, err := http.Post(h.srv.URL+"/agent/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("enroll: status %d: %s", resp.StatusCode, msg)
	}
	var out enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode enrollment: %v", err)
	}
	return out
}

func fullCaps() core.Capabilities {
	return core.Capabilities{
		Capture: core.CaptureDebugger, OpenTabs: true, Attach: true, Mirror: true,
		Screenshot: true, Cookies: true, Ops: core.KnownOps,
	}
}

// testAgent is a browser stand-in: it dials the channel and answers commands.
type testAgent struct {
	t    *testing.T
	conn *wire.Conn

	mu       sync.Mutex
	received []wire.Envelope

	commands chan wire.Envelope
}

func (h *hub) dial(t *testing.T, credential string) *testAgent {
	t.Helper()
	conn, _, err := wire.Dial(context.Background(), h.srv.URL+"/agent/v1/channel",
		http.Header{"Authorization": {"Bearer " + credential}},
		wire.UpgradeOptions{Subprotocols: []string{Subprotocol}})
	if err != nil {
		t.Fatalf("dial channel: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(wire.CloseNormal, "test over") })

	a := &testAgent{t: t, conn: conn, commands: make(chan wire.Envelope, 32)}
	return a
}

// serve answers commands with handle. A nil handle echoes an empty result.
func (a *testAgent) serve(handle func(wire.Envelope) wire.Envelope) {
	go func() {
		for {
			env, err := a.conn.ReadEnvelope()
			if err != nil {
				return
			}
			a.mu.Lock()
			a.received = append(a.received, env)
			a.mu.Unlock()

			select {
			case a.commands <- env:
			default:
			}

			if env.Kind != wire.KindCmd {
				continue
			}
			res := wire.NewRes(env.ID, env.SID, json.RawMessage(`{}`))
			if handle != nil {
				res = handle(env)
			}
			if err := a.conn.WriteEnvelope(res); err != nil {
				return
			}
		}
	}()
}

func (a *testAgent) awaitCommand(op string, within time.Duration) (wire.Envelope, bool) {
	deadline := time.After(within)
	for {
		select {
		case env := <-a.commands:
			if env.Kind == wire.KindCmd && (op == "" || env.Op == op) {
				return env, true
			}
		case <-deadline:
			return wire.Envelope{}, false
		}
	}
}

func (a *testAgent) emit(env wire.Envelope) {
	a.t.Helper()
	if err := a.conn.WriteEnvelope(env); err != nil {
		a.t.Fatalf("emit %s: %v", env.Op, err)
	}
}

// ------------------------------------------------------------------- tests

func TestEnrollmentYieldsAWorkingChannel(t *testing.T) {
	h := newHub(t)
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	if enrolled.Credential == "" || enrolled.AgentID == "" {
		t.Fatalf("enrollment returned %+v", enrolled)
	}
	if enrolled.ProtocolVersion != wire.ProtocolVersion {
		t.Errorf("protocol version = %d, want %d", enrolled.ProtocolVersion, wire.ProtocolVersion)
	}

	agent := h.dial(t, enrolled.Credential)
	agent.serve(nil)

	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	a, err := h.registry.GetAgent(context.Background(), enrolled.AgentID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.Status != core.AgentOnline {
		t.Errorf("status = %q, want online", a.Status)
	}
}

// The whole reason enrollment and credentials are separate types.
func TestEnrollmentTokenIsOneTime(t *testing.T) {
	h := newHub(t)
	token := h.mintToken(t, nil)
	h.enroll(t, token, fullCaps())

	body, _ := json.Marshal(enrollRequest{Token: token, Name: "second device"})
	resp, err := http.Post(h.srv.URL+"/agent/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409: a one-time token was accepted twice", resp.StatusCode)
	}
}

func TestEnrollmentRejectsUnknownToken(t *testing.T) {
	h := newHub(t)
	body, _ := json.Marshal(enrollRequest{Token: "not-a-real-token", Name: "impostor"})
	resp, err := http.Post(h.srv.URL+"/agent/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 404 or 401", resp.StatusCode)
	}
}

// Labels from the operator's token must win: an agent that could relabel itself
// could move into another team's fleet.
func TestTokenLabelsOverrideAgentClaims(t *testing.T) {
	h := newHub(t)
	token := h.mintToken(t, map[string]string{"team": "growth"})

	body, _ := json.Marshal(enrollRequest{
		Token: token, Name: "sneaky", Labels: map[string]string{"team": "finance", "own": "yes"},
	})
	resp, err := http.Post(h.srv.URL+"/agent/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()

	var out enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Labels["team"] != "growth" {
		t.Errorf("team label = %q, want growth: the agent overrode the operator", out.Labels["team"])
	}
	if out.Labels["own"] != "yes" {
		t.Error("an agent's own non-conflicting labels should survive")
	}
}

func TestChannelRequiresACredential(t *testing.T) {
	h := newHub(t)
	if _, _, err := wire.Dial(context.Background(), h.srv.URL+"/agent/v1/channel", nil, wire.UpgradeOptions{}); err == nil {
		t.Fatal("an unauthenticated channel upgrade succeeded")
	}
	if _, _, err := wire.Dial(context.Background(), h.srv.URL+"/agent/v1/channel",
		http.Header{"Authorization": {"Bearer wrong"}}, wire.UpgradeOptions{}); err == nil {
		t.Fatal("a bogus credential was accepted")
	}
}

// A browser's WebSocket API cannot set an Authorization header, so the
// subprotocol path is the one real agents use. It has to work.
func TestChannelAcceptsCredentialViaSubprotocol(t *testing.T) {
	h := newHub(t)
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	conn, resp, err := wire.Dial(context.Background(), h.srv.URL+"/agent/v1/channel", nil,
		wire.UpgradeOptions{Subprotocols: []string{Subprotocol, credentialProtocolPrefix + enrolled.Credential}})
	if err != nil {
		t.Fatalf("dial with subprotocol credential: %v", err)
	}
	defer conn.Close(wire.CloseNormal, "done")

	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != Subprotocol {
		t.Errorf("negotiated %q, want %q", got, Subprotocol)
	}
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })
}

func TestRevokedCredentialCannotReconnect(t *testing.T) {
	h := newHub(t)
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(nil)
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	if err := h.auth.RevokeAgent(context.Background(), enrolled.AgentID); err != nil {
		t.Fatalf("RevokeAgent: %v", err)
	}
	if _, _, err := wire.Dial(context.Background(), h.srv.URL+"/agent/v1/channel",
		http.Header{"Authorization": {"Bearer " + enrolled.Credential}}, wire.UpgradeOptions{}); err == nil {
		t.Error("a revoked credential still opened a channel")
	}
}

func TestCommandRoundTrip(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(func(env wire.Envelope) wire.Envelope {
		switch env.Op {
		case "openTab":
			return wire.NewRes(env.ID, env.SID, json.RawMessage(`{"tabId":7,"url":"https://example.test"}`))
		case core.OpNavigate:
			return wire.NewRes(env.ID, env.SID, json.RawMessage(`{"url":"https://example.test/next","title":"Next"}`))
		}
		return wire.NewRes(env.ID, env.SID, json.RawMessage(`{}`))
	})
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	sess, err := h.registry.OpenSession(ctx, enrolled.AgentID, core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if sess.TabID != 7 {
		t.Errorf("tab id = %d, want 7", sess.TabID)
	}

	cmd, err := h.registry.Dispatch(ctx, sess.ID, core.OpNavigate,
		json.RawMessage(`{"url":"https://example.test/next"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmd.State != core.CommandDone {
		t.Fatalf("state = %q (%s), want done", cmd.State, cmd.Error)
	}
	if !strings.Contains(string(cmd.Result), "Next") {
		t.Errorf("result = %s", cmd.Result)
	}
}

// An agent's err envelope must surface as a failed command carrying the agent's
// own reason, not as a generic transport error.
func TestAgentErrorSurfacesAsCommandError(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(func(env wire.Envelope) wire.Envelope {
		if env.Op == "openTab" {
			return wire.NewRes(env.ID, env.SID, json.RawMessage(`{"tabId":1}`))
		}
		return wire.NewErr(env.ID, env.SID, wire.CodeNotFound, "selector matched nothing")
	})
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	sess, err := h.registry.OpenSession(ctx, enrolled.AgentID, core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	cmd, err := h.registry.Dispatch(ctx, sess.ID, core.OpClick, json.RawMessage(`{"selector":"#nope"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmd.State != core.CommandError {
		t.Errorf("state = %q, want error", cmd.State)
	}
	if !strings.Contains(cmd.Error, "selector matched nothing") {
		t.Errorf("error = %q, want the agent's own reason", cmd.Error)
	}
}

// A dropped channel must fail every in-flight call immediately. Otherwise each
// one waits out a full deadline for an answer that can never arrive.
func TestChannelDropFailsInFlightCommands(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(func(env wire.Envelope) wire.Envelope {
		if env.Op == "openTab" {
			return wire.NewRes(env.ID, env.SID, json.RawMessage(`{"tabId":1}`))
		}
		// Never answer anything else.
		return wire.Envelope{}
	})
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	sess, err := h.registry.OpenSession(ctx, enrolled.AgentID, core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	done := make(chan core.Command, 1)
	go func() {
		cmd, _ := h.registry.Dispatch(ctx, sess.ID, core.OpWaitFor, nil, 60*time.Second)
		done <- cmd
	}()

	time.Sleep(200 * time.Millisecond)
	_ = agent.conn.Close(wire.CloseGoingAway, "browser closed")

	select {
	case cmd := <-done:
		if cmd.State == core.CommandDone {
			t.Error("a command completed even though the channel dropped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an in-flight command outlived its channel by more than 5s")
	}
}

func TestMirrorIsServedWithoutARoundTrip(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(func(env wire.Envelope) wire.Envelope {
		return wire.NewRes(env.ID, env.SID, json.RawMessage(`{"tabId":1}`))
	})
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	sess, err := h.registry.OpenSession(ctx, enrolled.AgentID, core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	snapshot, _ := json.Marshal(map[string]any{
		"frameId": mirror.MainFrame,
		"url":     "https://example.test",
		"title":   "Example",
		"root": map[string]any{
			"id": 1, "t": core.NodeElement, "n": "html",
			"c": []any{map[string]any{"id": 2, "t": core.NodeElement, "n": "body"}},
		},
	})
	agent.emit(wire.NewEvt(sess.ID, evtMirrorSnapshot, 1, snapshot))

	waitFor(t, 3*time.Second, func() bool {
		_, ok := h.mirrors.Document(sess.ID)
		return ok
	})

	mutation, _ := json.Marshal(map[string]any{
		"ops": []mirror.Op{{
			Kind: mirror.OpInsert, Parent: 2,
			Node: &core.Node{ID: 3, Type: core.NodeElement, Name: "h1",
				Kids: []*core.Node{{ID: 4, Type: core.NodeText, Value: "Hello"}}},
		}},
	})
	agent.emit(wire.NewEvt(sess.ID, evtMirrorMutation, 2, mutation))

	waitFor(t, 3*time.Second, func() bool {
		doc, ok := h.mirrors.Document(sess.ID)
		return ok && doc.Seq == 2
	})

	doc, err := h.registry.DOM(ctx, sess.ID, false)
	if err != nil {
		t.Fatalf("DOM: %v", err)
	}
	html := mirror.RenderHTML(doc)
	if !strings.Contains(html, "<h1>Hello</h1>") {
		t.Errorf("mirror did not reflect the mutation:\n%s", html)
	}
}

// The gap rule is what makes the mirror trustworthy: a hole in the stream must
// make the hub demand a fresh snapshot rather than serve a diverged document.
func TestMirrorGapDemandsAResnapshot(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(func(env wire.Envelope) wire.Envelope {
		return wire.NewRes(env.ID, env.SID, json.RawMessage(`{"tabId":1}`))
	})
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	sess, err := h.registry.OpenSession(ctx, enrolled.AgentID, core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	snapshot, _ := json.Marshal(map[string]any{
		"frameId": mirror.MainFrame,
		"root":    map[string]any{"id": 1, "t": core.NodeElement, "n": "html"},
	})
	agent.emit(wire.NewEvt(sess.ID, evtMirrorSnapshot, 1, snapshot))
	waitFor(t, 3*time.Second, func() bool {
		_, ok := h.mirrors.Document(sess.ID)
		return ok
	})

	// Skip seq 2 entirely.
	mutation, _ := json.Marshal(map[string]any{"ops": []mirror.Op{{Kind: mirror.OpRemove, ID: 1}}})
	agent.emit(wire.NewEvt(sess.ID, evtMirrorMutation, 7, mutation))

	if _, ok := agent.awaitCommand(core.OpSnapshotDOM, 5*time.Second); !ok {
		t.Fatal("a sequence gap did not produce a re-snapshot demand")
	}

	m, ok := h.mirrors.Lookup(sess.ID)
	if !ok {
		t.Fatal("mirror disappeared")
	}
	if !m.Stale() {
		t.Error("the mirror still serves after a gap, so it may be silently wrong")
	}
	if _, ok := h.mirrors.Document(sess.ID); ok {
		t.Error("a stale mirror was still served")
	}
}

// A navigation replaces the document, so the old mirror must stop being served.
func TestNavigationInvalidatesTheMirror(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(func(env wire.Envelope) wire.Envelope {
		return wire.NewRes(env.ID, env.SID, json.RawMessage(`{"tabId":1}`))
	})
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	sess, _ := h.registry.OpenSession(ctx, enrolled.AgentID, core.OpenSessionRequest{URL: "https://example.test"})

	snapshot, _ := json.Marshal(map[string]any{"root": map[string]any{"id": 1, "t": core.NodeElement, "n": "html"}})
	agent.emit(wire.NewEvt(sess.ID, evtMirrorSnapshot, 1, snapshot))
	waitFor(t, 3*time.Second, func() bool {
		_, ok := h.mirrors.Document(sess.ID)
		return ok
	})

	nav, _ := json.Marshal(map[string]any{"url": "https://example.test/somewhere-else", "title": "Elsewhere"})
	agent.emit(wire.NewEvt(sess.ID, evtNavigated, 2, nav))

	waitFor(t, 3*time.Second, func() bool {
		_, ok := h.mirrors.Document(sess.ID)
		return !ok
	})

	waitFor(t, 3*time.Second, func() bool {
		s, err := h.registry.GetSession(ctx, sess.ID)
		return err == nil && s.URL == "https://example.test/somewhere-else"
	})
}

func TestAttachedSessionIsRecorded(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(nil)
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	sessionID := "s_" + strings.Repeat("A", 26)
	body, _ := json.Marshal(sessionEvent{
		SessionID: sessionID, TabID: 12, URL: "https://mail.example.test", Title: "Inbox",
		Origin: string(core.OriginManaged), // a lie the hub must not believe
	})
	agent.emit(wire.NewEvt(sessionID, evtSessionAttached, 1, body))

	waitFor(t, 3*time.Second, func() bool {
		_, err := h.registry.GetSession(ctx, sessionID)
		return err == nil
	})

	sess, err := h.registry.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Origin != core.OriginAttached {
		t.Errorf("origin = %q, want attached: an agent laundered provenance", sess.Origin)
	}
	if sess.URL != "https://mail.example.test" {
		t.Errorf("url = %q", sess.URL)
	}
}

func TestExchangeReachesTheRequestLog(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	agent := h.dial(t, enrolled.Credential)
	agent.serve(func(env wire.Envelope) wire.Envelope {
		return wire.NewRes(env.ID, env.SID, json.RawMessage(`{"tabId":1}`))
	})
	waitFor(t, 2*time.Second, func() bool { return h.registry.Online() == 1 })

	sess, _ := h.registry.OpenSession(ctx, enrolled.AgentID, core.OpenSessionRequest{URL: "https://example.test"})

	body, _ := json.Marshal(core.Exchange{
		Method: "GET", URL: "https://api.example.test/items", Status: 200,
		MimeType: "application/json", DurationMs: 31,
		ResBody: "NOT-A-DIGEST", StartedAt: time.Now().UTC(),
	})
	agent.emit(wire.NewEvt(sess.ID, evtExchange, 1, body))

	waitFor(t, 3*time.Second, func() bool {
		log, err := h.registry.Exchanges(ctx, sess.ID, 0)
		return err == nil && len(log) == 1
	})

	log, err := h.registry.Exchanges(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("Exchanges: %v", err)
	}
	if log[0].URL != "https://api.example.test/items" || log[0].Status != 200 {
		t.Errorf("exchange = %+v", log[0])
	}
	if log[0].ResBody != "" {
		t.Errorf("a malformed digest %q was recorded; it could never resolve", log[0].ResBody)
	}
}

// ---------------------------------------------------------------- artifacts

func (h *hub) putArtifact(t *testing.T, credential, digest string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, h.srv.URL+"/agent/v1/artifacts/"+digest, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	return resp
}

func TestArtifactUpload(t *testing.T) {
	h := newHub(t)
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())
	body := []byte(`{"items":[1,2,3]}`)
	digest := blob.Digest(body)

	resp := h.putArtifact(t, enrolled.Credential, digest, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, msg)
	}
	if !h.blobs.Has(context.Background(), digest) {
		t.Error("the artifact was not stored")
	}

	// Re-uploading the same bytes is a no-op the agent can rely on.
	again := h.putArtifact(t, enrolled.Credential, digest, body)
	defer again.Body.Close()
	if again.StatusCode != http.StatusOK && again.StatusCode != http.StatusCreated {
		t.Errorf("re-upload status = %d", again.StatusCode)
	}
}

// The digest is the name. Bytes that hash to something else must be refused, or
// every later read returns the wrong content under a trusted name.
func TestArtifactDigestIsVerified(t *testing.T) {
	h := newHub(t)
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	claimed := blob.Digest([]byte("what I said I was sending"))
	resp := h.putArtifact(t, enrolled.Credential, claimed, []byte("what I actually sent"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if h.blobs.Has(context.Background(), claimed) {
		t.Error("a mismatched artifact was stored anyway")
	}
}

func TestArtifactSizeCap(t *testing.T) {
	h := newHub(t)
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	big := bytes.Repeat([]byte("x"), 8192) // the hub caps at 4096 in this test
	resp := h.putArtifact(t, enrolled.Credential, blob.Digest(big), big)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestArtifactRequiresCredential(t *testing.T) {
	h := newHub(t)
	body := []byte("anonymous upload")
	req, _ := http.NewRequest(http.MethodPut, h.srv.URL+"/agent/v1/artifacts/"+blob.Digest(body), bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// HEAD is what lets an agent skip an upload the hub already has.
func TestArtifactHeadReportsPresence(t *testing.T) {
	h := newHub(t)
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())
	body := []byte("cached bytes")
	digest := blob.Digest(body)

	head := func() int {
		req, _ := http.NewRequest(http.MethodHead, h.srv.URL+"/agent/v1/artifacts/"+digest, nil)
		req.Header.Set("Authorization", "Bearer "+enrolled.Credential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("head: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := head(); got != http.StatusNotFound {
		t.Errorf("status before upload = %d, want 404", got)
	}
	resp := h.putArtifact(t, enrolled.Credential, digest, body)
	_ = resp.Body.Close()
	if got := head(); got != http.StatusOK {
		t.Errorf("status after upload = %d, want 200", got)
	}
}

func TestArtifactRejectsMalformedDigest(t *testing.T) {
	h := newHub(t)
	enrolled := h.enroll(t, h.mintToken(t, nil), fullCaps())

	resp := h.putArtifact(t, enrolled.Credential, "not-a-digest", []byte("x"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandlerImplementsAgentLink(t *testing.T) {
	var _ core.AgentLink = (*link)(nil)
}

// ------------------------------------------------------------------ helpers

// waitFor polls until cond holds, because the agent plane is asynchronous by
// construction: events travel one way and are applied by the read loop.
func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition did not hold within %s", within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
