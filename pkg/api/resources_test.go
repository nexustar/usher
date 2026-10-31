package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nexustar/usher/pkg/agentprofile"
	"github.com/nexustar/usher/pkg/agentupdate"
	"github.com/nexustar/usher/pkg/api"
	"github.com/nexustar/usher/pkg/core"
	"github.com/nexustar/usher/pkg/pluginapi/pluginapitest"
	"github.com/nexustar/usher/pkg/schedule"
)

func TestAgentCRUD(t *testing.T) {
	store, err := agentprofile.Load(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	do := serve(t, api.Config{Router: pluginapitest.NewFakeRouter(), Agents: store})

	if code, body := do(http.MethodPost, "/agents", `{"name":"dev","backend":"codex"}`); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	// A different name in the body renames the agent, so the response — not the
	// request path — is what identifies it afterwards.
	if code, body := do(http.MethodPut, "/agents/dev", `{"name":"dev-renamed","model":"test-model"}`); code != http.StatusOK || !strings.Contains(body, `"name":"dev-renamed"`) {
		t.Fatalf("update = %d %s", code, body)
	}
	if code, body := do(http.MethodDelete, "/agents/dev-renamed", ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", code, body)
	}
	if got := store.List(); len(got) != 1 || !got[0].Builtin {
		t.Fatalf("agents after delete = %+v", got)
	}
}

type stubStarter struct {
	id   string
	opts core.CreateOptions
}

func (s *stubStarter) StartSession(o core.CreateOptions) (string, error) {
	s.opts = o
	return s.id, nil
}

func scheduleServer(t *testing.T, starter schedule.Starter, profiles ...agentprofile.Profile) (*schedule.Runner, func(method, path, body string) (int, string)) {
	t.Helper()
	store, err := schedule.Load(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	agents := agentprofile.New(profiles)
	runner := schedule.NewRunner(store, starter, agents, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return runner, serve(t, api.Config{Router: pluginapitest.NewFakeRouter(), Agents: agents, Schedules: runner})
}

func TestScheduleCRUD(t *testing.T) {
	runner, do := scheduleServer(t, &stubStarter{id: "sess-1"})

	code, body := do(http.MethodPost, "/schedules", `{"name":"nightly","enabled":true,"cron":"0 3 * * *","cwd":"/work","prompt":"Run the tests"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var task struct {
		ID      string `json:"id"`
		NextRun string `json:"next_run"`
	}
	if err := json.Unmarshal([]byte(body), &task); err != nil {
		t.Fatal(err)
	}
	if task.ID == "" || task.NextRun == "" {
		t.Fatalf("create body = %s, want an id and a next_run", body)
	}

	if code, body := do(http.MethodPut, "/schedules/"+task.ID, `{"name":"nightly","enabled":true,"cron":"0 4 * * *","cwd":"/work","prompt":"Run the tests"}`); code != http.StatusOK || !strings.Contains(body, `"cron":"0 4 * * *"`) {
		t.Fatalf("update = %d %s", code, body)
	}
	// The pane labels the cron field with the server's clock, so the list has
	// to say which clock that is.
	if code, body := do(http.MethodGet, "/schedules", ""); code != http.StatusOK || !strings.Contains(body, `"name":"nightly"`) || !strings.Contains(body, `"timezone"`) {
		t.Fatalf("list = %d %s", code, body)
	}
	if code, body := do(http.MethodDelete, "/schedules/"+task.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", code, body)
	}
	if got := runner.Store().List(); len(got) != 0 {
		t.Fatalf("schedules after delete = %+v", got)
	}
	if code, _ := do(http.MethodDelete, "/schedules/"+task.ID, ""); code != http.StatusNotFound {
		t.Fatalf("delete of a gone id = %d, want 404", code)
	}
}

// The target is checked on save, not only when the task fires: a typo found at
// 3am is a run that silently did nothing.
func TestCreateScheduleRejectsBadTarget(t *testing.T) {
	_, do := scheduleServer(t, &stubStarter{})
	cases := map[string]string{
		"unknown agent":   `{"name":"a","cron":"0 3 * * *","agent":"ghost","prompt":"p"}`,
		"cwd is required": `{"name":"a","cron":"0 3 * * *","prompt":"p"}`,
		"5 fields":        `{"name":"a","cron":"nope","cwd":"/w","prompt":"p"}`,
		"prompt is":       `{"name":"a","cron":"0 3 * * *","cwd":"/w"}`,
	}
	for want, body := range cases {
		if code, got := do(http.MethodPost, "/schedules", body); code != http.StatusBadRequest || !strings.Contains(got, want) {
			t.Errorf("create %s = %d %s, want 400 containing %q", body, code, got, want)
		}
	}
}

// An agent supplies the cwd the request omits, and the fields it pins reach
// the session at fire time.
func TestScheduleResolvesAgentOnRun(t *testing.T) {
	starter := &stubStarter{id: "sess-9"}
	runner, do := scheduleServer(t, starter, agentprofile.Profile{Name: "dev", Cwd: "/work/dev", Backend: "codex"})

	if code, body := do(http.MethodPost, "/schedules", `{"name":"a","enabled":true,"cron":"0 3 * * *","agent":"dev","prompt":"Run the tests"}`); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	id := runner.Store().List()[0].ID
	if code, body := do(http.MethodPost, "/schedules/"+id+"/run", ""); code != http.StatusCreated || !strings.Contains(body, `"id":"sess-9"`) {
		t.Fatalf("run = %d %s", code, body)
	}
	want := core.CreateOptions{Backend: "codex", Cwd: "/work/dev", InitialMessage: "Run the tests"}
	if !reflect.DeepEqual(starter.opts, want) {
		t.Fatalf("StartSession(%+v), want %+v", starter.opts, want)
	}
}

// serve returns a request helper for paths below api.Prefix.
func serve(t *testing.T, c api.Config) func(method, path, body string) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	api.New(c).Register(mux)
	return func(method, path, body string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, api.Prefix+path, strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
}

func TestUpdateRoutes(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho 1.0.0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := agentupdate.Spec{
		Name: "claude", Cmd: cli,
		Latest: func(context.Context) (string, error) { return "1.1.0", nil },
	}
	m := agentupdate.New(t.Context(), []agentupdate.Spec{spec}, slog.New(slog.DiscardHandler))
	do := serve(t, api.Config{Router: pluginapitest.NewFakeRouter(), Updates: m})

	code, body := do(http.MethodGet, "/updates?refresh=1", "")
	if code != http.StatusOK || !strings.Contains(body, `"latest":"1.1.0"`) || !strings.Contains(body, `"update_available":true`) {
		t.Fatalf("GET /updates = %d %s", code, body)
	}
	if code, _ := do(http.MethodPost, "/updates/nope", ""); code != http.StatusNotFound {
		t.Errorf("unknown agent = %d", code)
	}
	if code, _ := do(http.MethodPost, "/updates/claude", ""); code != http.StatusAccepted {
		t.Errorf("start = %d", code)
	}
}

func TestUpdateRoutesAbsentWithoutManager(t *testing.T) {
	do := serve(t, api.Config{Router: pluginapitest.NewFakeRouter()})
	if code, _ := do(http.MethodGet, "/updates", ""); code != http.StatusNotFound {
		t.Errorf("GET /updates = %d", code)
	}
}
