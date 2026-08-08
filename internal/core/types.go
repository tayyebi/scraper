// Package core is the domain: types and ports, no I/O.
//
// Every plane is an adapter over this package, and this package imports no
// adapter. That is the whole reason a second Control Plane transport (gRPC)
// can be added without touching anything here.
package core

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------- agents

// AgentStatus is connection state, not health. An agent is Online exactly while
// it holds a command channel; there is no "unhealthy" -- a browser either
// answers or it does not.
type AgentStatus string

const (
	AgentOnline  AgentStatus = "online"
	AgentOffline AgentStatus = "offline"
)

// CaptureMode names how an agent observes network traffic. The modes are not
// interchangeable and the difference is visible to callers, which is the point
// of reporting it: a caller that needs document bodies must not be handed a
// fetch-patch agent and a log that silently omits them.
type CaptureMode string

const (
	// CaptureNone means the agent records no exchanges at all.
	CaptureNone CaptureMode = "none"

	// CaptureFetchPatch patches fetch and XMLHttpRequest in the page's MAIN
	// world. Silent and store-legal, and it covers the JSON/XHR traffic most
	// scraping targets actually carry -- but it cannot see document
	// navigations, subresource loads, or anything issued outside those two
	// APIs (img, script, EventSource, WebSocket, service-worker fetches).
	CaptureFetchPatch CaptureMode = "fetch-patch"

	// CaptureFilterResponse uses Firefox's webRequest.filterResponseData.
	// Full fidelity, no debugger banner, no extra permission prompt.
	CaptureFilterResponse CaptureMode = "filter-response"

	// CaptureDebugger uses chrome.debugger/CDP. Full fidelity, but it shows the
	// browser's "is being debugged" infobar and is opt-in per agent.
	CaptureDebugger CaptureMode = "debugger"
)

// FullFidelity reports whether the mode observes every exchange rather than
// only the ones that happen to go through a patched JS API.
func (m CaptureMode) FullFidelity() bool {
	return m == CaptureFilterResponse || m == CaptureDebugger
}

// Capabilities is what an agent advertises at enrollment. It is surfaced
// verbatim on the Control API so a caller can tell, before dispatching, whether
// the answer it gets back will be complete.
type Capabilities struct {
	Capture    CaptureMode `json:"capture"`
	Eval       bool        `json:"eval"`
	Screenshot bool        `json:"screenshot"`
	Cookies    bool        `json:"cookies"`
	Attach     bool        `json:"attach"`
	OpenTabs   bool        `json:"openTabs"`
	Mirror     bool        `json:"mirror"`
	Ops        []string    `json:"ops"`
}

// Supports reports whether the agent advertised the given command op.
func (c Capabilities) Supports(op string) bool {
	for _, s := range c.Ops {
		if s == op {
			return true
		}
	}
	return false
}

// CaptureGaps describes, in plain words, what this agent's capture mode will
// miss. The Control API returns it alongside the request log rather than
// letting a caller assume the log is exhaustive.
func (c Capabilities) CaptureGaps() []string {
	switch c.Capture {
	case CaptureNone:
		return []string{"this agent records no network traffic"}
	case CaptureFetchPatch:
		return []string{
			"document navigations are not recorded",
			"subresources (img, script, css, media) are not recorded",
			"requests issued by service workers are not recorded",
			"WebSocket and EventSource traffic is not recorded",
			"a page that captures a reference to fetch before the patch installs is not recorded",
		}
	default:
		return nil
	}
}

type Agent struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Labels       map[string]string `json:"labels,omitempty"`
	Capabilities Capabilities      `json:"capabilities"`
	Status       AgentStatus       `json:"status"`
	Browser      string            `json:"browser,omitempty"`
	BrowserVer   string            `json:"browserVersion,omitempty"`
	Platform     string            `json:"platform,omitempty"`
	UserAgent    string            `json:"userAgent,omitempty"`
	AgentVersion string            `json:"agentVersion,omitempty"`
	EnrolledAt   time.Time         `json:"enrolledAt"`
	LastSeenAt   time.Time         `json:"lastSeenAt"`
}

// --------------------------------------------------------------- sessions

// SessionOrigin distinguishes the two ways a tab comes under control. The
// distinction is not cosmetic: an attached tab belongs to a human who was
// using it, so closing it is rude and the console says so.
type SessionOrigin string

const (
	// OriginManaged means the hub opened this tab.
	OriginManaged SessionOrigin = "managed"

	// OriginAttached means a human handed over a tab they already had open.
	// This is the point of BYOB: it is how you drive a logged-in session.
	OriginAttached SessionOrigin = "attached"
)

