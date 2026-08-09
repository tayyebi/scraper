// Package console is the Operator Console: plane three.
//
// It is deliberately just another Control API client. This package serves three
// static files and handles login; every piece of data the console displays it
// fetches from /v1/... exactly as an automation would. That constraint is the
// point -- if the console needed a private endpoint, the Control API would be
// incomplete, and nobody would notice.
package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tayyebi/scraper/internal/adapters/controlhttp"
	"github.com/tayyebi/scraper/internal/auth"
	"github.com/tayyebi/scraper/internal/core"
	"github.com/tayyebi/scraper/web"
)

// Handler serves the console.
type Handler struct {
	auth   *auth.Service
	log    *slog.Logger
	assets fs.FS

	// secureCookies marks the session cookie Secure. It is off for plain HTTP
	// because a Secure cookie is silently dropped there, which would present as
	// "login does nothing" -- the worst possible failure for a first run.
	secureCookies bool
}

// Options configures the console.
type Options struct {
	Auth          *auth.Service
	Logger        *slog.Logger
	SecureCookies bool
}

// New builds the console handler.
func New(opts Options) (*Handler, error) {
	assets, err := fs.Sub(web.Console, "console")
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		auth:          opts.Auth,
		log:           log,
		assets:        assets,
		secureCookies: opts.SecureCookies,
	}, nil
}

// Routes registers the console.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusFound)
	})
	mux.HandleFunc("GET /console/{$}", h.index)
	mux.HandleFunc("GET /console/{file}", h.asset)
	mux.HandleFunc("POST /console/login", h.login)
	mux.HandleFunc("POST /console/logout", h.logout)
	mux.HandleFunc("GET /console/session", h.session)
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	h.serveAsset(w, r, "index.html")
}

func (h *Handler) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	// Only the three files exist; anything else is a probe.
	switch name {
	case "index.html", "style.css", "app.js":
		h.serveAsset(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	switch {
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}

	// The console renders captured page titles and URLs. A strict CSP means a
	// hostile title cannot become script even if some future change forgets to
	// escape it -- defence the code does not have to remember. It also forbids
	// inline script, which is why app.js is a separate file rather than a
	// <script> block.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	http.ServeFileFS(w, r, h.assets, name)
}

// ----------------------------------------------------------------- helpers

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("unreadable request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ------------------------------------------------------------------- login

type loginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	session, secret, err := h.auth.Login(r.Context(), req.User, req.Password)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, core.ErrForbidden) {
			status = http.StatusForbidden
		}
		h.log.Warn("console login refused", "user", req.User, "err", err)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     controlhttp.SessionCookie,
		Value:    secret,
		Path:     "/",
		Expires:  session.ExpiresAt,
		HttpOnly: true,
		Secure:   h.secureCookies,
		// Strict rather than Lax: nothing links into the console from
		// elsewhere, and a cookie that never rides a cross-site request cannot
		// be the CSRF half of an attack.
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"user":      session.User,
		"expiresAt": session.ExpiresAt,
	})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(controlhttp.SessionCookie); err == nil && c.Value != "" {
		if err := h.auth.Logout(r.Context(), c.Value); err != nil {
			h.log.Warn("console logout", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     controlhttp.SessionCookie,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// session tells the page who it is, so app.js can decide between the login form
// and the console without guessing from a 401 on some other request.
func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(controlhttp.SessionCookie)
	if err != nil || c.Value == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"loginEnabled":  h.auth.ConsoleEnabled(),
		})
		return
	}
	sess, err := h.auth.AuthenticateConsole(r.Context(), c.Value)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"loginEnabled":  h.auth.ConsoleEnabled(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"loginEnabled":  true,
		"user":          sess.User,
		"expiresAt":     sess.ExpiresAt,
	})
}
