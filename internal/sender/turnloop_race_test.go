package sender

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

// Concurrent completion and cancellation must still emit an exit.
func TestMergeLoggedTurn_CompletionRacingCancelStillExits(t *testing.T) {
	old := cancelGrace
	cancelGrace = 200 * time.Millisecond
	t.Cleanup(func() { cancelGrace = old })

	const attempts = 25
	for i := 0; i < attempts; i++ {
		path := filepath.Join(t.TempDir(), "s.jsonl")
		if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan fakeResult, 1)
		tail := fastCfg()
		tail.contentOnly = true
		ch := mergeLoggedTurn(ctx, loggedTurnConfig[fakeResult, fakeDelta]{
			backend: "claude", idKey: "session_id", id: "s", path: path,
			tail: tail, done: done, deltas: make(chan fakeDelta),
			delta:  func(fakeDelta) (string, string, bool) { return "text", "", false },
			result: func(context.Context, chan<- StreamEvent, fakeResult) {},
		})

		// Let the loop reach its event select.
		time.Sleep(20 * time.Millisecond)

		// Make completion and cancellation ready together.
		var start sync.WaitGroup
		start.Add(1)
		var fired sync.WaitGroup
		fired.Add(2)
		go func() { defer fired.Done(); start.Wait(); done <- fakeResult{} }()
		go func() { defer fired.Done(); start.Wait(); cancel() }()
		start.Done()
		fired.Wait()

		evs := collect(t, ch, 5*time.Second)
		cancel()
		if !hasType(evs, backend.EventProcessExit) {
			t.Fatalf("attempt %d: completion racing cancel produced no %s: %v",
				i, backend.EventProcessExit, types(evs))
		}
	}
}
