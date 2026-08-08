// Package registry is the command router: it holds the live agent channels and
// turns Control API calls into commands on them.
//
// It is the only place that knows both halves. The Control Plane knows about
// requests, the Agent Plane knows about sockets, and neither knows the other
// exists -- everything they share meets here, behind core.Fleet.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tayyebi/scraper/internal/bus"
	"github.com/tayyebi/scraper/internal/core"
)

// Agent-scoped ops. These are addressed to an agent rather than a session, so
// they are not part of the v1 session command vocabulary in core.KnownOps.
const (
	opOpenTab  = "openTab"
	opCloseTab = "closeTab"
)

// DefaultCommandTimeout bounds a command that gave no deadline of its own.
//
// Deliberately generous: the far end is a real browser doing real work on a
// real network, and a scraper waiting on a slow page is not a stuck scraper.
const DefaultCommandTimeout = 30 * time.Second

// DocumentSource supplies materialized DOM mirrors. internal/mirror implements
// it; the registry only needs to ask whether a fresh-enough copy exists.
type DocumentSource interface {
	Document(sessionID string) (core.Document, bool)
}

// Registry implements core.Fleet.
type Registry struct {
	store core.Store
	blobs core.BlobStore
	bus   core.EventBus
	docs  DocumentSource
	log   *slog.Logger

	mu    sync.RWMutex
	links map[string]core.AgentLink
}

// Options configures a Registry.
type Options struct {
	Store     core.Store
	Blobs     core.BlobStore
	Bus       core.EventBus
	Documents DocumentSource
	Logger    *slog.Logger
}

// New builds a Registry.
func New(opts Options) *Registry {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		store: opts.Store,
		blobs: opts.Blobs,
		bus:   opts.Bus,
		docs:  opts.Documents,
		log:   log,
		links: make(map[string]core.AgentLink),
	}
}

// SetDocumentSource wires the mirror in after construction, which keeps the
// dependency one-way: the mirror knows the registry, not the reverse.
func (r *Registry) SetDocumentSource(d DocumentSource) { r.docs = d }

// ------------------------------------------------------------ live channels

// Attach registers a live command channel and marks the agent online.
func (r *Registry) Attach(ctx context.Context, link core.AgentLink) {
	agentID := link.AgentID()

	r.mu.Lock()
	previous, replaced := r.links[agentID]
	r.links[agentID] = link
	r.mu.Unlock()

	if replaced && previous != link {
		// A browser that reconnects before the hub noticed the old socket died
		// would otherwise leave a channel nothing reads from.
		r.log.Info("replacing a stale channel", "agent", agentID)
		_ = previous.Close("replaced by a newer channel from the same agent")
	}

	if err := r.store.SetAgentStatus(ctx, agentID, core.AgentOnline, time.Now().UTC()); err != nil {
		r.log.Error("marking agent online", "agent", agentID, "err", err)
	}
	r.publish(ctx, bus.Event("", agentID, core.EventAgentOnline, map[string]any{"agentId": agentID}))
}

// Detach removes a channel and marks the agent offline.
//
// It also closes the agent's sessions. Leaving them open would advertise tabs
// that cannot be steered, and every command against one would burn a full
// deadline before failing.
func (r *Registry) Detach(ctx context.Context, link core.AgentLink) {
	agentID := link.AgentID()

	r.mu.Lock()
	// Only remove if this is still the current link: a reconnect may already
	// have installed a newer one, and that one must survive.
	if current, ok := r.links[agentID]; ok && current == link {
		delete(r.links, agentID)
	} else {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	now := time.Now().UTC()
	if err := r.store.SetAgentStatus(ctx, agentID, core.AgentOffline, now); err != nil {
		r.log.Error("marking agent offline", "agent", agentID, "err", err)
	}
	if err := r.store.CloseSessionsForAgent(ctx, agentID, now); err != nil {
		r.log.Error("closing sessions of a departed agent", "agent", agentID, "err", err)
	}
	r.publish(ctx, bus.Event("", agentID, core.EventAgentOffline, map[string]any{"agentId": agentID}))
}

// Link returns the live channel for an agent.
func (r *Registry) Link(agentID string) (core.AgentLink, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.links[agentID]
	return l, ok
}

// Online reports how many agents currently hold a channel.
func (r *Registry) Online() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.links)
}

// ------------------------------------------------------------------- agents

