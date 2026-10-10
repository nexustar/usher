package api_test

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/api"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/pluginapi/pluginapitest"
)

// The web server and the control socket mount the same handlers under their
// own prefix: a request must read the same on both.
func TestSamePrefixedRoutesAnswerAlike(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1", Title: "work", Cwd: "/w"})
	f.Archive("s1")
	mux := http.NewServeMux()
	srv := api.New(f, nil, t.TempDir())
	srv.Register(mux, "/api")
	srv.Register(mux, "/v1")

	do := func(method, path, body string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/sessions", ""},
		{http.MethodGet, "/sessions?include_archived=1", ""},
		{http.MethodGet, "/sessions/s1", ""},
		{http.MethodGet, "/sessions/nope", ""},
		{http.MethodGet, "/sessions/s1/transcript", ""},
		{http.MethodPost, "/sessions/s1/send", `{"text":""}`},
		{http.MethodPost, "/sessions/nope/archive", ""},
		{http.MethodGet, "/interactions", ""},
	} {
		webCode, webBody := do(c.method, "/api"+c.path, c.body)
		sockCode, sockBody := do(c.method, "/v1"+c.path, c.body)
		if webCode != sockCode || webBody != sockBody {
			t.Errorf("%s %s: web %d %q, socket %d %q", c.method, c.path, webCode, webBody, sockCode, sockBody)
		}
	}

	// Archived sessions are listed only on request.
	if _, body := do(http.MethodGet, "/v1/sessions", ""); strings.Contains(body, `"s1"`) {
		t.Errorf("archived session listed by default: %s", body)
	}
	if code, _ := do(http.MethodGet, "/v1/sessions/nope", ""); code != http.StatusNotFound {
		t.Errorf("missing session: status %d", code)
	}
}

func TestUploadTakesTheRawBody(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	mux := http.NewServeMux()
	api.New(f, nil, t.TempDir()).Register(mux, "/api")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/s1/upload?filename=a.txt", strings.NewReader("hello")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "a.txt") {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/s1/upload", strings.NewReader("hello")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing filename: status %d", rec.Code)
	}
}

// Both streams frame an event the same way: its type as the SSE event name and
// the whole event as data. The global stream opens with what is already true.
func TestEventStreamsShareOneFrame(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1", Status: core.StatusRunning})
	f.SetPending(interaction.Pending{ID: "p1", SessionID: "s1"})
	mux := http.NewServeMux()
	api.New(f, nil, t.TempDir()).Register(mux, "/api")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	frames := func(path string, n int) []string {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out []string
		r := bufio.NewReader(resp.Body)
		for len(out) < n {
			line, err := r.ReadString('\n')
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
		return out
	}

	got := frames("/api/sessions/s1/events", 2)
	want := []string{"event: turn.active", `data: {"session_id":"s1","type":"turn.active"}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("session stream opened with %q, want %q", got, want)
	}
	got = frames("/api/events", 4)
	if len(got) != 4 || got[0] != want[0] || got[1] != want[1] ||
		got[2] != "event: interaction.pending" || !strings.Contains(got[3], `"session_id":"s1","type":"interaction.pending","raw":{"id":"p1"`) {
		t.Errorf("global stream opened with %q", got)
	}
}
