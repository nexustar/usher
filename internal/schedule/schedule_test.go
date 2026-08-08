package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/core"
)

func testStore(t *testing.T, tasks ...Task) *Store {
	t.Helper()
	store, err := Load(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if _, err := store.Create(task); err != nil {
			t.Fatalf("Create(%q): %v", task.Name, err)
		}
	}
	return store
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(Task{
		Name: "nightly", Enabled: true, Cron: "0 3 * * *",
		Agent: "dev", Prompt: "Run the tests", Cwd: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("Create returned no id")
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.List()
	if len(got) != 1 || got[0] != created {
		t.Fatalf("reloaded = %+v, want [%+v]", got, created)
	}
}

// A missing file is the state before the first task, not an error.
func TestLoadMissingFile(t *testing.T) {
	store, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || len(store.List()) != 0 {
		t.Fatalf("Load = %v, %v", store.List(), err)
	}
}

func TestLoadRejectsBadCron(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	data := `{"schedules":[{"id":"a","name":"x","prompt":"p","cron":"nope"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an invalid cron expression")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]Task{
		"name is required":   {Prompt: "p", Cron: "* * * * *"},
		"prompt is required": {Name: "n", Cron: "* * * * *"},
		"got 0":              {Name: "n", Prompt: "p"},
		"got 1":              {Name: "n", Prompt: "p", Cron: "nope"},
	}
	for want, task := range cases {
		if _, err := testStore(t).Create(task); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Create(%+v) = %v, want error containing %q", task, err, want)
		}
	}
}

// Update keeps the id, so a client can PUT back the object it was handed.
func TestUpdate(t *testing.T) {
	store := testStore(t, Task{Name: "a", Enabled: true, Cron: "0 3 * * *", Prompt: "p"})
	task := store.List()[0]

	updated, err := store.Update(task.ID, Task{Name: "b", Cron: "0 4 * * *", Prompt: "q"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != task.ID || updated.Cron != "0 4 * * *" {
		t.Fatalf("Update = %+v, want the id kept and the spec replaced", updated)
	}
	if _, err := store.Update("missing", Task{Name: "b", Cron: "0 4 * * *", Prompt: "q"}); err == nil {
		t.Fatal("Update of a missing task succeeded")
	}
}

// A failed write leaves the store as it was, rather than half-applied.
func TestWriteFailureLeavesStoreUnchanged(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, Task{Name: "a", Enabled: true, Cron: "0 3 * * *", Prompt: "p"})
	before := store.List()

	// A directory where the file goes: the rename cannot land.
	store.path = filepath.Join(dir, "blocked")
	if err := os.MkdirAll(store.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Task{Name: "b", Enabled: true, Cron: "0 4 * * *", Prompt: "p"}); err == nil {
		t.Fatal("Create succeeded with an unwritable path")
	}
	if got := store.List(); len(got) != len(before) || got[0] != before[0] {
		t.Fatalf("store after failed write = %+v, want %+v", got, before)
	}
}

func TestDeleteAndGet(t *testing.T) {
	store := testStore(t, Task{Name: "a", Cron: "* * * * *", Prompt: "p"})
	id := store.List()[0].ID
	if err := store.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(id); ok {
		t.Fatal("Get found a deleted task")
	}
	if err := store.Delete(id); err == nil {
		t.Fatal("Delete of a missing task succeeded")
	}
}

func TestNextRun(t *testing.T) {
	now := at(t, "2026-07-28 09:00")
	cases := []struct {
		name string
		task Task
		want time.Time
	}{
		{"cron", Task{Enabled: true, Cron: "0 12 * * *"}, at(t, "2026-07-28 12:00")},
		{"disabled", Task{Cron: "0 12 * * *"}, time.Time{}},
	}
	for _, c := range cases {
		if got := c.task.NextRun(now); !got.Equal(c.want) {
			t.Errorf("%s: NextRun = %s, want %s", c.name, got, c.want)
		}
	}
}

// --- runner ---------------------------------------------------------------

type fakeStarter struct {
	mu    sync.Mutex
	opts  []core.CreateOptions
	id    string
	err   error
	block chan struct{}
}

func (f *fakeStarter) StartSession(o core.CreateOptions) (string, error) {
	f.mu.Lock()
	f.opts = append(f.opts, o)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return f.id, f.err
}

func (f *fakeStarter) calls() []core.CreateOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.CreateOptions(nil), f.opts...)
}

// passThrough stands in for the agent store: it applies no defaults, so tests
// see exactly the fields the task carried.
type passThrough struct{ err error }

func (p passThrough) Resolve(agent, cwd, backend, model string) (core.CreateOptions, error) {
	return core.CreateOptions{Cwd: cwd, Backend: backend, Model: model}, p.err
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestRunner(store *Store, starter Starter, resolver Resolver) *Runner {
	return NewRunner(store, starter, resolver, quietLogger())
}

// waitFor polls until cond holds, for the one test that drives the tick loop.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the run to land")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCheckFiresCronOncePerWindow(t *testing.T) {
	store := testStore(t, Task{
		Name: "nightly", Enabled: true, Cron: "0 3 * * *",
		Cwd: "/work", Backend: "claude", Prompt: "Run the tests",
	})
	starter := &fakeStarter{id: "sess-1"}
	runner := newTestRunner(store, starter, passThrough{})

	// A window that contains 03:00 fires; the next one, which does not, is
	// silent even though it follows immediately.
	runner.check(at(t, "2026-07-28 02:59"), at(t, "2026-07-28 03:01"))
	got := starter.calls()
	want := core.CreateOptions{Cwd: "/work", Backend: "claude", InitialMessage: "Run the tests"}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("StartSession(%+v), want one call with %+v", got, want)
	}
	runner.check(at(t, "2026-07-28 03:01"), at(t, "2026-07-28 03:03"))
	if calls := starter.calls(); len(calls) != 1 {
		t.Fatalf("fired %d times, want 1", len(calls))
	}
	if task := store.List()[0]; !task.Enabled {
		t.Fatalf("after run = %+v, want a repeat left armed", task)
	}
}

// A window spanning several matches — a slept laptop, a long stop-the-world —
// starts one session, not one per missed match.
func TestCheckCollapsesMissedMatches(t *testing.T) {
	store := testStore(t, Task{Name: "hourly", Enabled: true, Cron: "0 * * * *", Prompt: "p", Cwd: "/w"})
	starter := &fakeStarter{id: "s"}
	newTestRunner(store, starter, passThrough{}).
		check(at(t, "2026-07-28 01:00"), at(t, "2026-07-28 09:00"))
	if calls := starter.calls(); len(calls) != 1 {
		t.Fatalf("fired %d times, want 1", len(calls))
	}
}

func TestCheckSkipsDisabled(t *testing.T) {
	store := testStore(t, Task{Name: "off", Cron: "* * * * *", Prompt: "p", Cwd: "/w"})
	starter := &fakeStarter{id: "s"}
	newTestRunner(store, starter, passThrough{}).
		check(at(t, "2026-07-28 08:59"), at(t, "2026-07-28 09:01"))
	if calls := starter.calls(); len(calls) != 0 {
		t.Fatalf("a disabled task fired %d times", len(calls))
	}
}

// A failed run is reported and changes nothing: the task stays armed for its
// next match.
func TestRunNowReportsFailure(t *testing.T) {
	store := testStore(t, Task{Name: "a", Enabled: true, Cron: "0 3 * * *", Prompt: "p", Cwd: "/w"})
	id := store.List()[0].ID
	runner := newTestRunner(store, &fakeStarter{}, passThrough{err: errors.New(`unknown agent "gone"`)})

	if _, err := runner.RunNow(id); err == nil {
		t.Fatal("RunNow reported success")
	}
	if task, _ := store.Get(id); !task.Enabled {
		t.Fatalf("after a failed run = %+v, want it untouched", task)
	}
	if _, err := runner.RunNow("missing"); err == nil {
		t.Fatal("RunNow accepted an unknown id")
	}
}

// Runs never overlap: a second one waits for the backend in front of it rather
// than starting alongside.
func TestRunsAreSerialized(t *testing.T) {
	store := testStore(t, Task{Name: "a", Enabled: true, Cron: "* * * * *", Prompt: "p", Cwd: "/w"})
	starter := &fakeStarter{id: "s", block: make(chan struct{})}
	runner := newTestRunner(store, starter, passThrough{})

	first := make(chan struct{})
	go func() {
		defer close(first)
		runner.check(at(t, "2026-07-28 08:59"), at(t, "2026-07-28 09:00"))
	}()
	waitFor(t, func() bool { return len(starter.calls()) == 1 })

	second := make(chan struct{})
	go func() {
		defer close(second)
		_, _ = runner.RunNow(store.List()[0].ID)
	}()
	time.Sleep(20 * time.Millisecond)
	if calls := starter.calls(); len(calls) != 1 {
		t.Fatalf("%d sessions started while one was in flight, want 1", len(calls))
	}

	close(starter.block)
	<-first
	<-second
	if calls := starter.calls(); len(calls) != 2 {
		t.Fatalf("%d sessions started in total, want 2", len(calls))
	}
}

// Exercises the tick loop itself. Its clock advances a minute per reading, so
// every window straddles a minute boundary and an every-minute task is always
// due; wall time would have the test wait for a real one.
func TestRunStopsWithContext(t *testing.T) {
	store := testStore(t, Task{Name: "a", Enabled: true, Cron: "* * * * *", Prompt: "p", Cwd: "/w"})
	starter := &fakeStarter{id: "s"}
	runner := newTestRunner(store, starter, passThrough{})
	runner.interval = time.Millisecond
	clock := at(t, "2026-07-28 09:00")
	runner.now = func() time.Time { clock = clock.Add(time.Minute); return clock }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runner.Run(ctx) }()
	waitFor(t, func() bool { return len(starter.calls()) >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