func (r *Registry) ListAgents(ctx context.Context) ([]core.Agent, error) {
	agents, err := r.store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	// The database records what was last written; the link map is the truth
	// about right now. A hub that was killed mid-flight has rows saying
	// "online" for agents that are not.
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range agents {
		if _, ok := r.links[agents[i].ID]; ok {
			agents[i].Status = core.AgentOnline
		} else {
			agents[i].Status = core.AgentOffline
		}
	}
	return agents, nil
}

func (r *Registry) GetAgent(ctx context.Context, agentID string) (core.Agent, error) {
	a, err := r.store.GetAgent(ctx, agentID)
	if err != nil {
		return core.Agent{}, err
	}
	if _, ok := r.Link(agentID); ok {
		a.Status = core.AgentOnline
	} else {
		a.Status = core.AgentOffline
	}
	return a, nil
}

// RevokeAgent drops the credential, closes the channel, and deletes the agent.
//
// Order matters: the credential goes first, so a race where the agent
// reconnects between steps cannot succeed.
func (r *Registry) RevokeAgent(ctx context.Context, agentID string) error {
	if _, err := r.store.GetAgent(ctx, agentID); err != nil {
		return err
	}
	if link, ok := r.Link(agentID); ok {
		_ = link.Close("this agent's credential was revoked")
		r.Detach(ctx, link)
	}
	return r.store.DeleteAgent(ctx, agentID)
}

// ----------------------------------------------------------------- sessions

// OpenSession asks an agent to open a managed tab.
//
// The session id is minted here and handed to the agent, rather than the agent
// proposing one. The hub is the only party that can guarantee ids do not
// collide across agents, and an agent that could choose its own could name a
// session belonging to somebody else.
func (r *Registry) OpenSession(ctx context.Context, agentID string, req core.OpenSessionRequest) (core.Session, error) {
	link, ok := r.Link(agentID)
	if !ok {
		return core.Session{}, fmt.Errorf("%w: agent %s holds no command channel", core.ErrAgentOffline, agentID)
	}
	if caps := link.Capabilities(); !caps.OpenTabs {
		return core.Session{}, &core.ErrUnsupported{
			AgentID: agentID,
			Op:      opOpenTab,
			Reason:  "this agent cannot open tabs; attach an existing tab from its popup instead",
		}
	}

	sessionID := core.NewID(core.PrefixSession)
	params, err := json.Marshal(map[string]any{
		"sessionId": sessionID,
		"url":       req.URL,
		"active":    req.Active,
		"window":    req.Window,
	})
	if err != nil {
		return core.Session{}, err
	}

	callCtx, cancel := context.WithTimeout(ctx, DefaultCommandTimeout)
	defer cancel()

	raw, err := link.Call(callCtx, sessionID, opOpenTab, params)
	if err != nil {
		return core.Session{}, err
	}

	var res struct {
		TabID int64  `json:"tabId"`
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			return core.Session{}, fmt.Errorf("agent returned an unreadable openTab result: %w", err)
		}
	}

	sess := core.Session{
		ID:        sessionID,
		AgentID:   agentID,
		TabID:     res.TabID,
		URL:       cmp(res.URL, req.URL),
		Title:     res.Title,
		Origin:    core.OriginManaged,
		State:     core.SessionOpen,
		CreatedAt: time.Now().UTC(),
	}
	if err := r.store.PutSession(ctx, sess); err != nil {
		return core.Session{}, err
	}
	r.publish(ctx, bus.Event(sess.ID, agentID, core.EventSessionOpened, sess))
	return sess, nil
}

