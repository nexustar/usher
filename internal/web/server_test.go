package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestFocusSwitchBanner(t *testing.T) {
	full := "0af0c1d2-3e4f-5678-9abc-def012345678"

	// Same focus → no banner; the persistent UI focus link already shows it.
	if got := focusSwitchBanner(full, full, "auth"); got != "" {
		t.Errorf("same focus should give no banner, got %q", got)
	}
	// Turn that touched no session → no banner.
	if got := focusSwitchBanner(full, "", "auth"); got != "" {
		t.Errorf("untouched turn should give no banner, got %q", got)
	}

	// Switch between sessions → "Switching to" + title + link.
	got := focusSwitchBanner("11111111-aaaa", full, "auth-service")
	want := "↪ Switching to [auth-service](#/s/" + full + ")\n\n"
	if got != want {
		t.Errorf("switch banner:\n got %q\nwant %q", got, want)
	}

	// First focus (none → X) → "Routing to".
	if got := focusSwitchBanner("", full, "auth-service"); got != "↪ Routing to [auth-service](#/s/"+full+")\n\n" {
		t.Errorf("first-focus banner = %q", got)
	}

	// Untitled session → short id as link text.
	if got := focusSwitchBanner("", full, ""); got != "↪ Routing to [0af0c1d2](#/s/"+full+")\n\n" {
		t.Errorf("untitled banner = %q", got)
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
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, err := mw.CreateFormFile("file", "screenshot.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("png data")); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/uploads", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
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

func TestHumanizeAge(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		last time.Time
		want string
	}{
		{time.Time{}, "unknown"},                 // zero → unknown
		{now.Add(-10 * time.Second), "just now"}, // <1m
		{now.Add(-5 * time.Minute), "5m ago"},    // minutes
		{now.Add(-3 * time.Hour), "3h ago"},      // hours
		{now.Add(-50 * time.Hour), "2d ago"},     // days (floored)
		{now.Add(10 * time.Second), "just now"},  // future/skew
	}
	for _, c := range cases {
		if got := humanizeAge(now, c.last); got != c.want {
			t.Errorf("humanizeAge(now, %v) = %q, want %q", c.last, got, c.want)
		}
	}
}
