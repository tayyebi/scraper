// Package agentws is the Agent Plane: the ingress browsers dial in to.
//
// Three endpoints, and the split between them is the design:
//
//	POST /agent/v1/enroll             spend a one-time token, get a credential
//	GET  /agent/v1/channel            upgrade to the command channel
//	PUT  /agent/v1/artifacts/{digest} upload captured bytes
//
// Bulk bytes deliberately do not cross the channel. A captured response body
// would head-of-line block every command multiplexed onto the same socket, so
// artifacts go over ordinary HTTP -- which also makes them parallel, retryable,
// and skippable when the hub already holds the digest.
package agentws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tayyebi/scraper/internal/auth"
	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/mirror"
	"github.com/tayyebi/scraper/internal/registry"
	"github.com/tayyebi/scraper/internal/store/blob"
	"github.com/tayyebi/scraper/internal/wire"
)

// Defaults.
const (
	// DefaultPingInterval is well under Chrome's ~30s MV3 service-worker idle
	// eviction, because WebSocket traffic is what resets that timer.
	DefaultPingInterval = 20 * time.Second

	// DefaultReadTimeout is generous relative to the ping interval: a phone on
	// a slow network may take a while to answer, and dropping a live agent is
	// worse than noticing a dead one late.
	DefaultReadTimeout = 70 * time.Second

	// DefaultMaxArtifactBytes caps a single captured body.
	DefaultMaxArtifactBytes = 32 << 20 // 32 MiB

	// Subprotocol is the channel's negotiated subprotocol.
	Subprotocol = "hub.v1"

	// credentialProtocolPrefix lets a browser pass its credential through the
	// subprotocol header. The WebSocket API in a browser cannot set an
	// Authorization header -- this is not a preference, it is the only
	// header-shaped channel the platform gives a client.
	credentialProtocolPrefix = "hub.credential."
)

// Handler serves the Agent Plane.
type Handler struct {
	registry *registry.Registry
	auth     *auth.Service
	store    core.Store
	blobs    core.BlobStore
	mirrors  *mirror.Manager
	log      *slog.Logger

	pingInterval time.Duration
	readTimeout  time.Duration
	maxArtifact  int64
}

// Options configures the Agent Plane.
type Options struct {
	Registry         *registry.Registry
	Auth             *auth.Service
	Store            core.Store
	Blobs            core.BlobStore
	Mirrors          *mirror.Manager
	Logger           *slog.Logger
	PingInterval     time.Duration
	ReadTimeout      time.Duration
	MaxArtifactBytes int64
}

// New builds the Agent Plane handler.
func New(opts Options) *Handler {
	h := &Handler{
		registry:     opts.Registry,
		auth:         opts.Auth,
		store:        opts.Store,
		blobs:        opts.Blobs,
		mirrors:      opts.Mirrors,
		log:          opts.Logger,
		pingInterval: opts.PingInterval,
		readTimeout:  opts.ReadTimeout,
		maxArtifact:  opts.MaxArtifactBytes,
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.pingInterval <= 0 {
		h.pingInterval = DefaultPingInterval
	}
	if h.readTimeout <= 0 {
		h.readTimeout = DefaultReadTimeout
	}
	if h.maxArtifact <= 0 {
		h.maxArtifact = DefaultMaxArtifactBytes
	}
	return h
}

// Routes registers the Agent Plane on a mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /agent/v1/enroll", h.handleEnroll)
	mux.HandleFunc("GET /agent/v1/channel", h.handleChannel)
	mux.HandleFunc("PUT /agent/v1/artifacts/{digest}", h.handlePutArtifact)
	mux.HandleFunc("HEAD /agent/v1/artifacts/{digest}", h.handleHeadArtifact)
}

// ---------------------------------------------------------------- enrollment