type SessionState string

const (
	SessionOpen   SessionState = "open"
	SessionClosed SessionState = "closed"
)

type Session struct {
	ID        string        `json:"id"`
	AgentID   string        `json:"agentId"`
	TabID     int64         `json:"tabId"`
	URL       string        `json:"url"`
	Title     string        `json:"title"`
	Origin    SessionOrigin `json:"origin"`
	State     SessionState  `json:"state"`
	CreatedAt time.Time     `json:"createdAt"`
	ClosedAt  *time.Time    `json:"closedAt,omitempty"`
}

// OpenSessionRequest asks an agent to open a managed tab.
type OpenSessionRequest struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
	Window string `json:"window,omitempty"`
}

type SessionFilter struct {
	AgentID string
	State   SessionState
	Origin  SessionOrigin
	Limit   int
}

// -------------------------------------------------------------- commands

type CommandState string

const (
	CommandPending    CommandState = "pending"
	CommandDispatched CommandState = "dispatched"
	CommandDone       CommandState = "done"
	CommandError      CommandState = "error"
	CommandTimeout    CommandState = "timeout"
)

// Terminal reports whether the command will never change state again.
func (s CommandState) Terminal() bool {
	return s == CommandDone || s == CommandError || s == CommandTimeout
}

// Commands v1. Anything not on this list is rejected at the edge rather than
// forwarded to an agent that would not understand it.
const (
	OpNavigate    = "navigate"
	OpWaitFor     = "waitFor"
	OpClick       = "click"
	OpType        = "type"
	OpScroll      = "scroll"
	OpSelect      = "select"
	OpEval        = "eval"
	OpSnapshotDOM = "snapshotDom"
	OpExtract     = "extract"
	OpScreenshot  = "screenshot"
	OpCookies     = "cookies"
	OpBack        = "back"
	OpForward     = "forward"
	OpReload      = "reload"
	OpClose       = "close"
)

// KnownOps is the v1 command vocabulary, in the order the docs list it.
var KnownOps = []string{
	OpNavigate, OpWaitFor, OpClick, OpType, OpScroll, OpSelect, OpEval,
	OpSnapshotDOM, OpExtract, OpScreenshot, OpCookies, OpBack, OpForward,
	OpReload, OpClose,
}

// IsKnownOp reports whether op is part of the v1 vocabulary.
func IsKnownOp(op string) bool {
	for _, s := range KnownOps {
		if s == op {
			return true
		}
	}
	return false
}

type Command struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	AgentID   string          `json:"agentId"`
	Op        string          `json:"op"`
	Params    json.RawMessage `json:"params,omitempty"`
	State     CommandState    `json:"state"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	DeadlineAt time.Time      `json:"deadlineAt"`
	DoneAt    *time.Time      `json:"doneAt,omitempty"`
}

// ---------------------------------------------------------------- events

// Event types on the telemetry stream. These are distinct from the request log:
// the request log is a queryable table of exchanges, the event stream is a live
// feed of things happening.
const (
	EventSessionOpened  = "session.opened"
	EventSessionClosed  = "session.closed"
	EventNavigated      = "navigated"
	EventTitleChanged   = "title.changed"
	EventMirrorSnapshot = "mirror.snapshot"
	EventMirrorMutation = "mirror.mutation"
	EventMirrorGap      = "mirror.gap"
	EventExchange       = "exchange"
	EventConsole        = "console"
	EventCommandResult  = "command.result"
	EventAgentOnline    = "agent.online"
	EventAgentOffline   = "agent.offline"
	EventError          = "error"
)

type Event struct {
	ID        int64           `json:"id"`
	AgentID   string          `json:"agentId,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Type      string          `json:"type"`
	Seq       int64           `json:"seq,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
	At        time.Time       `json:"at"`
}

// ------------------------------------------------------------ request log

// Exchange is one request/response pair in a session's request log. Bodies are
// not inline: they are artifacts in the blob store, referenced by digest, so an
// exchange row stays small no matter how large the response was.
type Exchange struct {
	ID          int64             `json:"id"`
	SessionID   string            `json:"sessionId"`
	AgentID     string            `json:"agentId"`
	RequestID   string            `json:"requestId"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Status      int               `json:"status"`
	StatusText  string            `json:"statusText,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	ResourceType string           `json:"resourceType,omitempty"`
	Initiator   string            `json:"initiator,omitempty"`
	ReqHeaders  map[string]string `json:"requestHeaders,omitempty"`
	ResHeaders  map[string]string `json:"responseHeaders,omitempty"`
	ReqBody     string            `json:"requestBodyDigest,omitempty"`
	ResBody     string            `json:"responseBodyDigest,omitempty"`
	ReqBodySize int64             `json:"requestBodySize,omitempty"`
	ResBodySize int64             `json:"responseBodySize,omitempty"`
	FromCache   bool              `json:"fromCache,omitempty"`
	Truncated   bool              `json:"truncated,omitempty"`
	StartedAt   time.Time         `json:"startedAt"`
	DurationMs  int64             `json:"durationMs"`
}

// -------------------------------------------------------------- artifacts

// BlobInfo describes stored bytes. Digest is the sha256 hex of the content, and
// it is the only name a blob has.
type BlobInfo struct {
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	MimeType  string    `json:"mimeType,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// ------------------------------------------------------------------- DOM

