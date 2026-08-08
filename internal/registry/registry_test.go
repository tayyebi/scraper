package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tayyebi/scraper/internal/bus"
	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/store/sqlite"
)

// fakeLink stands in for a live agent channel. The registry only ever talks to
// core.AgentLink, so a socket is not needed to test routing.
type fakeLink struct {
	id   string
	caps core.Capabilities

	mu       sync.Mutex
	calls    []string
	lastArgs json.RawMessage
	closed   string

	// respond is called for every Call. Nil means "return {} successfully".
	// It receives the call's context so a fake can honour a deadline the way a
	// real channel does.
	respond func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error)
}

func (f *fakeLink) AgentID() string                { return f.id }
func (f *fakeLink) Capabilities() core.Capabilities { return f.caps }

func (f *fakeLink) Call(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, op)
	f.lastArgs = params
	respond := f.respond
	f.mu.Unlock()

	if respond != nil {
		return respond(ctx, sessionID, op, params)
	}
	return json.RawMessage(`{}`), nil
}

func (f *fakeLink) Close(reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = reason
	return nil
}

func (f *fakeLink) callsMade() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLink) closeReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func fullCaps() core.Capabilities {
	return core.Capabilities{
		Capture:  core.CaptureDebugger,
		OpenTabs: true,
		Attach:   true,
		Mirror:   true,
		Ops:      core.KnownOps,
	}
}

type harness struct {
	reg   *Registry
	store *sqlite.Store
	bus   *bus.Bus
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	b := bus.New()
	t.Cleanup(b.Close)

	reg := New(Options{
		Store:  st,
		Bus:    b,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &harness{reg: reg, store: st, bus: b}
}

// enroll registers an agent row and attaches a live link.
func (h *harness) enroll(t *testing.T, id string, caps core.Capabilities) *fakeLink {
	t.Helper()
	ctx := context.Background()
	if err := h.store.PutAgent(ctx, core.Agent{
		ID: id, Name: id, Capabilities: caps, Status: core.AgentOffline,
		EnrolledAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}
	link := &fakeLink{id: id, caps: caps}
	h.reg.Attach(ctx, link)
	return link
}

func TestAttachAndDetachTrackLiveness(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	if h.reg.Online() != 1 {
		t.Errorf("Online = %d, want 1", h.reg.Online())
	}
	a, err := h.reg.GetAgent(ctx, "a_1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.Status != core.AgentOnline {
		t.Errorf("status = %q, want online", a.Status)
	}

	h.reg.Detach(ctx, link)
	if h.reg.Online() != 0 {
		t.Errorf("Online = %d after detach, want 0", h.reg.Online())
	}
	a, err = h.reg.GetAgent(ctx, "a_1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.Status != core.AgentOffline {
		t.Errorf("status = %q, want offline", a.Status)
	}
}

// A hub that was killed leaves rows saying "online" for agents that are not.
// The link map is the truth about now, so listing must correct the rows.
func TestListAgentsCorrectsStaleStatus(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.store.PutAgent(ctx, core.Agent{
		ID: "a_ghost", Status: core.AgentOnline,
		EnrolledAt: time.Now(), LastSeenAt: time.Now(),
	}); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}

	agents, err := h.reg.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("got %d agents", len(agents))
	}
	if agents[0].Status != core.AgentOffline {
		t.Errorf("status = %q, want offline: the row outlived the channel", agents[0].Status)
	}
}

// A browser reconnecting before the hub noticed the old socket died must not
// leave a channel nobody reads.
func TestReattachClosesTheStaleChannel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first := h.enroll(t, "a_1", fullCaps())
	second := &fakeLink{id: "a_1", caps: fullCaps()}
	h.reg.Attach(ctx, second)

	if first.closeReason() == "" {
		t.Error("the superseded channel was not closed")
	}
	if got, _ := h.reg.Link("a_1"); got != core.AgentLink(second) {
		t.Error("the registry did not switch to the new channel")
	}

	// Detaching the stale link must not remove the live one.
	h.reg.Detach(ctx, first)
	if h.reg.Online() != 1 {
		t.Error("detaching a superseded channel removed the live one")
	}
}

