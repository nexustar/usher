package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/agentprofile"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/schedule"
)

type stubStarter struct {
	id   string
	opts core.CreateOptions
}

func (s *stubStarter) StartSession(o core.CreateOptions) (string, error) {
	s.opts = o
	return s.id, nil
}

func scheduleServer(t *testing.T, starter schedule.Starter, profiles ...agentprofile.Profile) *Server {
	t.Helper()
	store, err := schedule.Load(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	agents := agentprofile.New(profiles)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Server{agents: agents, schedules: schedule.NewRunner(store, starter, agents, quiet)}
}

func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleCreateSchedule(rec, httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(body)))
	return rec
}

func TestScheduleCRUDHandlers(t *testing.T) {
	s := scheduleServer(t, &stubStarter{id: "sess-1"})

	created := post(t, s, `{"name":"nightly","enabled":true,"cron":"0 3 * * *","cwd":"/work","prompt":"Run the tests"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var task struct {
		ID      string `json:"id"`
		NextRun string `json:"next_run"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.ID == "" || task.NextRun == "" {
		t.Fatalf("create body = %s, want an id and a next_run", created.Body.String())
	}

	update := httptest.NewRequest(http.MethodPut, "/api/schedules/"+task.ID,
		strings.NewReader(`{"name":"nightly","enabled":true,"cron":"0 4 * * *","cwd":"/work","prompt":"Run the tests"}`))
	update.SetPathValue("id", task.ID)
	updated := httptest.NewRecorder()
	s.handleUpdateSchedule(updated, update)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"cron":"0 4 * * *"`) {
		t.Fatalf("update status = %d, body = %s", updated.Code, updated.Body.String())
	}

	list := httptest.NewRecorder()
	s.handleSchedules(list, httptest.NewRequest(http.MethodGet, "/api/schedules", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"name":"nightly"`) {
		t.Fatalf("list status = %d, body = %s", list.Code, list.Body.String())
	}
	// The pane labels the cron field with the server's clock, so the list has
	// to say which clock that is.
	if !strings.Contains(list.Body.String(), `"timezone"`) {
		t.Fatalf("list body has no timezone: %s", list.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/schedules/"+task.ID, nil)
	deleteReq.SetPathValue("id", task.ID)
	deleted := httptest.NewRecorder()
	s.handleDeleteSchedule(deleted, deleteReq)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", deleted.Code, deleted.Body.String())
	}
	if got := s.schedules.Store().List(); len(got) != 0 {
		t.Fatalf("schedules after delete = %+v", got)
	}

	// The id is gone now, so deleting it again is a 404 rather than a repeat
	// success.
	again := httptest.NewRequest(http.MethodDelete, "/api/schedules/"+task.ID, nil)
	again.SetPathValue("id", task.ID)
	missing := httptest.NewRecorder()
	s.handleDeleteSchedule(missing, again)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("delete of a gone id = %d, want 404", missing.Code)
	}
}

// The target is checked on save, not only when the task fires: a typo found at
// 3am is a run that silently did nothing.
func TestCreateScheduleRejectsBadTarget(t *testing.T) {
	s := scheduleServer(t, &stubStarter{})
	cases := map[string]string{
		"unknown agent":   `{"name":"a","cron":"0 3 * * *","agent":"ghost","prompt":"p"}`,
		"cwd is required": `{"name":"a","cron":"0 3 * * *","prompt":"p"}`,
		"5 fields":        `{"name":"a","cron":"nope","cwd":"/w","prompt":"p"}`,
		"prompt is":       `{"name":"a","cron":"0 3 * * *","cwd":"/w"}`,
	}
	for want, body := range cases {
		rec := post(t, s, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("create %s = %d %s, want 400 containing %q", body, rec.Code, rec.Body.String(), want)
		}
	}
}

// An agent supplies the cwd the request omits, and the fields it pins reach
// the session at fire time.
func TestScheduleResolvesAgentOnRun(t *testing.T) {
	starter := &stubStarter{id: "sess-9"}
	s := scheduleServer(t, starter, agentprofile.Profile{Name: "dev", Cwd: "/work/dev", Backend: "codex"})

	created := post(t, s, `{"name":"a","enabled":true,"cron":"0 3 * * *","agent":"dev","prompt":"Run the tests"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	id := s.schedules.Store().List()[0].ID

	run := httptest.NewRequest(http.MethodPost, "/api/schedules/"+id+"/run", nil)
	run.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleRunSchedule(rec, run)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "sess-9") {
		t.Fatalf("run status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := core.CreateOptions{Backend: "codex", Cwd: "/work/dev", InitialMessage: "Run the tests"}
	if !reflect.DeepEqual(starter.opts, want) {
		t.Fatalf("StartSession(%+v), want %+v", starter.opts, want)
	}
}
