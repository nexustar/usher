package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestReadOnlyMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := readOnlyMiddleware(next)

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/sessions", http.StatusOK},
		{http.MethodHead, "/", http.StatusOK},
		{http.MethodGet, "/api/sessions/abc/events", http.StatusOK},
		{http.MethodPost, "/api/sessions/abc/send", http.StatusForbidden},
		{http.MethodDelete, "/api/sessions/abc", http.StatusForbidden},
		{http.MethodPut, "/api/agents/x", http.StatusForbidden},
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
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
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

// Each draft upload gets its own directory, so the same name uploaded twice
// keeps it both times.
func TestDraftUploadKeepsFilenamePerUpload(t *testing.T) {
	s := &Server{attachmentsDir: t.TempDir()}
	upload := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/uploads?filename=screenshot.png", strings.NewReader("png data"))
		rec := httptest.NewRecorder()
		s.handleDraftUpload(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
		}
		var got struct{ Path string }
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.Path
	}

	first, second := upload(), upload()
	if first == second {
		t.Fatalf("both uploads landed on %q", first)
	}
	for _, p := range []string{first, second} {
		if filepath.Base(p) != "screenshot.png" {
			t.Errorf("path = %q, want it to keep the name screenshot.png", p)
		}
		if got, want := filepath.Dir(filepath.Dir(p)), filepath.Join(s.attachmentsDir, draftsDir); got != want {
			t.Errorf("%q sits in %q, want a subdirectory of %q", p, got, want)
		}
		if data, err := os.ReadFile(p); err != nil || string(data) != "png data" {
			t.Errorf("content of %q = %q (err %v)", p, data, err)
		}
	}
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
