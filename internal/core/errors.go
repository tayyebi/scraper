package core

import (
	"errors"
	"fmt"
)

// Domain errors. Adapters map these to their own vocabulary -- controlhttp to
// status codes, agentws to close codes -- so the core never mentions HTTP.
var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrForbidden      = errors.New("forbidden")
	ErrAgentOffline   = errors.New("agent offline")
	ErrSessionClosed  = errors.New("session closed")
	ErrTimeout        = errors.New("deadline exceeded")
	ErrTooLarge       = errors.New("payload too large")
	ErrDigestMismatch = errors.New("digest mismatch")
	ErrBadRequest     = errors.New("bad request")
)

// ErrUnsupported reports that an agent cannot do what was asked. It carries the
// specifics because "unsupported" alone sends the caller reading source: the
// useful version names the op and what the agent actually advertised.
type ErrUnsupported struct {
	AgentID string
	Op      string
	Reason  string
}

func (e *ErrUnsupported) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("agent %s cannot %s: %s", e.AgentID, e.Op, e.Reason)
	}
	return fmt.Sprintf("agent %s cannot %s", e.AgentID, e.Op)
}

// CommandFailure is a failure reported by the agent itself, as opposed to a
// failure to reach the agent. The distinction matters to a caller: a retry
// fixes an unreachable agent and will not fix a selector that matched nothing.
//
// Named for the failure rather than the state because CommandError is already
// the CommandState constant for that state.
type CommandFailure struct {
	Op      string
	Code    string
	Message string
}

func (e *CommandFailure) Error() string {
	return fmt.Sprintf("%s failed: %s (%s)", e.Op, e.Message, e.Code)
}