// NodeType mirrors the DOM's nodeType constants so a snapshot round-trips
// without a translation table on either side.
type NodeType int

const (
	NodeElement  NodeType = 1
	NodeText     NodeType = 3
	NodeComment  NodeType = 8
	NodeDocument NodeType = 9
	NodeDoctype  NodeType = 10
	NodeFragment NodeType = 11
)

type Attr struct {
	Name  string `json:"n"`
	Value string `json:"v"`
}

// Node is one node of a mirrored document. Field names are deliberately short:
// a mutation stream on a busy page is mostly this struct, repeated.
type Node struct {
	ID     int64    `json:"id"`
	Type   NodeType `json:"t"`
	Name   string   `json:"n,omitempty"`
	Value  string   `json:"v,omitempty"`
	Attrs  []Attr   `json:"a,omitempty"`
	Kids   []*Node  `json:"c,omitempty"`
	Shadow *Node    `json:"sr,omitempty"`
	Frame  string   `json:"f,omitempty"`
}

// Document is a materialized mirror of one frame, plus its child frames.
//
// Seq is the mutation sequence number this document reflects. A caller that
// sees Seq stall while the page is visibly changing is looking at a stale
// mirror, which is why the number is exposed rather than hidden.
type Document struct {
	SessionID  string      `json:"sessionId"`
	FrameID    string      `json:"frameId"`
	URL        string      `json:"url"`
	Title      string      `json:"title,omitempty"`
	Seq        int64       `json:"seq"`
	CapturedAt time.Time   `json:"capturedAt"`
	Root       *Node       `json:"root"`
	Frames     []*Document `json:"frames,omitempty"`
}

// ------------------------------------------------------------------ auth

// Scope is an API key's permission. Scopes are coarse on purpose: three levels
// a human can reason about beat twenty a human will get wrong.
type Scope string

const (
	// ScopeRead can list and read: agents, sessions, DOM, events, request log.
	ScopeRead Scope = "read"

	// ScopeSteer can additionally dispatch commands and open sessions.
	ScopeSteer Scope = "steer"

	// ScopeAdmin can additionally mint enrollment tokens, revoke agents, and
	// manage API keys.
	ScopeAdmin Scope = "admin"
)

// Implies reports whether holding s grants want. Scopes nest: admin implies
// steer implies read.
func (s Scope) Implies(want Scope) bool {
	rank := map[Scope]int{ScopeRead: 1, ScopeSteer: 2, ScopeAdmin: 3}
	return rank[s] >= rank[want] && rank[s] > 0 && rank[want] > 0
}

// APIKey is a Control API credential. The secret itself is never stored -- only
// its hash -- so a database leak does not yield usable keys.
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     Scope      `json:"scope"`
	CreatedAt time.Time  `json:"createdAt"`
	LastUsed  *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

// EnrollmentToken is a one-time secret that an agent exchanges for a
// credential. It is deliberately short-lived and single-use: see GLOSSARY.md
// for why this is split from the credential rather than being one reusable
// pairing secret.
type EnrollmentToken struct {
	ID        string            `json:"id"`
	Hash      string            `json:"-"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	ExpiresAt time.Time         `json:"expiresAt"`
	UsedAt    *time.Time        `json:"usedAt,omitempty"`
	UsedBy    string            `json:"usedByAgentId,omitempty"`
}

// Spent reports whether the token can no longer be exchanged.
func (t EnrollmentToken) Spent(now time.Time) bool {
	return t.UsedAt != nil || now.After(t.ExpiresAt)
}

// ConsoleSession is an operator's cookie session for the console. It is
// separate from API keys on purpose: a browser cookie and a bearer token have
// different theft models and different revocation needs.
type ConsoleSession struct {
	ID        string    `json:"id"`
	Hash      string    `json:"-"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}