// UpsertSession records a session the agent told us about -- an attached tab,
// or a managed tab whose URL or title changed.
func (r *Registry) UpsertSession(ctx context.Context, sess core.Session) error {
	existing, err := r.store.GetSession(ctx, sess.ID)
	isNew := err != nil && errors.Is(err, core.ErrNotFound)
	if err != nil && !isNew {
		return err
	}
	if !isNew {
		// Never let an update rewrite provenance or creation time.
		sess.Origin = existing.Origin
		sess.CreatedAt = existing.CreatedAt
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	if sess.State == "" {
		sess.State = core.SessionOpen
	}
	if err := r.store.PutSession(ctx, sess); err != nil {
		return err
	}
	if isNew {
		r.publish(ctx, bus.Event(sess.ID, sess.AgentID, core.EventSessionOpened, sess))
	}
	return nil
}

func (r *Registry) ListSessions(ctx context.Context, f core.SessionFilter) ([]core.Session, error) {
	return r.store.ListSessions(ctx, f)
}

func (r *Registry) GetSession(ctx context.Context, sessionID string) (core.Session, error) {
	return r.store.GetSession(ctx, sessionID)
}

// CloseSession closes a tab. For an attached session the tab belonged to a
// human before it belonged to us, so the agent is asked to detach rather than
// to close the window out from under them; that policy lives agent-side, and
// the origin is passed along so it can apply it.
func (r *Registry) CloseSession(ctx context.Context, sessionID string) error {
	sess, err := r.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.State == core.SessionClosed {
		return nil
	}

	if link, ok := r.Link(sess.AgentID); ok {
		params, _ := json.Marshal(map[string]any{"origin": string(sess.Origin)})
		callCtx, cancel := context.WithTimeout(ctx, DefaultCommandTimeout)
		defer cancel()
		if _, err := link.Call(callCtx, sessionID, opCloseTab, params); err != nil {
			r.log.Warn("agent refused or failed to close a tab", "session", sessionID, "err", err)
		}
	}

	now := time.Now().UTC()
	sess.State = core.SessionClosed
	sess.ClosedAt = &now
	if err := r.store.PutSession(ctx, sess); err != nil {
		return err
	}
	r.publish(ctx, bus.Event(sessionID, sess.AgentID, core.EventSessionClosed, sess))
	return nil
}

// ----------------------------------------------------------------- commands

// Dispatch sends a command to the agent holding the session.
//
// With wait == 0 the command is queued and a pending Command is returned
// immediately; otherwise the call blocks for up to wait. Both paths write the
// same row, which is what lets a caller whose wait expired poll GetCommand
// instead of losing the result.
func (r *Registry) Dispatch(ctx context.Context, sessionID, op string, params json.RawMessage, wait time.Duration) (core.Command, error) {
	if !core.IsKnownOp(op) {
		return core.Command{}, fmt.Errorf("%w: %q is not a v1 command", core.ErrBadRequest, op)
	}

	sess, err := r.store.GetSession(ctx, sessionID)
	if err != nil {
		return core.Command{}, err
	}
	if sess.State != core.SessionOpen {
		return core.Command{}, fmt.Errorf("%w: session %s", core.ErrSessionClosed, sessionID)
	}

	link, ok := r.Link(sess.AgentID)
	if !ok {
		return core.Command{}, fmt.Errorf("%w: agent %s holds no command channel", core.ErrAgentOffline, sess.AgentID)
	}

	// Refusing here rather than forwarding is the difference between a clear
	// error and a command that times out for a reason nobody can see.
	if caps := link.Capabilities(); len(caps.Ops) > 0 && !caps.Supports(op) {
		return core.Command{}, &core.ErrUnsupported{
			AgentID: sess.AgentID,
			Op:      op,
			Reason:  "the agent did not advertise this op at enrollment",
		}
	}

	timeout := wait
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	now := time.Now().UTC()
	cmd := core.Command{
		ID:         core.NewID(core.PrefixCommand),
		SessionID:  sessionID,
		AgentID:    sess.AgentID,
		Op:         op,
		Params:     params,
		State:      core.CommandPending,
		CreatedAt:  now,
		DeadlineAt: now.Add(timeout),
	}
	if err := r.store.PutCommand(ctx, cmd); err != nil {
		return core.Command{}, err
	}

	if wait <= 0 {
		// Detached from ctx on purpose: the caller's HTTP request is about to
		// end, and cancelling the command with it would make an accepted
		// command silently not happen.
		go func() {
			bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cancel()
			r.run(bg, cmd, link)
		}()
		return cmd, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return r.run(callCtx, cmd, link), nil
}

// run executes a command and records its terminal state.
func (r *Registry) run(ctx context.Context, cmd core.Command, link core.AgentLink) core.Command {
	raw, err := link.Call(ctx, cmd.SessionID, cmd.Op, cmd.Params)
	done := time.Now().UTC()
	cmd.DoneAt = &done

	switch {
	case err == nil:
		cmd.State = core.CommandDone
		cmd.Result = raw
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, core.ErrTimeout):
		cmd.State = core.CommandTimeout
		cmd.Error = fmt.Sprintf("the agent did not answer within %s", cmd.DeadlineAt.Sub(cmd.CreatedAt))
	default:
		cmd.State = core.CommandError
		cmd.Error = err.Error()
	}

	// Recording uses a background context: the caller's context is very often
	// exactly what just expired, and a timeout that fails to be written is a
	// command stuck in "pending" forever.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := r.store.CompleteCommand(recCtx, cmd.ID, cmd.State, cmd.Result, cmd.Error, done); err != nil {
		r.log.Error("recording command result", "command", cmd.ID, "err", err)
	}
	r.publish(recCtx, bus.Event(cmd.SessionID, cmd.AgentID, core.EventCommandResult, map[string]any{
		"commandId": cmd.ID,
		"op":        cmd.Op,
		"state":     cmd.State,
		"error":     cmd.Error,
	}))
	return cmd
}

