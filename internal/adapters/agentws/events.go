package agentws

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/mirror"
	"github.com/tayyebi/scraper/internal/store/blob"
	"github.com/tayyebi/scraper/internal/wire"
)

// Event ops an agent may send unsolicited. These are the agent's vocabulary for
// "something happened", as distinct from results, which answer a command.
const (
	evtSessionAttached = "session.attached"
	evtSessionUpdated  = "session.updated"
	evtSessionClosed   = "session.closed"
	evtNavigated       = "navigated"
	evtMirrorSnapshot  = "mirror.snapshot"
	evtMirrorMutation  = "mirror.mutation"
	evtExchange        = "exchange"
	evtConsole         = "console"
)

// handleEvent routes one unsolicited event from an agent.
//
// Everything here treats the agent as untrusted input. It is a browser
// extension living alongside pages that would like to influence it, and being
// authenticated says who sent a message, not that its contents are sane.
func (h *Handler) handleEvent(ctx context.Context, l *link, env wire.Envelope) {
	switch env.Op {
	case evtSessionAttached, evtSessionUpdated:
		h.onSessionUpsert(ctx, l, env)
	case evtSessionClosed:
		h.onSessionClosed(ctx, l, env)
	case evtNavigated:
		h.onNavigated(ctx, l, env)
	case evtMirrorSnapshot:
		h.onMirrorSnapshot(ctx, l, env)
	case evtMirrorMutation:
		h.onMirrorMutation(ctx, l, env)
	case evtExchange:
		h.onExchange(ctx, l, env)
	default:
		// Unknown events are recorded rather than rejected: a newer agent
		// reporting something this hub does not model yet is not an error, and
		// dropping it would lose the only trace it happened.
		h.record(ctx, l, env, defaultString(env.Op, core.EventError))
	}
}

