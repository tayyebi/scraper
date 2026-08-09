package agentws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/wire"
)

// link is one live command channel, implementing core.AgentLink.
//
// Correlation lives here rather than in the registry because this type owns the
// socket: it is the only thing reading frames, so it is the only thing that can
// match a result to the command that asked for it.
type link struct {
	agentID string
	caps    core.Capabilities
	conn    *wire.Conn
	hub     *Handler
	log     *slog.Logger

	mu      sync.Mutex
	pending map[string]chan wire.Envelope
	closed  bool
}

func newLink(h *Handler, agentID string, caps core.Capabilities, conn *wire.Conn) *link {
	return &link{
		agentID: agentID,
		caps:    caps,
		conn:    conn,
		hub:     h,
		log:     h.log.With("agent", agentID),
		pending: make(map[string]chan wire.Envelope),
	}
}

func (l *link) AgentID() string                 { return l.agentID }
func (l *link) Capabilities() core.Capabilities { return l.caps }

// Call sends a command and waits for the correlated result.
func (l *link) Call(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error) {
	id := core.NewID(core.PrefixCommand)

	// Buffered so a result arriving just as the caller gives up does not block
	// the read loop -- which would stall every other session on this channel.
	reply := make(chan wire.Envelope, 1)

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: the agent's channel is closed", core.ErrAgentOffline)
	}
	l.pending[id] = reply
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		delete(l.pending, id)
		l.mu.Unlock()
	}()

	if err := l.conn.WriteEnvelope(wire.NewCmd(id, sessionID, op, params)); err != nil {
		return nil, fmt.Errorf("%w: writing to the agent channel: %v", core.ErrAgentOffline, err)
	}

	select {
	case env := <-reply:
		if env.Kind == wire.KindErr && env.Err != nil {
			if env.Err.Code == wire.CodeUnsupported {
				return nil, &core.ErrUnsupported{AgentID: l.agentID, Op: op, Reason: env.Err.Message}
			}
			return nil, &core.CommandFailure{Op: op, Code: env.Err.Code, Message: env.Err.Message}
		}
		return env.Body, nil

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close tears the channel down with a reason the agent will log.
func (l *link) Close(reason string) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	return l.conn.Close(wire.CloseNormal, reason)
}

// serve runs the read loop until the channel drops. It is the only reader.
func (l *link) serve(ctx context.Context) {
	pingInterval := l.hub.pingInterval
	readTimeout := l.hub.readTimeout

	// Any inbound traffic proves the peer is alive, so the read deadline is
	// pushed forward on every frame -- including the pongs answering our pings.
	extend := func() { _ = l.conn.SetReadDeadline(time.Now().Add(readTimeout)) }
	extend()
	l.conn.SetOnPong(func([]byte) { extend() })

	// Chrome evicts an MV3 service worker after ~30s idle but counts WebSocket
	// traffic as activity, so this ping is not only liveness detection: it is
	// what keeps the agent's own worker alive. See docs/protocol.md.
	stopPing := make(chan struct{})
	var pingWG sync.WaitGroup
	pingWG.Add(1)
	go func() {
		defer pingWG.Done()
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := l.conn.Ping(nil); err != nil {
					return
				}
			}
		}
	}()

	defer func() {
		close(stopPing)
		pingWG.Wait()
		l.failPending()
	}()

	for {
		env, err := l.conn.ReadEnvelope()
		if err != nil {
			if wire.IsCleanClose(err) {
				l.log.Info("agent channel closed")
			} else if wire.IsBadEnvelope(err) {
				l.log.Warn("agent sent a malformed envelope", "err", err)
				_ = l.conn.Close(wire.ClosePolicyViolation, "malformed envelope")
			} else {
				l.log.Info("agent channel dropped", "err", err)
			}
			return
		}
		extend()

		switch env.Kind {
		case wire.KindRes, wire.KindErr:
			l.deliver(env)
		case wire.KindEvt:
			l.hub.handleEvent(ctx, l, env)
		case wire.KindCmd:
			// Agents do not issue commands to the hub. Accepting them would
			// invert the direction of control the whole design rests on.
			_ = l.conn.WriteEnvelope(wire.NewErr(env.ID, env.SID, wire.CodeDenied,
				"agents do not send commands to the hub"))
		}
	}
}

func (l *link) deliver(env wire.Envelope) {
	l.mu.Lock()
	reply, ok := l.pending[env.ID]
	l.mu.Unlock()
	if !ok {
		// A result for a command that already timed out. Expected, not an
		// error: the caller gave up, and the agent could not have known.
		l.log.Debug("result for an unknown or expired command", "command", env.ID)
		return
	}
	select {
	case reply <- env:
	default:
	}
}

// failPending unblocks every in-flight Call when the channel dies, so callers
// fail immediately instead of each burning its full deadline.
func (l *link) failPending() {
	l.mu.Lock()
	l.closed = true
	pending := l.pending
	l.pending = make(map[string]chan wire.Envelope)
	l.mu.Unlock()

	for id, reply := range pending {
		select {
		case reply <- wire.NewErr(id, "", wire.CodeDetached, "the agent's channel dropped before it answered"):
		default:
		}
	}
}