// Sessions of a departed agent must close, or every command against one burns a
// full deadline before failing.
func TestDetachClosesSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	sess, err := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	h.reg.Detach(ctx, link)

	got, err := h.reg.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.State != core.SessionClosed {
		t.Errorf("session state = %q after the agent left, want closed", got.State)
	}
}

func TestOpenSessionMintsTheIDHubSide(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"tabId":42,"url":"https://example.test/landed","title":"Landed"}`), nil
	}

	sess, err := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	if !core.HasPrefix(sess.ID, core.PrefixSession) {
		t.Errorf("session id %q is not a hub-minted session id", sess.ID)
	}
	if sess.Origin != core.OriginManaged {
		t.Errorf("origin = %q, want managed", sess.Origin)
	}
	if sess.TabID != 42 || sess.Title != "Landed" {
		t.Errorf("agent's result was not recorded: %+v", sess)
	}

	// The agent must have been told which session id it was opening.
	var params map[string]any
	link.mu.Lock()
	args := link.lastArgs
	link.mu.Unlock()
	if err := json.Unmarshal(args, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	if params["sessionId"] != sess.ID {
		t.Errorf("agent was sent sessionId %v, want %s", params["sessionId"], sess.ID)
	}
}

func TestOpenSessionRequiresTheCapability(t *testing.T) {
	h := newHarness(t)
	caps := fullCaps()
	caps.OpenTabs = false
	h.enroll(t, "a_1", caps)

	_, err := h.reg.OpenSession(context.Background(), "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	var unsupported *core.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if unsupported.Reason == "" {
		t.Error("ErrUnsupported gave no reason, so the caller cannot tell what to do instead")
	}
}

func TestOpenSessionOnOfflineAgent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.PutAgent(ctx, core.Agent{ID: "a_1", EnrolledAt: time.Now(), LastSeenAt: time.Now()}); err != nil {
		t.Fatalf("PutAgent: %v", err)
	}
	_, err := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	if !errors.Is(err, core.ErrAgentOffline) {
		t.Errorf("err = %v, want ErrAgentOffline", err)
	}
}

func TestDispatchSynchronous(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		if op == opOpenTab {
			return json.RawMessage(`{"tabId":1}`), nil
		}
		return json.RawMessage(`{"title":"Example"}`), nil
	}

	sess, err := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	cmd, err := h.reg.Dispatch(ctx, sess.ID, core.OpNavigate, json.RawMessage(`{"url":"https://example.test/2"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmd.State != core.CommandDone {
		t.Errorf("state = %q, want done (error: %s)", cmd.State, cmd.Error)
	}
	if string(cmd.Result) != `{"title":"Example"}` {
		t.Errorf("result = %s", cmd.Result)
	}

	// And it is durable, so a caller can re-read it.
	stored, err := h.reg.GetCommand(ctx, cmd.ID)
	if err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if stored.State != core.CommandDone {
		t.Errorf("stored state = %q, want done", stored.State)
	}
}