type enrollRequest struct {
	Token        string            `json:"token"`
	Name         string            `json:"name"`
	Labels       map[string]string `json:"labels,omitempty"`
	Browser      string            `json:"browser"`
	BrowserVer   string            `json:"browserVersion"`
	Platform     string            `json:"platform"`
	UserAgent    string            `json:"userAgent"`
	AgentVersion string            `json:"agentVersion"`
	Capabilities core.Capabilities `json:"capabilities"`
}

type enrollResponse struct {
	AgentID         string            `json:"agentId"`
	Credential      string            `json:"credential"`
	ProtocolVersion int               `json:"protocolVersion"`
	PingSeconds     int               `json:"pingIntervalSeconds"`
	MaxArtifact     int64             `json:"maxArtifactBytes"`
	Labels          map[string]string `json:"labels,omitempty"`
}

func (h *Handler) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "unreadable enrollment request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, "an enrollment token is required")
		return
	}

	ctx := r.Context()
	agentID := core.NewID(core.PrefixAgent)

	token, credential, err := h.auth.RedeemEnrollment(ctx, req.Token, agentID)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	// Labels from the token win over labels the agent proposes: the operator
	// minting the token is the one deciding what this device is, and an agent
	// must not be able to relabel itself into somebody else's fleet.
	labels := map[string]string{}
	for k, v := range req.Labels {
		labels[k] = v
	}
	for k, v := range token.Labels {
		labels[k] = v
	}

	now := time.Now().UTC()
	agent := core.Agent{
		ID:           agentID,
		Name:         defaultString(req.Name, "unnamed agent"),
		Labels:       labels,
		Capabilities: normalizeCapabilities(req.Capabilities),
		Status:       core.AgentOffline,
		Browser:      req.Browser,
		BrowserVer:   req.BrowserVer,
		Platform:     req.Platform,
		UserAgent:    req.UserAgent,
		AgentVersion: req.AgentVersion,
		EnrolledAt:   now,
		LastSeenAt:   now,
	}
	if err := h.store.PutAgent(ctx, agent); err != nil {
		writeError(w, http.StatusInternalServerError, "could not record the agent: "+err.Error())
		return
	}

	h.log.Info("agent enrolled", "agent", agentID, "browser", req.Browser, "capture", agent.Capabilities.Capture)

	writeJSON(w, http.StatusCreated, enrollResponse{
		AgentID:         agentID,
		Credential:      credential,
		ProtocolVersion: wire.ProtocolVersion,
		PingSeconds:     int(h.pingInterval / time.Second),
		MaxArtifact:     h.maxArtifact,
		Labels:          labels,
	})
}

// normalizeCapabilities keeps an agent's self-report honest where the claim is
// checkable. It cannot verify that an agent can really click, but it can refuse
// a capture mode that does not exist and an op outside the v1 vocabulary.
func normalizeCapabilities(c core.Capabilities) core.Capabilities {
	switch c.Capture {
	case core.CaptureFetchPatch, core.CaptureFilterResponse, core.CaptureDebugger:
	default:
		c.Capture = core.CaptureNone
	}
	known := make([]string, 0, len(c.Ops))
	for _, op := range c.Ops {
		if core.IsKnownOp(op) {
			known = append(known, op)
		}
	}
	c.Ops = known
	return c
}

// ------------------------------------------------------------------ channel

