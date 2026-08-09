package console

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tayyebi/scraper/internal/adapters/controlhttp"
	"github.com/tayyebi/scraper/internal/auth"
	"github.com/tayyebi/scraper/internal/store/sqlite"
)

const (
	testUser     = "operator"
	testPassword = "correct horse battery staple"
)

func newConsole(t *testing.T, withLogin bool) *httptest.Server {
	t.Helper()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	opts := auth.Options{Store: st}
	if withLogin {
		hash, err := auth.HashPassword(testPassword)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		opts.ConsoleUser = testUser
		opts.ConsolePasswordHash = hash
	}

	h, err := New(Options{
		Auth:   auth.New(opts),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAssetsAreServed(t *testing.T) {
	srv := newConsole(t, true)

	cases := []struct {
		path     string
		wantType string
		wantText string
	}{
		{"/console/", "text/html", "<title>Browser fleet controller</title>"},
		{"/console/index.html", "text/html", "data-view=\"console\""},
		{"/console/style.css", "text/css", "[data-status=\"online\"]"},
		{"/console/app.js", "text/javascript", "/v1/agents"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.wantType) {
				t.Errorf("Content-Type = %q, want %q", ct, c.wantType)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), c.wantText) {
				t.Errorf("%s does not contain %q", c.path, c.wantText)
			}
		})
	}
}

// The console renders page titles captured from arbitrary sites. A strict CSP
// is the backstop for the escaping the code is already careful about.
func TestAssetsCarryAStrictCSP(t *testing.T) {
	srv := newConsole(t, true)
	resp, err := http.Get(srv.URL + "/console/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Error("the CSP allows inline script")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
}

func TestUnknownAssetIsNotFound(t *testing.T) {
	srv := newConsole(t, true)
	for _, path := range []string{"/console/secrets.txt", "/console/../go.mod", "/console/app.js.map"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s was served", path)
		}
	}
}

func TestRootRedirectsToConsole(t *testing.T) {
	srv := newConsole(t, true)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/console/" {
		t.Errorf("Location = %q", loc)
	}
}

func login(t *testing.T, srv *httptest.Server, user, password string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"user": user, "password": password})
	resp, err := http.Post(srv.URL+"/console/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return resp
}

func TestLoginSetsAHardenedCookie(t *testing.T) {
	srv := newConsole(t, true)
	resp := login(t, srv, testUser, testPassword)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, msg)
	}

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == controlhttp.SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is readable from JavaScript")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict, so it can ride a cross-site request")
	}
	if cookie.Value == testPassword {
		t.Fatal("the cookie contains the password")
	}
}

func TestWrongCredentialsAreRefused(t *testing.T) {
	srv := newConsole(t, true)

	for _, c := range []struct{ user, password string }{
		{testUser, "wrong"},
		{"nobody", testPassword},
		{"", ""},
	} {
		resp := login(t, srv, c.user, c.password)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("user=%q status = %d, want 401", c.user, resp.StatusCode)
		}
		for _, cookie := range resp.Cookies() {
			if cookie.Name == controlhttp.SessionCookie && cookie.Value != "" {
				t.Error("a failed login set a session cookie")
			}
		}
	}
}

// A hub with no console password configured must say so, rather than showing a
// login form that can never succeed.
func TestLoginDisabledIsReported(t *testing.T) {
	srv := newConsole(t, false)

	resp, err := http.Get(srv.URL + "/console/session")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Authenticated bool `json:"authenticated"`
		LoginEnabled  bool `json:"loginEnabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Authenticated {
		t.Error("authenticated without a session")
	}
	if out.LoginEnabled {
		t.Error("login reported as enabled on a hub with no console password")
	}

	refused := login(t, srv, testUser, testPassword)
	_ = refused.Body.Close()
	if refused.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when no console login is configured", refused.StatusCode)
	}
}

func TestSessionLifecycle(t *testing.T) {
	srv := newConsole(t, true)
	jar := newJar(t)
	client := &http.Client{Jar: jar}

	// Before login.
	if authed := sessionState(t, client, srv); authed {
		t.Fatal("authenticated before logging in")
	}

	body, _ := json.Marshal(map[string]string{"user": testUser, "password": testPassword})
	resp, err := client.Post(srv.URL+"/console/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = resp.Body.Close()

	if authed := sessionState(t, client, srv); !authed {
		t.Fatal("not authenticated after a successful login")
	}

	out, err := client.Post(srv.URL+"/console/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	_ = out.Body.Close()
	if out.StatusCode != http.StatusNoContent {
		t.Errorf("logout status = %d, want 204", out.StatusCode)
	}

	if authed := sessionState(t, client, srv); authed {
		t.Error("still authenticated after logging out: the session was not revoked server-side")
	}
}

func sessionState(t *testing.T, client *http.Client, srv *httptest.Server) bool {
	t.Helper()
	resp, err := client.Get(srv.URL + "/console/session")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Authenticated
}

// The console must not invent endpoints of its own: everything it displays has
// to come from the Control API, or the API is incomplete and nobody notices.
func TestConsoleOnlyCallsTheControlAPI(t *testing.T) {
	srv := newConsole(t, true)
	resp, err := http.Get(srv.URL + "/console/app.js")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	source, _ := io.ReadAll(resp.Body)

	// Every path literal the console fetches must be /v1/... or the three
	// console-only endpoints for signing in.
	allowed := []string{"/console/login", "/console/logout", "/console/session"}
	for _, line := range strings.Split(string(source), "\n") {
		for _, marker := range []string{"fetch(", "EventSource("} {
			idx := strings.Index(line, marker)
			if idx < 0 {
				continue
			}
			rest := line[idx+len(marker):]
			if !strings.Contains(rest, "/v1/") && !containsAny(rest, allowed) && !strings.Contains(rest, "path") {
				t.Errorf("console reaches outside the Control API: %s", strings.TrimSpace(line))
			}
		}
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func newJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return jar
}
