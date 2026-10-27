package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexustar/usher/pkg/api"
	"github.com/nexustar/usher/pkg/auth"
)

func TestHandleTheme(t *testing.T) {
	t.Run("configured file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "custom.css")
		if err := os.WriteFile(path, []byte(":root { --accent: red; }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := &Server{themePath: path, logger: slog.Default()}
		rec := httptest.NewRecorder()
		s.handleTheme(rec, httptest.NewRequest(http.MethodGet, "/theme.css", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q", got)
		}
		if got := rec.Body.String(); got != ":root { --accent: red; }\n" {
			t.Errorf("body = %q", got)
		}
	})

	t.Run("missing file is empty CSS", func(t *testing.T) {
		rec := httptest.NewRecorder()
		(&Server{}).handleTheme(rec, httptest.NewRequest(http.MethodGet, "/theme.css", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("body = %q, want empty", rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
	})
}

func TestAppName(t *testing.T) {
	root, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	const name = `Box <A> & "B"`
	const escaped = `Box &lt;A&gt; &amp; &#34;B&#34;`
	s := &Server{appName: name, logger: slog.Default()}

	t.Run("index.html", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.serveTemplated(root, "index.html", "text/html; charset=utf-8")(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		body := rec.Body.String()
		for _, want := range []string{
			"<title>" + escaped + "</title>",
			`<meta name="application-name" content="` + escaped + `">`,
			`<a href="#/">` + escaped + `</a>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body lacks %q", want)
			}
		}
		if strings.Contains(body, "{{") {
			t.Error("body still has a placeholder")
		}
	})

	t.Run("manifest", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.serveTemplated(root, "manifest.webmanifest", "application/manifest+json")(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/manifest+json" {
			t.Errorf("Content-Type = %q", got)
		}
		var m struct {
			Name      string `json:"name"`
			ShortName string `json:"short_name"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("manifest is not JSON: %v\n%s", err, rec.Body.String())
		}
		if m.Name != name || m.ShortName != name {
			t.Errorf("name = %q, short_name = %q, want %q", m.Name, m.ShortName, name)
		}
	})

	t.Run("login", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.renderLogin(rec, httptest.NewRequest(http.MethodGet, "/login", nil), "", http.StatusOK, "")
		if body := rec.Body.String(); !strings.Contains(body, "<h1>"+escaped+"</h1>") {
			t.Errorf("login page lacks the name:\n%s", body)
		}
	})

	t.Run("default", func(t *testing.T) {
		if got := NewServer("", "", nil, ProxyAuth{}, nil, nil, "", "", "", "", api.Config{}, nil).appName; got != "usher" {
			t.Errorf("appName = %q, want usher", got)
		}
	})
}

func TestReadOnlyMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := readOnlyMiddleware(next)

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/v1/sessions", http.StatusOK},
		{http.MethodHead, "/", http.StatusOK},
		{http.MethodGet, "/api/v1/sessions/abc/events", http.StatusOK},
		{http.MethodPost, "/api/v1/sessions/abc/messages", http.StatusForbidden},
		{http.MethodPatch, "/api/v1/sessions/abc", http.StatusForbidden},
		{http.MethodDelete, "/api/v1/sessions/abc", http.StatusForbidden},
		{http.MethodPut, "/api/v1/agents/x", http.StatusForbidden},
		{http.MethodPost, "/internal/router", http.StatusForbidden},
		{http.MethodPost, "/login", http.StatusOK},
		{http.MethodPost, "/logout", http.StatusOK},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
		if c.want == http.StatusForbidden {
			var body api.Error
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "forbidden" {
				t.Errorf("%s %s: body %q lacks error field", c.method, c.path, rec.Body.String())
			}
		}
	}
}

func TestTerminalControlsAreAllowListed(t *testing.T) {
	want := map[string]string{
		"up": "Up", "down": "Down", "left": "Left", "right": "Right",
		"enter": "Enter", "escape": "Escape", "tab": "Tab",
		"ctrl-c": "C-c", "ctrl-z": "C-z", "ctrl-d": "C-d", "ctrl-x": "C-x",
		"ctrl-o": "C-o", "ctrl-w": "C-w", "ctrl-k": "C-k", "ctrl-u": "C-u",
	}
	if len(terminalControls) != len(want) {
		t.Fatalf("terminalControls has %d entries, want %d", len(terminalControls), len(want))
	}
	for name, key := range want {
		if terminalControls[name] != key {
			t.Errorf("terminalControls[%q] = %q, want %q", name, terminalControls[name], key)
		}
	}
}

func TestLegacyPermissionRequestContinuesWithoutPending(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hook/PermissionRequest", nil)
	req.SetPathValue("event", "PermissionRequest")
	(&Server{}).handleHook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got, want := rec.Body.String(), "{\"continue\":true}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestTerminalInputValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty text", `{"request_id":"r1"}`},
		{"missing request id", `{"text":"pwd"}`},
		{"nul", `{"text":"a\u0000b","request_id":"r1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/terminal/input", bytes.NewBufferString(tt.body))
			(&Server{}).handleTerminalInput(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGzipMiddleware(t *testing.T) {
	json200 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "26")
		_, _ = w.Write([]byte(`{"hello":"compressed-yes"}`))
	})
	sse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapper must still satisfy http.Flusher")
		}
		_, _ = w.Write([]byte("event: x\ndata: {}\n\n"))
	})
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	})

	t.Run("compresses json 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		gzipMiddleware(json200).ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		if rec.Header().Get("Content-Length") != "" {
			t.Error("identity Content-Length must be dropped on a gzipped body")
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("body is not gzip: %v", err)
		}
		body, _ := io.ReadAll(zr)
		if string(body) != `{"hello":"compressed-yes"}` {
			t.Errorf("decompressed body = %q", body)
		}
	})

	t.Run("skips without accept-encoding", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gzipMiddleware(json200).ServeHTTP(rec, httptest.NewRequest("GET", "/api/x", nil))
		if rec.Header().Get("Content-Encoding") != "" {
			t.Error("must not compress when the client didn't ask")
		}
		if rec.Body.String() != `{"hello":"compressed-yes"}` {
			t.Errorf("body = %q", rec.Body.String())
		}
	})

	t.Run("compresses a created resource", func(t *testing.T) {
		created := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"s1"}`))
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/sessions", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		gzipMiddleware(created).ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated || rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("status %d, Content-Encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("body is not gzip: %v", err)
		}
		if body, _ := io.ReadAll(zr); string(body) != `{"id":"s1"}` {
			t.Errorf("decompressed body = %q", body)
		}
	})

	t.Run("leaves SSE untouched", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/s/events", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		gzipMiddleware(sse).ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Error("event-stream must never be compressed")
		}
		if rec.Body.String() != "event: x\ndata: {}\n\n" {
			t.Errorf("body = %q", rec.Body.String())
		}
	})

	t.Run("leaves non-200 untouched", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		gzipMiddleware(notFound).ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Error("non-200 must not be compressed")
		}
		if rec.Body.String() != `{"error":"nope"}` {
			t.Errorf("body = %q", rec.Body.String())
		}
	})
}

