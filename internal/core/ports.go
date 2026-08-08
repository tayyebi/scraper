package core

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// Fleet is the northbound port: everything an operator or an automation can do.
//
// The Operator Console consumes exactly this, through the same HTTP adapter any
// other client uses. If the console needs something Fleet does not expose, the
// API is incomplete -- that is the invariant this port exists to enforce.
type Fleet interface {
	ListAgents(ctx context.Context) ([]Agent, error)
	GetAgent(ctx context.Context, agentID string) (Agent, error)
	RevokeAgent(ctx context.Context, agentID string) error

	OpenSession(ctx context.Context, agentID string, req OpenSessionRequest) (Session, error)
	ListSessions(ctx context.Context, f SessionFilter) ([]Session, error)
	GetSession(ctx context.Context, sessionID string) (Session, error)
	CloseSession(ctx context.Context, sessionID string) error

	// Dispatch sends a command. If wait is zero the command is queued and
	// returned in state pending; otherwise the call blocks for up to wait for a
	// terminal state. Both forms return the same Command, which is what lets a
	// caller poll GetCommand after a timed-out wait rather than losing the
	// result.
	Dispatch(ctx context.Context, sessionID, op string, params json.RawMessage, wait time.Duration) (Command, error)
	GetCommand(ctx context.Context, commandID string) (Command, error)

	// DOM returns the materialized mirror. With fresh=false it answers from the
	// hub's own copy and never touches the agent; with fresh=true it forces a
	// round trip and a re-snapshot.
	DOM(ctx context.Context, sessionID string, fresh bool) (Document, error)

	Exchanges(ctx context.Context, sessionID string, limit int) ([]Exchange, error)

	// Subscribe returns a live event feed for a session, and a cancel function
	// that must be called or the subscription leaks. Delivery is lossy under
	// backpressure by design -- see EventBus.
	Subscribe(ctx context.Context, sessionID string) (<-chan Event, func(), error)
}

// AgentLink is one live command channel. The agentws adapter implements it; the
// registry holds them by agent id.
//
// It is an interface rather than a struct so the registry can be tested without
// a socket, and so a future transport (a native host, a phone-side gRPC stream)
// can be routed to identically.
type AgentLink interface {
	AgentID() string
	Capabilities() Capabilities

	// Call sends a command and blocks until the agent's correlated result
	// arrives, ctx is done, or the channel drops.
	Call(ctx context.Context, sessionID, op string, params json.RawMessage) (json.RawMessage, error)

	// Close tears down the channel with a reason the agent will log.
	Close(reason string) error
}

// Store is the metadata persistence port. Bodies never pass through it -- those
// go to BlobStore -- so every method here is small and indexable.
type Store interface {
	PutAgent(ctx context.Context, a Agent) error
	GetAgent(ctx context.Context, id string) (Agent, error)
	ListAgents(ctx context.Context) ([]Agent, error)
	SetAgentStatus(ctx context.Context, id string, status AgentStatus, at time.Time) error
	DeleteAgent(ctx context.Context, id string) error

	PutSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, id string) (Session, error)
	ListSessions(ctx context.Context, f SessionFilter) ([]Session, error)
	CloseSessionsForAgent(ctx context.Context, agentID string, at time.Time) error

	PutCommand(ctx context.Context, c Command) error
	GetCommand(ctx context.Context, id string) (Command, error)
	CompleteCommand(ctx context.Context, id string, state CommandState, result json.RawMessage, errMsg string, at time.Time) error

	AppendEvent(ctx context.Context, e Event) (Event, error)
	ListEvents(ctx context.Context, sessionID string, afterID int64, limit int) ([]Event, error)

	AppendExchange(ctx context.Context, x Exchange) (Exchange, error)
	ListExchanges(ctx context.Context, sessionID string, limit int) ([]Exchange, error)

	// Retention drops events and exchanges older than before, and returns the
	// digests whose last reference just disappeared so the blob store can
	// collect them.
	Retention(ctx context.Context, before time.Time) ([]string, error)

	Close() error
}

// AuthStore persists credentials. Split from Store only for readability; one
// SQLite implementation satisfies both.
type AuthStore interface {
	PutEnrollmentToken(ctx context.Context, t EnrollmentToken) error

	// SpendEnrollmentToken atomically marks a token used and returns it. It
	// must be atomic: two agents racing the same token is exactly the case a
	// one-time token exists to prevent.
	SpendEnrollmentToken(ctx context.Context, hash string, agentID string, now time.Time) (EnrollmentToken, error)

	PutAgentCredential(ctx context.Context, agentID, hash string, now time.Time) error
	AgentByCredential(ctx context.Context, hash string) (string, error)
	DeleteAgentCredential(ctx context.Context, agentID string) error

	PutAPIKey(ctx context.Context, k APIKey, hash string) error
	APIKeyByHash(ctx context.Context, hash string) (APIKey, error)
	ListAPIKeys(ctx context.Context) ([]APIKey, error)
	RevokeAPIKey(ctx context.Context, id string, at time.Time) error
	TouchAPIKey(ctx context.Context, id string, at time.Time) error

	PutConsoleSession(ctx context.Context, s ConsoleSession) error
	ConsoleSessionByHash(ctx context.Context, hash string) (ConsoleSession, error)
	DeleteConsoleSession(ctx context.Context, id string) error
}

// BlobStore is the content-addressed artifact store. Content addressing is not
// decoration: identical response bodies captured a thousand times cost one copy,
// and an agent can skip an upload it knows the hub already has.
type BlobStore interface {
	// Put streams r into the store, verifying that its sha256 equals digest.
	// A mismatch is an error and stores nothing -- the name is the checksum, so
	// a blob that does not match its name must never exist.
	Put(ctx context.Context, digest string, r io.Reader, mimeType string, limit int64) (BlobInfo, error)
	Open(ctx context.Context, digest string) (io.ReadSeekCloser, BlobInfo, error)
	Stat(ctx context.Context, digest string) (BlobInfo, error)
	Has(ctx context.Context, digest string) bool
	Delete(ctx context.Context, digest string) error

	// Sweep enforces retention: drop blobs older than maxAge, then drop oldest
	// blobs until the store is under maxBytes. Returns how many it removed.
	Sweep(ctx context.Context, maxAge time.Duration, maxBytes int64, keep func(digest string) bool) (int, error)
}

// EventBus is in-process pub/sub for the live event stream.
//
// Delivery is deliberately lossy: a slow SSE client must never be able to stall
// the agent that is producing events. Subscribers get a bounded buffer and see
// a drop marker when they fall behind, which is honest, whereas unbounded
// queueing would turn one stuck browser tab into an out-of-memory kill.
type EventBus interface {
	Publish(e Event)
	Subscribe(sessionID string, buffer int) (<-chan Event, func())
	SubscribeAll(buffer int) (<-chan Event, func())
	Close()
}