// An accepted command must still run after the caller's HTTP request ends.
func TestDispatchAsyncSurvivesCallerCancellation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	released := make(chan struct{})
	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		if op == opOpenTab {
			return json.RawMessage(`{"tabId":1}`), nil
		}
		<-released
		return json.RawMessage(`{"done":true}`), nil
	}

	sess, err := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	reqCtx, cancel := context.WithCancel(ctx)
	cmd, err := h.reg.Dispatch(reqCtx, sess.ID, core.OpClick, nil, 0)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmd.State != core.CommandPending {
		t.Errorf("state = %q, want pending", cmd.State)
	}

	cancel() // the HTTP request ends here
	close(released)

	deadline := time.Now().Add(3 * time.Second)
	for {
		stored, err := h.reg.GetCommand(ctx, cmd.ID)
		if err != nil {
			t.Fatalf("GetCommand: %v", err)
		}
		if stored.State.Terminal() {
			if stored.State != core.CommandDone {
				t.Fatalf("state = %q (%s), want done: cancelling the request killed an accepted command", stored.State, stored.Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the queued command never completed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDispatchRecordsAgentErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		if op == opOpenTab {
			return json.RawMessage(`{"tabId":1}`), nil
		}
		return nil, errors.New("selector matched nothing")
	}

	sess, _ := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	cmd, err := h.reg.Dispatch(ctx, sess.ID, core.OpClick, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmd.State != core.CommandError {
		t.Errorf("state = %q, want error", cmd.State)
	}
	if cmd.Error == "" {
		t.Error("the agent's reason was not recorded")
	}
}

func TestDispatchTimesOut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		if op == opOpenTab {
			return json.RawMessage(`{"tabId":1}`), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	sess, _ := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	cmd, err := h.reg.Dispatch(ctx, sess.ID, core.OpWaitFor, nil, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmd.State != core.CommandTimeout {
		t.Errorf("state = %q, want timeout", cmd.State)
	}
}

func TestDispatchRejectsUnknownOps(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.enroll(t, "a_1", fullCaps())
	sess, _ := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})

	_, err := h.reg.Dispatch(ctx, sess.ID, "rm -rf /", nil, time.Second)
	if !errors.Is(err, core.ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
}

// Refusing an unadvertised op is the difference between a clear error and a
// command that burns its deadline for a reason nobody can see.
func TestDispatchRejectsUnadvertisedOps(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	caps := fullCaps()
	caps.Ops = []string{core.OpNavigate, core.OpClose} // no eval
	link := h.enroll(t, "a_1", caps)
	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"tabId":1}`), nil
	}

	sess, err := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	_, err = h.reg.Dispatch(ctx, sess.ID, core.OpEval, nil, time.Second)
	var unsupported *core.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if len(link.callsMade()) != 1 {
		t.Errorf("calls = %v: the unsupported op was forwarded to the agent anyway", link.callsMade())
	}
}

func TestDispatchOnClosedSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.enroll(t, "a_1", fullCaps())
	sess, _ := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})
	if err := h.reg.CloseSession(ctx, sess.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	_, err := h.reg.Dispatch(ctx, sess.ID, core.OpNavigate, nil, time.Second)
	if !errors.Is(err, core.ErrSessionClosed) {
		t.Errorf("err = %v, want ErrSessionClosed", err)
	}
}

// An attached tab belonged to a human first, so the agent is told the origin
// and applies the policy itself.
func TestCloseSessionTellsTheAgentTheOrigin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	if err := h.reg.UpsertSession(ctx, core.Session{
		ID: "s_attached", AgentID: "a_1", TabID: 9,
		Origin: core.OriginAttached, State: core.SessionOpen,
		URL: "https://mail.example.test",
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	if err := h.reg.CloseSession(ctx, "s_attached"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	var params map[string]any
	link.mu.Lock()
	args := link.lastArgs
	link.mu.Unlock()
	if err := json.Unmarshal(args, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	if params["origin"] != string(core.OriginAttached) {
		t.Errorf("agent was told origin %v, want attached", params["origin"])
	}
}

// An update from an agent must not be able to relabel a session's provenance.
func TestUpsertSessionPreservesOrigin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.reg.UpsertSession(ctx, core.Session{
		ID: "s_1", AgentID: "a_1", Origin: core.OriginAttached, State: core.SessionOpen,
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := h.reg.UpsertSession(ctx, core.Session{
		ID: "s_1", AgentID: "a_1", Origin: core.OriginManaged, State: core.SessionOpen,
		Title: "now with a title",
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got, err := h.reg.GetSession(ctx, "s_1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Origin != core.OriginAttached {
		t.Errorf("origin = %q, want attached: an update rewrote provenance", got.Origin)
	}
	if got.Title != "now with a title" {
		t.Errorf("title was not updated: %q", got.Title)
	}
}

func TestRevokeAgentClosesTheChannel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	if err := h.reg.RevokeAgent(ctx, "a_1"); err != nil {
		t.Fatalf("RevokeAgent: %v", err)
	}
	if link.closeReason() == "" {
		t.Error("revoking an agent left its channel open")
	}
	if h.reg.Online() != 0 {
		t.Error("the revoked agent is still counted as online")
	}
	if _, err := h.reg.GetAgent(ctx, "a_1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound after revocation", err)
	}
}

// The mirror exists so that reading the DOM costs no round trip.
func TestDOMAnswersFromTheMirrorWithoutTouchingTheAgent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	sess, _ := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})

	h.reg.SetDocumentSource(stubDocs{sessionID: sess.ID})

	before := len(link.callsMade())
	doc, err := h.reg.DOM(ctx, sess.ID, false)
	if err != nil {
		t.Fatalf("DOM: %v", err)
	}
	if doc.SessionID != sess.ID {
		t.Errorf("document is for %q, want %q", doc.SessionID, sess.ID)
	}
	if got := len(link.callsMade()); got != before {
		t.Errorf("the agent was called %d extra times for a cached mirror read", got-before)
	}
}

func TestDOMFreshForcesARoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	link := h.enroll(t, "a_1", fullCaps())
	link.respond = func(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
		if op == opOpenTab {
			return json.RawMessage(`{"tabId":1}`), nil
		}
		return json.RawMessage(`{"url":"https://example.test","seq":7,"root":{"id":1,"t":9}}`), nil
	}
	sess, _ := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})

	before := len(link.callsMade())
	doc, err := h.reg.DOM(ctx, sess.ID, true)
	if err != nil {
		t.Fatalf("DOM: %v", err)
	}
	if len(link.callsMade()) == before {
		t.Error("fresh=1 did not force a round trip")
	}
	if doc.Seq != 7 || doc.Root == nil {
		t.Errorf("document = %+v", doc)
	}
}

type stubDocs struct{ sessionID string }

func (s stubDocs) Document(sessionID string) (core.Document, bool) {
	if sessionID != s.sessionID {
		return core.Document{}, false
	}
	return core.Document{SessionID: sessionID, Seq: 3, Root: &core.Node{ID: 1, Type: core.NodeDocument}}, true
}

func TestSubscribeDeliversSessionEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.enroll(t, "a_1", fullCaps())
	sess, _ := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})

	ch, cancel, err := h.reg.Subscribe(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if err := h.reg.RecordEvent(ctx, core.Event{
		SessionID: sess.ID, AgentID: "a_1", Type: core.EventNavigated,
		Body: json.RawMessage(`{"url":"https://example.test/next"}`), At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	select {
	case e := <-ch:
		if e.Type != core.EventNavigated {
			t.Errorf("event = %q, want navigated", e.Type)
		}
		if e.ID == 0 {
			t.Error("the delivered event has no store id, so it cannot be resumed from")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered")
	}
}

func TestSubscribeToUnknownSession(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.reg.Subscribe(context.Background(), "s_nope"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRecordExchangeAppearsInTheRequestLog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.enroll(t, "a_1", fullCaps())
	sess, _ := h.reg.OpenSession(ctx, "a_1", core.OpenSessionRequest{URL: "https://example.test"})

	if err := h.reg.RecordExchange(ctx, core.Exchange{
		SessionID: sess.ID, AgentID: "a_1", Method: "GET",
		URL: "https://api.example.test/items", Status: 200, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordExchange: %v", err)
	}

	log, err := h.reg.Exchanges(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("Exchanges: %v", err)
	}
	if len(log) != 1 || log[0].URL != "https://api.example.test/items" {
		t.Errorf("request log = %+v", log)
	}
}

// The registry is the one type both planes hold, so it must satisfy the port
// they are written against.
func TestRegistryImplementsFleet(t *testing.T) {
	var _ core.Fleet = (*Registry)(nil)
}