func (r *Registry) GetCommand(ctx context.Context, commandID string) (core.Command, error) {
	return r.store.GetCommand(ctx, commandID)
}

// ---------------------------------------------------------------------- DOM

// DOM returns the materialized mirror.
//
// With fresh == false this answers from the hub's own copy and never touches
// the agent -- that is the entire reason the mirror exists. fresh == true, or a
// missing mirror, forces a round trip and a re-snapshot.
func (r *Registry) DOM(ctx context.Context, sessionID string, fresh bool) (core.Document, error) {
	if !fresh && r.docs != nil {
		if doc, ok := r.docs.Document(sessionID); ok {
			return doc, nil
		}
	}

	cmd, err := r.Dispatch(ctx, sessionID, core.OpSnapshotDOM, nil, DefaultCommandTimeout)
	if err != nil {
		return core.Document{}, err
	}
	if cmd.State != core.CommandDone {
		return core.Document{}, &core.CommandFailure{Op: core.OpSnapshotDOM, Code: string(cmd.State), Message: cmd.Error}
	}

	// The agent's snapshot lands in the mirror through the event stream, so by
	// the time the command completes the materialized copy is authoritative.
	if r.docs != nil {
		if doc, ok := r.docs.Document(sessionID); ok {
			return doc, nil
		}
	}

	var doc core.Document
	if err := json.Unmarshal(cmd.Result, &doc); err != nil {
		return core.Document{}, fmt.Errorf("agent returned an unreadable DOM snapshot: %w", err)
	}
	doc.SessionID = sessionID
	return doc, nil
}

// ------------------------------------------------------------- observability

func (r *Registry) Exchanges(ctx context.Context, sessionID string, limit int) ([]core.Exchange, error) {
	return r.store.ListExchanges(ctx, sessionID, limit)
}

// Subscribe returns a live event feed for one session.
func (r *Registry) Subscribe(ctx context.Context, sessionID string) (<-chan core.Event, func(), error) {
	if _, err := r.store.GetSession(ctx, sessionID); err != nil {
		return nil, nil, err
	}
	ch, cancel := r.bus.Subscribe(sessionID, 0)
	return ch, cancel, nil
}

// RecordEvent persists an agent event and publishes it. The Agent Plane calls
// it for everything an agent reports unsolicited.
func (r *Registry) RecordEvent(ctx context.Context, e core.Event) error {
	stored, err := r.store.AppendEvent(ctx, e)
	if err != nil {
		return err
	}
	r.bus.Publish(stored)
	return nil
}

// RecordExchange appends to a session's request log.
func (r *Registry) RecordExchange(ctx context.Context, x core.Exchange) error {
	stored, err := r.store.AppendExchange(ctx, x)
	if err != nil {
		return err
	}
	r.bus.Publish(bus.Event(x.SessionID, x.AgentID, core.EventExchange, map[string]any{
		"id":     stored.ID,
		"method": stored.Method,
		"url":    stored.URL,
		"status": stored.Status,
	}))
	return nil
}

// publish persists an event and fans it out, logging rather than failing: an
// event that cannot be stored must not break the operation that produced it.
func (r *Registry) publish(ctx context.Context, e core.Event) {
	stored, err := r.store.AppendEvent(ctx, e)
	if err != nil {
		r.log.Error("appending event", "type", e.Type, "err", err)
		r.bus.Publish(e)
		return
	}
	r.bus.Publish(stored)
}

// cmp returns a when it is non-empty, otherwise b.
func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