func TestCrossOriginGuard(t *testing.T) {
	h := crossOriginGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	cases := []struct {
		name, method, site, origin string
		want                       int
	}{
		{"same origin", http.MethodPost, "same-origin", "", http.StatusOK},
		{"other port", http.MethodPost, "same-site", "", http.StatusForbidden},
		{"other site", http.MethodPost, "cross-site", "", http.StatusForbidden},
		{"old browser, other origin", http.MethodPost, "", "http://evil.example", http.StatusForbidden},
		{"curl", http.MethodPost, "", "", http.StatusOK},
		{"cross-site read", http.MethodGet, "cross-site", "", http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://127.0.0.1:7777/api/v1/sessions", strings.NewReader(`{}`))
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestHostGuard(t *testing.T) {
	store, err := auth.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{auth: store}).hostGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	status := func(host string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	allowed := []string{"127.0.0.1:7777", "localhost:7777", "LOCALHOST", "app.localhost:7777", "[::1]:7777", "::1", "192.168.1.5:7777", "100.64.0.1"}
	refused := []string{"evil.example:7777", "evil.example", "127.0.0.1.nip.io:7777", "localhost.evil.example"}
	for _, host := range allowed {
		if got := status(host); got != http.StatusOK {
			t.Errorf("no password, %s = %d, want 200", host, got)
		}
	}
	for _, host := range refused {
		if got := status(host); got != http.StatusForbidden {
			t.Errorf("no password, %s = %d, want 403", host, got)
		}
	}
	if err := store.SetPassword("pw"); err != nil {
		t.Fatal(err)
	}
	if got := status("usher.example:443"); got != http.StatusOK {
		t.Errorf("with password, named host = %d, want 200", got)
	}
}

func TestAuthMiddlewareAnswersAPICallsWithJSON(t *testing.T) {
	store, err := auth.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPassword("pw"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{auth: store}).authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, path := range []string{"/api/v1/sessions", "/internal/config"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body api.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusUnauthorized || err != nil || body.Error.Code != "unauthorized" {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("page load = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestProxyAuth(t *testing.T) {
	store, err := auth.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPassword("pw"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := &Server{
		auth:   store,
		proxy:  ProxyAuth{Header: "X-Forwarded-Email"},
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.authMiddleware(ok)
	status := func(method, path string, hdr map[string]string) int {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	user := map[string]string{"X-Forwarded-Email": "alice"}
	cases := []struct {
		name, method, path string
		hdr                map[string]string
		want               int
	}{
		{"signed in", http.MethodGet, "/api/v1/sessions", user, http.StatusOK},
		{"state change", http.MethodPost, "/api/v1/sessions", user, http.StatusOK},
		{"no header", http.MethodGet, "/api/v1/sessions", nil, http.StatusUnauthorized},
		{"page without header", http.MethodGet, "/settings", nil, http.StatusUnauthorized},
		{"blank header", http.MethodGet, "/api/v1/sessions", map[string]string{"X-Forwarded-Email": " "}, http.StatusUnauthorized},
		{"healthz exempt", http.MethodGet, "/healthz", nil, http.StatusOK},
	}
	for _, c := range cases {
		if got := status(c.method, c.path, c.hdr); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	if !strings.Contains(logs.String(), "actor=alice") || !strings.Contains(logs.String(), "method=POST") {
		t.Errorf("state change not logged with actor: %q", logs.String())
	}
	if strings.Contains(logs.String(), "method=GET") {
		t.Errorf("read logged: %q", logs.String())
	}

	// The password store is not consulted: a login cookie alone is refused.
	cookie, err := store.IssueCookie()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie without header = %d, want 401", rec.Code)
	}

	// /login sends the browser back through the proxy to the page it wanted.
	rec = httptest.NewRecorder()
	s.handleLogin(rec, httptest.NewRequest(http.MethodGet, "/login?next=/sessions/abc", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/sessions/abc" {
		t.Errorf("login = %d %q, want 303 /sessions/abc", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	s.handleLogout(rec, httptest.NewRequest(http.MethodGet, "/logout", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("logout without url = %d %q, want 303 /", rec.Code, rec.Header().Get("Location"))
	}
	s.proxy.LogoutURL = "https://sso.example/logout"
	rec = httptest.NewRecorder()
	s.handleLogout(rec, httptest.NewRequest(http.MethodGet, "/logout", nil))
	if rec.Header().Get("Location") != s.proxy.LogoutURL {
		t.Errorf("logout = %q, want %q", rec.Header().Get("Location"), s.proxy.LogoutURL)
	}

	// Named hosts are served.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.Host = "usher.example"
	rec = httptest.NewRecorder()
	s.hostGuard(ok).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("named host = %d, want 200", rec.Code)
	}
}

func TestLogoutURL(t *testing.T) {
	store, err := auth.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := (&Server{auth: store}).logoutURL(); got != "" {
		t.Errorf("no auth = %q, want hidden", got)
	}
	if err := store.SetPassword("pw"); err != nil {
		t.Fatal(err)
	}
	if got := (&Server{auth: store}).logoutURL(); got != "/logout" {
		t.Errorf("password = %q, want /logout", got)
	}
	gw := ProxyAuth{Header: "X-Forwarded-Email"}
	if got := (&Server{auth: store, proxy: gw}).logoutURL(); got != "" {
		t.Errorf("proxy without logout url = %q, want hidden", got)
	}
	gw.LogoutURL = "https://sso.example/logout"
	if got := (&Server{proxy: gw}).logoutURL(); got != gw.LogoutURL {
		t.Errorf("proxy = %q, want %q", got, gw.LogoutURL)
	}
}
