package sender

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

// fakeResult and fakeDelta isolate the shared turn loop from a backend.
type fakeResult struct{ failed bool }
type fakeDelta struct{}

func hasType(evs []StreamEvent, typ string) bool {
	for _, e := range evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}

type turnLoopFixture struct {
	ch     <-chan StreamEvent
	cancel context.CancelFunc
	done   chan fakeResult
	path   string
}

func startTurnLoop(t *testing.T) turnLoopFixture {
	t.Helper()
	old := cancelGrace
	oldQuiet := finalDrainQuiet
	cancelGrace = 200 * time.Millisecond
	finalDrainQuiet = 30 * time.Millisecond
	t.Cleanup(func() {
		cancelGrace = old
		finalDrainQuiet = oldQuiet
	})
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan fakeResult, 1)
	tail := fastCfg()
	tail.contentOnly = true
	ch := mergeLoggedTurn(ctx, loggedTurnConfig[fakeResult, fakeDelta]{
		backend: "claude", idKey: "session_id", id: "s", path: path,
		tail: tail, done: done, deltas: make(chan fakeDelta),
		delta: func(fakeDelta) (string, string, bool) { return "text", "", false },
		result: func(ctx context.Context, out chan<- StreamEvent, r fakeResult) {
			if r.failed {
				emitError(ctx, out, "turn failed")
			}
		},
	})
	// Let the tailer replay the existing line.
	time.Sleep(50 * time.Millisecond)
	return turnLoopFixture{ch: ch, cancel: cancel, done: done, path: path}
}

// A cancelled turn must emit its exit boundary.
func TestMergeLoggedTurn_CancelEmitsExit(t *testing.T) {
	f := startTurnLoop(t)
	f.cancel()
	evs := collect(t, f.ch, 5*time.Second)
	if !hasType(evs, backend.EventProcessExit) {
		t.Fatalf("cancelled turn produced no %s: %v", backend.EventProcessExit, types(evs))
	}
}

// Cancellation must drain records written before the backend result.
func TestMergeLoggedTurn_CancelDrainsLateBackendResult(t *testing.T) {
	f := startTurnLoop(t)
	f.cancel()
	go func() {
		time.Sleep(80 * time.Millisecond)
		appendLines(f.path, 0, `{"type":"assistant"}`)
		f.done <- fakeResult{}
	}()
	evs := collect(t, f.ch, 5*time.Second)
	if !hasType(evs, backend.EventProcessExit) {
		t.Fatalf("cancelled turn produced no %s: %v", backend.EventProcessExit, types(evs))
	}
	if !hasType(evs, "assistant") {
		t.Fatalf("cancel dropped a record persisted before the interrupt landed: %v", types(evs))
	}
}

// A backend that never answers the interrupt must not hold the turn open.
func TestMergeLoggedTurn_CancelFinalizesWithoutBackendResult(t *testing.T) {
	f := startTurnLoop(t)
	f.cancel()
	evs := collect(t, f.ch, 5*time.Second)
	if !hasType(evs, backend.EventProcessExit) {
		t.Fatalf("cancelled turn produced no %s: %v", backend.EventProcessExit, types(evs))
	}
}

// Normal completion is unchanged: the backend result finalizes the turn and the
// tail is drained behind it.
func TestMergeLoggedTurn_CompletionDrainsTail(t *testing.T) {
	f := startTurnLoop(t)
	appendLines(f.path, 0, `{"type":"assistant"}`)
	f.done <- fakeResult{}
	evs := collect(t, f.ch, 5*time.Second)
	if !hasType(evs, "assistant") || !hasType(evs, backend.EventProcessExit) {
		t.Fatalf("completed turn missing assistant/exit: %v", types(evs))
	}
	if hasType(evs, backend.EventError) {
		t.Fatalf("clean turn emitted an error: %v", types(evs))
	}
}

func TestMergeLoggedTurn_CompletionWaitsForDelayedRecord(t *testing.T) {
	f := startTurnLoop(t)
	f.done <- fakeResult{}
	go func() {
		time.Sleep(10 * time.Millisecond)
		appendLines(f.path, 0, `{"type":"assistant"}`)
	}()
	evs := collect(t, f.ch, 5*time.Second)
	if !hasType(evs, "assistant") {
		t.Fatalf("completion dropped delayed assistant: %v", types(evs))
	}
}

