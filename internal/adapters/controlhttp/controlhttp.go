// Package controlhttp is the Control Plane: the northbound REST API that
// automations and the Operator Console drive.
//
// "Northbound" means toward the operator. Everything here is a thin translation
// between HTTP and core.Fleet -- no domain logic lives in this package, which is
// what makes a second transport (gRPC) a matter of writing another adapter
// rather than refactoring.
//
// The Operator Console is a client of exactly these endpoints. If the console
// can do something this API cannot, the API is incomplete.
package controlhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tayyebi/scraper/internal/auth"
	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/registry"
)

// SessionCookie is the Operator Console's cookie name. It is defined here
// because the console authenticates against this API like any other client.
const SessionCookie = "hub_console"

// DefaultMaxWait bounds ?wait=. A caller asking to block for an hour is asking
// to hold a connection open through every proxy in between, which will drop it
// anyway; better to answer with a command id it can poll.
const DefaultMaxWait = 5 * time.Minute

// Handler serves the Control API.
type Handler struct {
	fleet    core.Fleet
	registry *registry.Registry
	store    core.Store
	blobs    core.BlobStore
	auth     *auth.Service
	log      *slog.Logger

	maxWait time.Duration
	version string
}

// Options configures the Control Plane.
type Options struct {
	Registry *registry.Registry
	Store    core.Store
	Blobs    core.BlobStore
	Auth     *auth.Service
	Logger   *slog.Logger
	MaxWait  time.Duration
	Version  string
}

// New builds the Control Plane handler.
func New(opts Options) *Handler {
	h := &Handler{
		fleet:    opts.Registry,
		registry: opts.Registry,
		store:    opts.Store,
		blobs:    opts.Blobs,
		auth:     opts.Auth,
		log:      opts.Logger,
		maxWait:  opts.MaxWait,
		version:  opts.Version,
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.maxWait <= 0 {
		h.maxWait = DefaultMaxWait
	}
	if h.version == "" {
		h.version = "dev"
	}
	return h
}

// Routes registers the Control API.
//
// Patterns use the stdlib mux's method-and-wildcard syntax, which is why this
// project needs no router dependency.
func (h *Handler) Routes(mux *http.ServeMux) {
	// Enrollment and agents.
	mux.HandleFunc("POST /v1/enrollments", h.require(core.ScopeAdmin, h.mintEnrollment))
	mux.HandleFunc("GET /v1/agents", h.require(core.ScopeRead, h.listAgents))
	mux.HandleFunc("GET /v1/agents/{id}", h.require(core.ScopeRead, h.getAgent))
	mux.HandleFunc("DELETE /v1/agents/{id}", h.require(core.ScopeAdmin, h.revokeAgent))
	mux.HandleFunc("POST /v1/agents/{id}/sessions", h.require(core.ScopeSteer, h.openSession))

	// Sessions.
	mux.HandleFunc("GET /v1/sessions", h.require(core.ScopeRead, h.listSessions))
	mux.HandleFunc("GET /v1/sessions/{id}", h.require(core.ScopeRead, h.getSession))
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.require(core.ScopeSteer, h.closeSession))

	// Steering.
	mux.HandleFunc("POST /v1/sessions/{id}/commands", h.require(core.ScopeSteer, h.postCommand))
	mux.HandleFunc("GET /v1/commands/{id}", h.require(core.ScopeRead, h.getCommand))

	// Observation.
	mux.HandleFunc("GET /v1/sessions/{id}/dom", h.require(core.ScopeRead, h.getDOM))
	mux.HandleFunc("GET /v1/sessions/{id}/events", h.require(core.ScopeRead, h.streamEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/requests", h.require(core.ScopeRead, h.listRequests))
	mux.HandleFunc("GET /v1/sessions/{id}/har", h.require(core.ScopeRead, h.exportHAR))
	mux.HandleFunc("GET /v1/artifacts/{digest}", h.require(core.ScopeRead, h.getArtifact))

	// Keys, so the console can manage credentials through the same API.
	mux.HandleFunc("GET /v1/apikeys", h.require(core.ScopeAdmin, h.listAPIKeys))
	mux.HandleFunc("POST /v1/apikeys", h.require(core.ScopeAdmin, h.mintAPIKey))
	mux.HandleFunc("DELETE /v1/apikeys/{id}", h.require(core.ScopeAdmin, h.revokeAPIKey))

	mux.HandleFunc("GET /v1/meta", h.require(core.ScopeRead, h.meta))
}

// ------------------------------------------------------------------- auth

type identityKey struct{}

// Identity is who made a request.
type Identity struct {
	Kind  string // "apikey" or "console"
	ID    string
	Name  string
	Scope core.Scope
}

// IdentityFrom returns the caller's identity, for handlers that need it.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// require authenticates and authorizes, then calls next.
//
// Two credential shapes are accepted because they have genuinely different
// theft models: a bearer token an automation holds, and a cookie a browser
// holds. Treating a cookie as a bearer token is how CSRF happens.
func (h *Handler) require(scope core.Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, err := h.authenticate(r)
		if err != nil {
			// The WWW-Authenticate header tells a caller which credential the
			// endpoint actually wants, rather than leaving them to guess.
			w.Header().Set("WWW-Authenticate", `Bearer realm="hub"`)
			writeError(w, http.StatusUnauthorized, "a valid API key or console session is required")
			return
		}
		if !identity.Scope.Implies(scope) {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("this endpoint needs the %q scope; your credential has %q", scope, identity.Scope))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, identity)))
	}
}

