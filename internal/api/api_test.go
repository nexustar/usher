package api_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/api"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/pluginapi/pluginapitest"
)

func TestSessionRoutes(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1", Title: "work", Cwd: "/w"})
	f.Archive("s1")
	do := serve(t, api.Config{Router: f})

	// Archived sessions are listed only on request.
	if _, body := do(http.MethodGet, "/sessions", ""); body != `{"sessions":[]}`+"\n" {
		t.Errorf("default list = %s", body)
	}
	if _, body := do(http.MethodGet, "/sessions?include_archived=1", ""); !strings.Contains(body, `"id":"s1"`) {
		t.Errorf("list with archived = %s", body)
	}

	code, body := do(http.MethodPatch, "/sessions/s1", `{"archived":false,"auto_approve":true}`)
	if code != http.StatusOK || !strings.Contains(body, `"archived":false`) || !strings.Contains(body, `"auto_approve":true`) {
		t.Errorf("patch = %d %s", code, body)
	}
	if f.IsArchived("s1") || !f.IsAutoApprove("s1") {
		t.Errorf("patch left archived=%v auto_approve=%v", f.IsArchived("s1"), f.IsAutoApprove("s1"))
	}

	if code, body := do(http.MethodPost, "/sessions/s1/messages", `{"text":"hi"}`); code != http.StatusAccepted {
		t.Errorf("message = %d %s", code, body)
	}
	if got := f.Sent("s1"); len(got) != 1 || got[0] != "hi" {
		t.Errorf("sent = %v", got)
	}

	for _, c := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{http.MethodGet, "/sessions/nope", "", http.StatusNotFound, "not_found"},
		{http.MethodPatch, "/sessions/nope", `{"pinned":true}`, http.StatusNotFound, "not_found"},
		{http.MethodPost, "/sessions/s1/messages", `{"text":""}`, http.StatusBadRequest, "invalid_request"},
	} {
		code, body := do(c.method, c.path, c.body)
		var e api.Error
		if err := json.Unmarshal([]byte(body), &e); err != nil || code != c.status || e.Error.Code != c.code || e.Error.Message == "" {
			t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, code, body, c.status, c.code)
		}
	}
}

func TestUploadTakesTheRawBody(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	do := serve(t, api.Config{Router: f, AttachmentsDir: t.TempDir()})

	if code, body := do(http.MethodPost, "/uploads?session_id=s1&filename=a.txt", "hello"); code != http.StatusCreated || !strings.Contains(body, "/s1/a.txt") {
		t.Fatalf("session upload = %d %s", code, body)
	}
	if code, body := do(http.MethodPost, "/uploads?filename=a.txt", "hello"); code != http.StatusCreated || !strings.Contains(body, "/_drafts/") {
		t.Fatalf("draft upload = %d %s", code, body)
	}
	if code, _ := do(http.MethodPost, "/uploads?session_id=s1", "hello"); code != http.StatusBadRequest {
		t.Fatalf("missing filename: status %d", code)
	}
	if code, _ := do(http.MethodPost, "/uploads?session_id=nope&filename=a.txt", "hello"); code != http.StatusNotFound {
		t.Fatalf("unknown session: status %d", code)
	}
}