// The jsonl is flushed before the protocol result, so the end-of-turn marker is
// usually forwarded by the main loop before completion arrives. The loop must
// remember it and finalize at once — not wait out the silence window for a
// marker drainTail will never see again.
func TestMergeLoggedTurn_MarkerBeforeDoneSkipsQuiet(t *testing.T) {
	oldQuiet := finalDrainQuiet
	finalDrainQuiet = 10 * time.Second // a quiet-based stop would blow the deadline
	t.Cleanup(func() { finalDrainQuiet = oldQuiet })

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan fakeResult, 1)
	tail := fastCfg()
	tail.contentOnly = true
	tail.turnComplete = isTurnComplete
	ch := mergeLoggedTurn(ctx, loggedTurnConfig[fakeResult, fakeDelta]{
		backend: "claude", idKey: "session_id", id: "s", path: path,
		tail: tail, done: done, deltas: make(chan fakeDelta),
		delta:  func(fakeDelta) (string, string, bool) { return "text", "", false },
		result: func(context.Context, chan<- StreamEvent, fakeResult) {},
	})
	// Forward the marker through the main loop, then complete after it landed.
	appendLines(path, 0, `{"type":"system","subtype":"turn_duration"}`)
	time.Sleep(80 * time.Millisecond)
	done <- fakeResult{}

	// collect fails the test on timeout, so finishing under 3s proves the quiet
	// window was skipped.
	evs := collect(t, ch, 3*time.Second)
	if !hasType(evs, backend.EventProcessExit) {
		t.Fatalf("no exit after marker: %v", types(evs))
	}
}

// Same on the cancel path: finishCancelled can forward the abort marker while
// waiting for the backend result, so it must remember it and skip the quiet
// window too.
func TestMergeLoggedTurn_CancelMarkerBeforeResultSkipsQuiet(t *testing.T) {
	oldQuiet, oldGrace := finalDrainQuiet, cancelGrace
	finalDrainQuiet = 10 * time.Second // a quiet-based stop would blow the deadline
	cancelGrace = 10 * time.Second     // so the wait ends on the result, not grace
	t.Cleanup(func() { finalDrainQuiet = oldQuiet; cancelGrace = oldGrace })

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan fakeResult, 1)
	tail := fastCfg()
	tail.contentOnly = true
	tail.turnAborted = isClaudeTurnAborted
	ch := mergeLoggedTurn(ctx, loggedTurnConfig[fakeResult, fakeDelta]{
		backend: "claude", idKey: "session_id", id: "s", path: path,
		tail: tail, done: done, deltas: make(chan fakeDelta),
		delta:  func(fakeDelta) (string, string, bool) { return "text", "", false },
		result: func(context.Context, chan<- StreamEvent, fakeResult) {},
	})
	time.Sleep(50 * time.Millisecond) // tailer replays the existing line
	cancel()                          // -> finishCancelled, now waiting for the result
	// The interrupt lands: the abort marker is forwarded, then the result arrives.
	appendLines(path, 0, `{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`)
	time.Sleep(80 * time.Millisecond)
	done <- fakeResult{}

	evs := collect(t, ch, 3*time.Second)
	if !hasType(evs, backend.EventProcessExit) {
		t.Fatalf("no exit after abort marker: %v", types(evs))
	}
}

// lateLogTurn starts a turn for a session whose log does not exist yet.
func lateLogTurn(t *testing.T) (ch <-chan StreamEvent, done chan fakeResult, path string) {
	t.Helper()
	oldQuiet := finalDrainQuiet
	finalDrainQuiet = 30 * time.Millisecond
	t.Cleanup(func() { finalDrainQuiet = oldQuiet })
	path = filepath.Join(t.TempDir(), "s.jsonl")
	done = make(chan fakeResult, 1)
	tail := fastCfg()
	tail.contentOnly = true
	ch = mergeLoggedTurn(context.Background(), loggedTurnConfig[fakeResult, fakeDelta]{
		backend: "claude", idKey: "session_id", id: "s", poll: 5 * time.Millisecond,
		locate: func() string {
			if _, err := os.Stat(path); err != nil {
				return ""
			}
			return path
		},
		tail: tail, done: done, deltas: make(chan fakeDelta),
		delta:  func(fakeDelta) (string, string, bool) { return "text", "", false },
		result: func(context.Context, chan<- StreamEvent, fakeResult) {},
	})
	return ch, done, path
}

// A backend may take any time to start on a prompt: the turn waits for the log
// as long as the turn itself lasts, then reads it from the top.
func TestMergeLoggedTurn_WaitsForLateLog(t *testing.T) {
	ch, done, path := lateLogTurn(t)
	time.Sleep(150 * time.Millisecond)
	select {
	case ev := <-ch:
		if ev.Type != backend.EventProcessStarted {
			t.Fatalf("before the log appeared: %s %s", ev.Type, ev.Raw)
		}
	default:
		t.Fatal("no start event")
	}
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"+`{"type":"assistant"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	done <- fakeResult{}
	evs := collect(t, ch, time.Second)
	if hasType(evs, backend.EventError) {
		t.Fatalf("late log reported as an error: %+v", evs)
	}
	lines := 0
	for _, e := range evs {
		if e.Type == "user" || e.Type == "assistant" {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("read %d log lines of 2: %+v", lines, evs)
	}
}

// A turn that ends with no log ever written is the one case that is an error.
func TestMergeLoggedTurn_EndsWithoutLog(t *testing.T) {
	ch, done, _ := lateLogTurn(t)
	done <- fakeResult{}
	evs := collect(t, ch, time.Second)
	if !hasType(evs, backend.EventError) {
		t.Fatalf("no error for a turn that left no log: %+v", evs)
	}
}