func (h *Handler) handleChannel(w http.ResponseWriter, r *http.Request) {
	credential := channelCredential(r)
	if credential == "" {
		writeError(w, http.StatusUnauthorized, "no agent credential presented")
		return
	}

	ctx := r.Context()
	agentID, err := h.auth.AuthenticateAgent(ctx, credential)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	agent, err := h.store.GetAgent(ctx, agentID)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	conn, err := wire.Upgrade(w, r, wire.UpgradeOptions{
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		// Upgrade already wrote the response.
		h.log.Info("channel handshake refused", "agent", agentID, "err", err)
		return
	}

	l := newLink(h, agentID, agent.Capabilities, conn)

	// The channel outlives the request that created it, so it does not inherit
	// the request's cancellation. Otherwise every store write made while the
	// channel is closing -- marking the agent offline, closing its sessions --
	// would run on an already-cancelled context and quietly fail.
	linkCtx := context.WithoutCancel(ctx)

	// Attach before serving: a command may arrive the instant the agent is
	// listed as online, and it must find a channel to travel on.
	h.registry.Attach(linkCtx, l)
	defer h.registry.Detach(linkCtx, l)

	h.log.Info("agent channel open", "agent", agentID, "remote", conn.RemoteAddr())
	l.serve(linkCtx)
}

// channelCredential finds the credential, accepting the three shapes a client
// might have available.
//
// The subprotocol form exists because a browser's WebSocket constructor cannot
// set request headers -- only the subprotocol list. The query parameter is
// accepted as a last resort and documented as the worst option, because URLs
// end up in access logs and referrers.
func channelCredential(r *http.Request) string {
	if tok := auth.BearerToken(r.Header.Get("Authorization")); tok != "" {
		return tok
	}
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, proto := range strings.Split(v, ",") {
			proto = strings.TrimSpace(proto)
			if after, ok := strings.CutPrefix(proto, credentialProtocolPrefix); ok {
				return after
			}
		}
	}
	return r.URL.Query().Get("credential")
}

// ---------------------------------------------------------------- artifacts

func (h *Handler) handlePutArtifact(w http.ResponseWriter, r *http.Request) {
	agentID, ok := h.authenticateAgentHTTP(w, r)
	if !ok {
		return
	}

	digest := blob.NormalizeDigest(r.PathValue("digest"))
	if !blob.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "the artifact path must be a lowercase hex sha256")
		return
	}

	// Already held: content addressing means there is nothing to overwrite, and
	// telling the agent so is what lets it skip the transfer next time.
	if h.blobs.Has(r.Context(), digest) {
		w.Header().Set("Location", "/v1/artifacts/"+digest)
		writeJSON(w, http.StatusOK, map[string]any{"digest": digest, "stored": false})
		return
	}

	body := http.MaxBytesReader(w, r.Body, h.maxArtifact+1)
	defer body.Close()

	info, err := h.blobs.Put(r.Context(), digest, body, r.Header.Get("Content-Type"), h.maxArtifact)
	if err != nil {
		switch {
		case errors.Is(err, core.ErrDigestMismatch):
			// The strongest signal available that an agent is broken or hostile.
			h.log.Warn("artifact digest mismatch", "agent", agentID, "digest", digest, "err", err)
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, core.ErrTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "storing the artifact failed: "+err.Error())
		}
		return
	}

	w.Header().Set("Location", "/v1/artifacts/"+digest)
	writeJSON(w, http.StatusCreated, map[string]any{
		"digest": info.Digest,
		"size":   info.Size,
		"stored": true,
	})
}

// handleHeadArtifact is how an agent avoids re-uploading bytes the hub already
// has. Cheap to answer and it removes the largest share of upload traffic on a
// site that serves the same assets on every page.
func (h *Handler) handleHeadArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authenticateAgentHTTP(w, r); !ok {
		return
	}
	digest := blob.NormalizeDigest(r.PathValue("digest"))
	if !blob.ValidDigest(digest) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !h.blobs.Has(r.Context(), digest) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if info, err := h.blobs.Stat(r.Context(), digest); err == nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) authenticateAgentHTTP(w http.ResponseWriter, r *http.Request) (string, bool) {
	credential := auth.BearerToken(r.Header.Get("Authorization"))
	if credential == "" {
		writeError(w, http.StatusUnauthorized, "an agent credential is required")
		return "", false
	}
	agentID, err := h.auth.AuthenticateAgent(r.Context(), credential)
	if err != nil {
		writeDomainError(w, err)
		return "", false
	}
	return agentID, true
}

// ------------------------------------------------------------------ helpers

func defaultString(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeDomainError maps core errors to status codes in one place, so no handler
// has to remember which error means which code.
func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, core.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, core.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, core.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, core.ErrBadRequest), errors.Is(err, core.ErrDigestMismatch):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
