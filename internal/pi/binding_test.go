package pi

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

// fakePiRuntime builds a worker bound to "s1" whose fake pi answers get_state
// with stateData and runs afterPrompt (shell) once a prompt arrives.
func fakePiRuntime(t *testing.T, stateData, afterPrompt string) (*Runtime, *worker) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	// Every received line lands in rpc.log (cwd is the worker dir) so a test can
	// assert on what usher sent back.
	body := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> rpc.log
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"type":"get_state"'*)
      printf '{"type":"response","id":"%s","success":true,"data":` + stateData + `}\n' "$id"
      ;;
    *'"type":"prompt"'*)
      printf '{"type":"response","id":"%s","success":true,"data":{}}\n' "$id"
      ` + afterPrompt + `
      ;;
    *)
      printf '{"type":"response","id":"%s","success":true,"data":{}}\n' "$id"
      ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := startClient(bin, dir, "", dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.stop)
	w := &worker{c: c, cwd: dir, path: path, last: time.Now()}
	r := &Runtime{
		workers: map[string]*worker{},
		max:     1,
		logger:  slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	// Through add(), like every production path, so the event pump runs.
	if err := r.add("s1", w); err != nil {
		t.Fatal(err)
	}
	return r, w
}

func hasEvent(events []backend.Event, typ string) bool {
	for _, ev := range events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// An extension command can move a worker to another session (fork, clone,
// new_session, switch_session, navigateTree) without pi announcing it. usher
// must notice through get_state and release the worker rather than write the
// next prompt into the wrong file.
func TestSwitchedWorkerIsDropped(t *testing.T) {
	r, w := fakePiRuntime(t,
		`{"sessionId":"other","sessionFile":"/tmp/other.jsonl","thinkingLevel":"high"}`,
		`(sleep 0.2; printf '{"type":"agent_settled"}\n') &`)

	ch, err := r.Send(context.Background(), "s1", "hello", w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	events := collectPi(t, ch, 5*time.Second)
	if !hasEvent(events, backend.EventError) {
		t.Fatalf("no error event reported the switch: %+v", events)
	}
	// The snapshot describes the session pi moved to, so it must not be
	// published as this session's usage.
	if hasEvent(events, backend.EventRuntime) {
		t.Fatalf("published the switched session's runtime: %+v", events)
	}
	r.mu.Lock()
	still := r.workers["s1"]
	r.mu.Unlock()
	if still != nil {
		t.Fatal("switched worker is still bound to s1")
	}
}

// A worker that still reports its own session keeps its binding, and its usage
// snapshot is published as usual.
func TestMatchingSessionKeepsWorker(t *testing.T) {
	r, w := fakePiRuntime(t,
		`{"sessionId":"s1","sessionFile":"/tmp/s1.jsonl","thinkingLevel":"low"}`,
		`(sleep 0.2; printf '{"type":"agent_settled"}\n') &`)

	ch, err := r.Send(context.Background(), "s1", "hello", w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	events := collectPi(t, ch, 5*time.Second)
	if hasEvent(events, backend.EventError) {
		t.Fatalf("unexpected error event: %+v", events)
	}
	if !hasEvent(events, backend.EventRuntime) {
		t.Fatalf("no runtime snapshot: %+v", events)
	}
	r.mu.Lock()
	still := r.workers["s1"]
	r.mu.Unlock()
	if still == nil {
		t.Fatal("worker was dropped despite serving s1")
	}
}