func (h *Handler) authenticate(r *http.Request) (Identity, error) {
	ctx := r.Context()

	if bearer := bearerToken(r); bearer != "" {
		key, err := h.auth.AuthenticateAPIKey(ctx, bearer)
		if err != nil {
			return Identity{}, err
		}
		// AuthenticateAPIKey already records last use, best-effort.
		return Identity{Kind: "apikey", ID: key.ID, Name: key.Name, Scope: key.Scope}, nil
	}

	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		sess, err := h.auth.AuthenticateConsole(ctx, c.Value)
		if err != nil {
			return Identity{}, err
		}
		// An operator who logged into the console is an operator.
		return Identity{Kind: "console", ID: sess.ID, Name: sess.User, Scope: core.ScopeAdmin}, nil
	}

	return Identity{}, core.ErrUnauthorized
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// ------------------------------------------------------------------ agents

type enrollmentRequest struct {
	Labels    map[string]string `json:"labels,omitempty"`
	TTLSecond int               `json:"ttlSeconds,omitempty"`
}

type enrollmentResponse struct {
	ID        string            `json:"id"`
	Token     string            `json:"token"`
	Labels    map[string]string `json:"labels,omitempty"`
	ExpiresAt time.Time         `json:"expiresAt"`
	Note      string            `json:"note"`
}

func (h *Handler) mintEnrollment(w http.ResponseWriter, r *http.Request) {
	var req enrollmentRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	token, secret, err := h.auth.MintEnrollment(r.Context(), req.Labels, time.Duration(req.TTLSecond)*time.Second)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, enrollmentResponse{
		ID:        token.ID,
		Token:     secret,
		Labels:    token.Labels,
		ExpiresAt: token.ExpiresAt,
		Note:      "This token is shown once, is good for a single agent, and is spent on first use.",
	})
}

// agentView is an Agent plus what its capture mode cannot see. The gaps travel
// with the agent so a caller learns about them before dispatching, not after
// reading a request log that looked complete.
type agentView struct {
	core.Agent
	CaptureGaps []string `json:"captureGaps,omitempty"`
	FullFidelity bool    `json:"fullFidelityCapture"`
}

func viewAgent(a core.Agent) agentView {
	return agentView{
		Agent:        a,
		CaptureGaps:  a.Capabilities.CaptureGaps(),
		FullFidelity: a.Capabilities.Capture.FullFidelity(),
	}
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.fleet.ListAgents(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	views := make([]agentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, viewAgent(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": views})
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.fleet.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewAgent(a))
}

func (h *Handler) revokeAgent(w http.ResponseWriter, r *http.Request) {
	if err := h.fleet.RevokeAgent(r.Context(), r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) openSession(w http.ResponseWriter, r *http.Request) {
	var req core.OpenSessionRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "a url is required to open a session")
		return
	}

	sess, err := h.fleet.OpenSession(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/sessions/"+sess.ID)
	writeJSON(w, http.StatusCreated, sess)
}

// ----------------------------------------------------------------- API keys

func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.auth.ListAPIKeys(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (h *Handler) mintAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string     `json:"name"`
		Scope core.Scope `json:"scope"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	key, secret, err := h.auth.MintAPIKey(r.Context(), req.Name, req.Scope)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":   key,
		"token": secret,
		"note":  "This key is shown once and is not recoverable. Store it now.",
	})
}

func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.RevokeAPIKey(r.Context(), r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) meta(w http.ResponseWriter, r *http.Request) {
	identity, _ := IdentityFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         h.version,
		"protocolVersion": 1,
		"commands":        core.KnownOps,
		"identity": map[string]any{
			"kind":  identity.Kind,
			"name":  identity.Name,
			"scope": identity.Scope,
		},
		"agentsOnline": h.registry.Online(),
	})
}

// ----------------------------------------------------------------- helpers

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("unreadable request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeDomainError maps core's vocabulary onto HTTP. This mapping is the only
// place the domain and the transport meet, which is what keeps core free of
// status codes.
func writeDomainError(w http.ResponseWriter, err error) {
	var unsupported *core.ErrUnsupported
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, core.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, core.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, core.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, core.ErrBadRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, core.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, core.ErrSessionClosed):
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &unsupported):
		// 422: the request was understood and is well-formed, but this agent
		// cannot carry it out. A 400 would suggest the caller sent something
		// malformed, which they did not.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, core.ErrAgentOffline):
		// 503 with Retry-After: the agent is expected back.
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, core.ErrTimeout):
		writeError(w, http.StatusGatewayTimeout, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func intParam(r *http.Request, name string, fallback int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func boolParam(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