type sessionEvent struct {
	SessionID string `json:"sessionId"`
	TabID     int64  `json:"tabId"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Origin    string `json:"origin"`
}

func (h *Handler) onSessionUpsert(ctx context.Context, l *link, env wire.Envelope) {
	var body sessionEvent
	if !decodeBody(env, &body) {
		return
	}
	sessionID := firstNonEmpty(body.SessionID, env.SID)
	if sessionID == "" {
		l.log.Warn("session event without a session id", "op", env.Op)
		return
	}

	// An agent proposing "managed" for a tab a human handed over would let it
	// launder provenance, so anything the agent volunteers is attached. The
	// registry additionally refuses to rewrite the origin of a session that
	// already exists.
	origin := core.OriginAttached
	if env.Op == evtSessionUpdated && body.Origin == string(core.OriginManaged) {
		origin = core.OriginManaged
	}

	sess := core.Session{
		ID:      sessionID,
		AgentID: l.agentID,
		TabID:   body.TabID,
		URL:     body.URL,
		Title:   body.Title,
		Origin:  origin,
		State:   core.SessionOpen,
	}
	if err := h.registry.UpsertSession(ctx, sess); err != nil {
		l.log.Error("recording a session", "session", sessionID, "err", err)
		return
	}
	h.record(ctx, l, env, core.EventSessionOpened)
}

func (h *Handler) onSessionClosed(ctx context.Context, l *link, env wire.Envelope) {
	var body sessionEvent
	decodeBody(env, &body)
	sessionID := firstNonEmpty(body.SessionID, env.SID)
	if sessionID == "" {
		return
	}

	sess, err := h.registry.GetSession(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			l.log.Error("looking up a closing session", "session", sessionID, "err", err)
		}
		return
	}
	if sess.AgentID != l.agentID {
		// An agent may only close its own sessions.
		l.log.Warn("agent tried to close another agent's session", "session", sessionID)
		return
	}

	now := time.Now().UTC()
	sess.State = core.SessionClosed
	sess.ClosedAt = &now
	if err := h.store.PutSession(ctx, sess); err != nil {
		l.log.Error("closing a session", "session", sessionID, "err", err)
	}
	if h.mirrors != nil {
		h.mirrors.Drop(sessionID)
	}
	h.record(ctx, l, env, core.EventSessionClosed)
}

func (h *Handler) onNavigated(ctx context.Context, l *link, env wire.Envelope) {
	var body struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	decodeBody(env, &body)

	if env.SID != "" {
		if sess, err := h.registry.GetSession(ctx, env.SID); err == nil && sess.AgentID == l.agentID {
			sess.URL = body.URL
			sess.Title = body.Title
			if err := h.store.PutSession(ctx, sess); err != nil {
				l.log.Error("recording a navigation", "session", env.SID, "err", err)
			}
		}
		// The old document is gone. Serving the mirror of a page that no longer
		// exists would be the worst kind of wrong: confidently stale.
		if h.mirrors != nil {
			if m, ok := h.mirrors.Lookup(env.SID); ok {
				m.MarkStale()
			}
		}
	}
	h.record(ctx, l, env, core.EventNavigated)
}

type snapshotEvent struct {
	FrameID string     `json:"frameId"`
	URL     string     `json:"url"`
	Title   string     `json:"title"`
	Root    *core.Node `json:"root"`
}

func (h *Handler) onMirrorSnapshot(ctx context.Context, l *link, env wire.Envelope) {
	if h.mirrors == nil || env.SID == "" {
		return
	}
	var body snapshotEvent
	if !decodeBody(env, &body) {
		return
	}
	if body.Root == nil {
		l.log.Warn("mirror snapshot without a root node", "session", env.SID)
		return
	}

	frameID := firstNonEmpty(body.FrameID, mirror.MainFrame)
	h.mirrors.For(env.SID).ApplySnapshot(frameID, core.Document{
		SessionID:  env.SID,
		FrameID:    frameID,
		URL:        body.URL,
		Title:      body.Title,
		Root:       body.Root,
		CapturedAt: env.Time(),
	}, env.Seq)

	h.record(ctx, l, env, core.EventMirrorSnapshot)
}

func (h *Handler) onMirrorMutation(ctx context.Context, l *link, env wire.Envelope) {
	if h.mirrors == nil || env.SID == "" {
		return
	}
	var body struct {
		Ops []mirror.Op `json:"ops"`
	}
	if !decodeBody(env, &body) {
		return
	}

	m := h.mirrors.For(env.SID)
	err := m.ApplyMutations(env.Seq, body.Ops)
	switch {
	case err == nil:
		h.record(ctx, l, env, core.EventMirrorMutation)

	case errors.Is(err, mirror.ErrSeqGap), errors.Is(err, mirror.ErrNoMirror):
		// This is the whole point of sequencing. The deltas that would close
		// the hole are gone, so the mirror stops serving and a fresh snapshot
		// is demanded. Doing this automatically is what keeps a caller from
		// ever reading a document that is quietly wrong.
		l.log.Info("mirror gap; demanding a re-snapshot", "session", env.SID, "err", err)
		h.demandResnapshot(l, env.SID)
		h.record(ctx, l, env, core.EventMirrorGap)

	default:
		l.log.Error("applying mutations", "session", env.SID, "err", err)
	}
}

// demandResnapshot asks the agent for a full snapshot, off the read loop.
//
// It must not block: the read loop is the only reader on this channel, and the
// agent's answer arrives on that same loop, so waiting here would deadlock the
// connection against itself.
func (h *Handler) demandResnapshot(l *link, sessionID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := l.Call(ctx, sessionID, core.OpSnapshotDOM, nil); err != nil {
			l.log.Warn("re-snapshot failed", "session", sessionID, "err", err)
		}
	}()
}

func (h *Handler) onExchange(ctx context.Context, l *link, env wire.Envelope) {
	var x core.Exchange
	if !decodeBody(env, &x) {
		return
	}
	x.SessionID = firstNonEmpty(x.SessionID, env.SID)
	x.AgentID = l.agentID
	if x.SessionID == "" {
		return
	}
	if x.StartedAt.IsZero() {
		x.StartedAt = env.Time()
	}

	// Digests are checked for shape but not for presence: an agent legitimately
	// reports an exchange before its body upload finishes, and rejecting the
	// row would lose the exchange to win nothing.
	x.ReqBody = sanitizeDigest(x.ReqBody)
	x.ResBody = sanitizeDigest(x.ResBody)

	if err := h.registry.RecordExchange(ctx, x); err != nil {
		l.log.Error("recording an exchange", "session", x.SessionID, "err", err)
	}
}

// record persists an agent event under a hub event type and publishes it.
func (h *Handler) record(ctx context.Context, l *link, env wire.Envelope, eventType string) {
	if err := h.registry.RecordEvent(ctx, core.Event{
		AgentID:   l.agentID,
		SessionID: env.SID,
		Type:      eventType,
		Seq:       env.Seq,
		Body:      env.Body,
		At:        env.Time(),
	}); err != nil {
		l.log.Error("recording an event", "type", eventType, "err", err)
	}
}

// ------------------------------------------------------------------ helpers

func decodeBody(env wire.Envelope, dst any) bool {
	if len(env.Body) == 0 {
		return false
	}
	return json.Unmarshal(env.Body, dst) == nil
}

// sanitizeDigest keeps a malformed digest out of the request log rather than
// rejecting the whole exchange. An unparseable digest is dropped to empty: the
// exchange is still worth recording, and a reference that could never resolve
// is worse than no reference.
func sanitizeDigest(s string) string {
	if s == "" {
		return ""
	}
	d := blob.NormalizeDigest(s)
	if !blob.ValidDigest(d) {
		return ""
	}
	return d
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