// Both streams frame an event the same way: its type as the SSE event name and
// the whole event as data. The global stream opens with what is already true.
func TestEventStreamsShareOneFrame(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1", Status: core.StatusRunning})
	f.SetPending(interaction.Pending{ID: "p1", SessionID: "s1"})
	mux := http.NewServeMux()
	api.New(api.Config{Router: f}).Register(mux)
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

	got := frames("/api/v1/sessions/s1/events", 2)
	want := []string{"event: turn.active", `data: {"session_id":"s1","type":"turn.active"}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("session stream opened with %q, want %q", got, want)
	}
	got = frames("/api/v1/events", 4)
	if len(got) != 4 || got[0] != want[0] || got[1] != want[1] ||
		got[2] != "event: interaction.pending" || !strings.Contains(got[3], `"session_id":"s1","type":"interaction.pending","data":{"id":"p1"`) {
		t.Errorf("global stream opened with %q", got)
	}
}

// Each draft upload gets its own directory, so the same name uploaded twice
// keeps it both times.
func TestDraftUploadKeepsFilenamePerUpload(t *testing.T) {
	dir := t.TempDir()
	do := serve(t, api.Config{Router: pluginapitest.NewFakeRouter(), AttachmentsDir: dir})
	upload := func() string {
		t.Helper()
		code, body := do(http.MethodPost, "/uploads?filename=screenshot.png", "png data")
		if code != http.StatusCreated {
			t.Fatalf("status = %d, body = %q", code, body)
		}
		var got api.Upload
		if err := json.Unmarshal([]byte(body), &got); err != nil {
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
		if got, want := filepath.Dir(filepath.Dir(p)), filepath.Join(dir, "_drafts"); got != want {
			t.Errorf("%q sits in %q, want a subdirectory of %q", p, got, want)
		}
		if data, err := os.ReadFile(p); err != nil || string(data) != "png data" {
			t.Errorf("content of %q = %q (err %v)", p, data, err)
		}
	}
}

func TestCreateAndControlRoutes(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1", Cwd: "/w"})
	mux := http.NewServeMux()
	api.New(api.Config{Router: f}).Register(mux)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, api.Prefix+path, strings.NewReader(body)))
		return rec
	}

	for _, c := range []struct{ path, body, id string }{
		{"/sessions", `{"cwd":"/w","initial_message":"hi"}`, "s_new"},
		{"/sessions/s1/fork", `{"after_uuid":"u1"}`, "s1_fork"},
	} {
		rec := do(http.MethodPost, c.path, c.body)
		var sess api.Session
		_ = json.Unmarshal(rec.Body.Bytes(), &sess)
		if rec.Code != http.StatusCreated || sess.ID != c.id || rec.Header().Get("Location") != api.Prefix+"/sessions/"+c.id {
			t.Errorf("POST %s = %d, Location %q, body %s", c.path, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
	}

	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/sessions/s1/interrupt", http.StatusNoContent},
		{http.MethodPost, "/sessions/nope/interrupt", http.StatusNotFound},
		{http.MethodPost, "/sessions/s1/pause", http.StatusNoContent},
		{http.MethodPost, "/sessions/nope/pause", http.StatusNotFound},
		{http.MethodPost, "/sessions/s1/resume", http.StatusNoContent},
		{http.MethodDelete, "/sessions/s1", http.StatusNoContent},
	} {
		if rec := do(c.method, c.path, ""); rec.Code != c.want || (c.want == http.StatusNoContent && rec.Body.Len() != 0) {
			t.Errorf("%s %s = %d %q, want %d", c.method, c.path, rec.Code, rec.Body, c.want)
		}
	}
}

func TestPatchTitle(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	do := serve(t, api.Config{Router: f})
	if code, body := do(http.MethodPatch, "/sessions/s1", `{"title":"  "}`); code != http.StatusBadRequest || !strings.Contains(body, `"invalid_request"`) {
		t.Errorf("blank title = %d %s", code, body)
	}
	if code, body := do(http.MethodPatch, "/sessions/s1", `{"title":"new"}`); code != http.StatusOK {
		t.Errorf("title = %d %s", code, body)
	}
}

func TestRespondInteraction(t *testing.T) {
	do := serve(t, api.Config{Router: pluginapitest.NewFakeRouter()})
	if code, body := do(http.MethodPost, "/interactions/p1/respond", `{"behavior":"allow","scope":"always"}`); code != http.StatusNoContent {
		t.Fatalf("respond = %d %s", code, body)
	}
	if code, body := do(http.MethodPost, "/interactions/p1/respond", `{"behavior":"allow"}`); code != http.StatusNotFound || !strings.Contains(body, `"not_found"`) {
		t.Errorf("respond again = %d %s", code, body)
	}
	if code, body := do(http.MethodPost, "/interactions/p2/respond", `{"behavior":"allow","scope":"session"}`); code != http.StatusBadRequest {
		t.Errorf("old scope = %d %s", code, body)
	}
}

func TestBackendsAndInfo(t *testing.T) {
	do := serve(t, api.Config{Router: pluginapitest.NewFakeRouter(), Version: "v0.10.0", ReadOnly: true})
	if _, body := do(http.MethodGet, "/backends", ""); body != `{"backends":[{"name":"claude","models":[]}]}`+"\n" {
		t.Errorf("backends = %s", body)
	}
	if _, body := do(http.MethodGet, "/info", ""); body != `{"version":"v0.10.0","read_only":true}`+"\n" {
		t.Errorf("info = %s", body)
	}
}

func TestPatchExtraArgs(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	f.AddSession(core.Session{ID: "r1"})
	f.SetRouterSession("r1")
	do := serve(t, api.Config{Router: f})
	code, body := do(http.MethodPatch, "/sessions/s1", `{"extra_args":["--effort","high"]}`)
	if code != http.StatusOK || !strings.Contains(body, `"extra_args":["--effort","high"]`) {
		t.Errorf("patch = %d %s", code, body)
	}
	if _, body := do(http.MethodGet, "/sessions", ""); strings.Contains(body, "--effort") {
		t.Errorf("list carries launch options: %s", body)
	}
	if code, body := do(http.MethodPatch, "/sessions/r1", `{"extra_args":[]}`); code != http.StatusConflict {
		t.Errorf("router session = %d %s", code, body)
	}
}
