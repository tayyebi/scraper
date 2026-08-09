package controlhttp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/internal/mirror"
	"github.com/tayyebi/scraper/internal/store/blob"
)

// ----------------------------------------------------------------- sessions

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	f := core.SessionFilter{
		AgentID: r.URL.Query().Get("agent"),
		State:   core.SessionState(r.URL.Query().Get("state")),
		Origin:  core.SessionOrigin(r.URL.Query().Get("origin")),
		Limit:   intParam(r, "limit", 200),
	}
	sessions, err := h.fleet.ListSessions(r.Context(), f)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request) {
	sess, err := h.fleet.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (h *Handler) closeSession(w http.ResponseWriter, r *http.Request) {
	if err := h.fleet.CloseSession(r.Context(), r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ----------------------------------------------------------------- commands

type commandRequest struct {
	Op     string          `json:"op"`
	Params json.RawMessage `json:"params,omitempty"`
}

// postCommand dispatches a command.
//
// ?wait=30s blocks for a result and answers 200; without it the command is
// accepted and answered 202 with an id to poll. Both write the same row, so a
// caller whose wait expired can still fetch the result rather than losing it.
func (h *Handler) postCommand(w http.ResponseWriter, r *http.Request) {
	var req commandRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Op == "" {
		writeError(w, http.StatusBadRequest, "a command needs an op; see GET /v1/meta for the vocabulary")
		return
	}

	wait, err := waitParam(r, h.maxWait)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cmd, err := h.fleet.Dispatch(r.Context(), r.PathValue("id"), req.Op, req.Params, wait)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	w.Header().Set("Location", "/v1/commands/"+cmd.ID)
	if wait <= 0 {
		writeJSON(w, http.StatusAccepted, cmd)
		return
	}
	if cmd.State == core.CommandTimeout {
		// The command is still live hub-side; the caller simply stopped
		// waiting. 504 with the id says exactly that.
		writeJSON(w, http.StatusGatewayTimeout, cmd)
		return
	}
	writeJSON(w, http.StatusOK, cmd)
}

func waitParam(r *http.Request, max time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("wait"))
	if raw == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return clampWait(d, max), nil
	}
	// A bare number reads as seconds, because "wait=30" is what everyone types
	// first and rejecting it teaches nothing.
	if d, err := time.ParseDuration(raw + "s"); err == nil {
		return clampWait(d, max), nil
	}
	return 0, fmt.Errorf("wait=%q is not a duration; try wait=30s", raw)
}

func clampWait(d, max time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > max {
		return max
	}
	return d
}

func (h *Handler) getCommand(w http.ResponseWriter, r *http.Request) {
	cmd, err := h.fleet.GetCommand(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cmd)
}

// ---------------------------------------------------------------------- DOM

func (h *Handler) getDOM(w http.ResponseWriter, r *http.Request) {
	doc, err := h.fleet.DOM(r.Context(), r.PathValue("id"), boolParam(r, "fresh"))
	if err != nil {
		writeDomainError(w, err)
		return
	}

	switch strings.ToLower(r.URL.Query().Get("format")) {
	case "html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The mirror's sequence number travels in a header so a caller can tell
		// two reads apart without parsing the document.
		w.Header().Set("X-Hub-Mirror-Seq", fmt.Sprint(doc.Seq))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, mirror.RenderHTML(doc))
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Hub-Mirror-Seq", fmt.Sprint(doc.Seq))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, mirror.Text(doc))
	default:
		writeJSON(w, http.StatusOK, doc)
	}
}

// -------------------------------------------------------------------- events

// streamEvents serves the live feed as SSE or NDJSON, chosen by Accept.
//
// Two formats because they have different consumers: EventSource in a browser
// speaks SSE and nothing else, while a scraper piping to jq wants newline JSON
// and finds SSE's framing to be noise.
func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "this server cannot stream")
		return
	}

	ch, cancel, err := h.fleet.Subscribe(r.Context(), sessionID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	defer cancel()

	useSSE := !strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/x-ndjson")

	if useSSE {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which turns a live stream
	// into one long silence followed by a burst. There is no standard header
	// for this; X-Accel-Buffering is the one nginx honours, and it is harmless
	// everywhere else.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Replay anything the caller missed, so reconnecting does not lose events.
	// The cursor is a row id, because two events can share a millisecond.
	if after := lastEventID(r); after > 0 && h.store != nil {
		missed, err := h.store.ListEvents(r.Context(), sessionID, after, 500)
		if err == nil {
			for _, e := range missed {
				if !writeEvent(w, e, useSSE) {
					return
				}
			}
			flusher.Flush()
		}
	}

	// A heartbeat keeps intermediaries from reaping a connection that is simply
	// on a quiet page.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case e, ok := <-ch:
			if !ok {
				return
			}
			if !writeEvent(w, e, useSSE) {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			if useSSE {
				if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
					return
				}
			} else if _, err := io.WriteString(w, "\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e core.Event, sse bool) bool {
	body, err := json.Marshal(e)
	if err != nil {
		return true
	}
	if !sse {
		_, err = w.Write(append(body, '\n'))
		return err == nil
	}
	// id: lets a reconnecting EventSource resume via Last-Event-ID.
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, body)
	return err == nil
}

func lastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw == "" {
		return 0
	}
	var n int64
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0
	}
	return n
}

// -------------------------------------------------------------- request log

func (h *Handler) listRequests(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")

	exchanges, err := h.fleet.Exchanges(r.Context(), sessionID, intParam(r, "limit", 500))
	if err != nil {
		writeDomainError(w, err)
		return
	}

	// The capture gaps travel with the log. A caller reading a fetch-patch
	// agent's log is looking at something that genuinely omits navigations and
	// subresources, and saying so here is the difference between an incomplete
	// answer and a misleading one.
	var gaps []string
	if sess, err := h.fleet.GetSession(r.Context(), sessionID); err == nil {
		if agent, err := h.fleet.GetAgent(r.Context(), sess.AgentID); err == nil {
			gaps = agent.Capabilities.CaptureGaps()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"exchanges":   exchanges,
		"captureGaps": gaps,
	})
}

// ---------------------------------------------------------------- artifacts

func (h *Handler) getArtifact(w http.ResponseWriter, r *http.Request) {
	digest := blob.NormalizeDigest(r.PathValue("digest"))
	if !blob.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "an artifact is addressed by its sha256 hex digest")
		return
	}

	rc, info, err := h.blobs.Open(r.Context(), digest)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	defer rc.Close()

	if info.MimeType != "" {
		w.Header().Set("Content-Type", info.MimeType)
	}
	// Content addressing makes this trivially true: these bytes can never
	// change, because different bytes would have a different name.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+digest+`"`)
	// Captured bytes are somebody else's HTML and JavaScript. Serving them
	// inline from the hub's origin would let a captured page script the
	// console, so they are always a download and never executed.
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")

	http.ServeContent(w, r, digest, info.CreatedAt, rc)
}
