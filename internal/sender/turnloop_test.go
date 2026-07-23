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
	cancelGrace = 200 * time.Millisecond
	t.Cleanup(func() { cancelGrace = old })
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
