package pi

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

// fakePiWorker optionally emits agent_settled after each prompt.
func fakePiWorker(t *testing.T, settle bool) (*Runtime, *worker) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	settleCmd := ""
	if settle {
		settleCmd = `(sleep 0.3; printf '{"type":"agent_settled"}\n') &`
	}
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"type":"response","id":"%s","success":true,"data":{}}\n' "$id"
  case "$line" in
    *'"type":"prompt"'*) ` + settleCmd + ` ;;
    *'"type":"fake_agent_start"'*) printf '{"type":"agent_start"}\n' ;;
    *'"type":"fake_agent_settled"'*) printf '{"type":"agent_settled"}\n' ;;
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
	r := &Runtime{workers: map[string]*worker{}, max: 1, logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	// Through add(), like every production path, so the event pump runs.
	if err := r.add("s1", w); err != nil {
		t.Fatal(err)
	}
	return r, w
}

func collectPi(t *testing.T, ch <-chan backend.Event, timeout time.Duration) []backend.Event {
	t.Helper()
	var out []backend.Event
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timer.C:
			t.Fatalf("timed out; got %d events", len(out))
		}
	}
}

// Cancellation drains records through agent_settled.
func TestPromptCancelDrainsUntilAgentSettled(t *testing.T) {
	r, w := fakePiWorker(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := r.prompt(ctx, "s1", w, "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	// Records written after abort must still be collected.
	go func() {
		time.Sleep(100 * time.Millisecond)
		f, _ := os.OpenFile(w.path, os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = f.WriteString(`{"type":"message","id":"a1"}` + "\n")
		_ = f.Close()
	}()

	evs := collectPi(t, ch, 5*time.Second)
	var exits, messages, aborts int
	for _, ev := range evs {
		switch ev.Type {
		case backend.EventProcessExit:
			exits++
			// User cancellation should reconcile as a normal end.
			if reason := exitReason(t, ev); reason != "" {
				t.Errorf("cancelled turn exited with reason %q, want none", reason)
			}
		case backend.EventError:
			aborts++
		case "message":
			messages++
		}
	}
	if exits != 1 {
		t.Errorf("subprocess.exit count = %d, want 1", exits)
	}
	// Without this the interrupt leaves no trace in the UI at all.
	if aborts != 1 {
		t.Errorf("abort notice count = %d, want 1", aborts)
	}
	if messages != 1 {
		t.Errorf("records persisted after cancel reached the stream: %d, want 1", messages)
	}
	if n := len(w.c.events); n != 0 {
		t.Errorf("%d events left buffered for the next turn, want 0", n)
	}
}

// A worker that never settles must not hold the turn open forever.
func TestPromptCancelFinalizesWithoutAgentSettled(t *testing.T) {
	old := cancelGrace
	cancelGrace = 150 * time.Millisecond
	t.Cleanup(func() { cancelGrace = old })
	r, w := fakePiWorker(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := r.prompt(ctx, "s1", w, "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	evs := collectPi(t, ch, 5*time.Second)
	exits, aborts := 0, 0
	for _, ev := range evs {
		switch ev.Type {
		case backend.EventProcessExit:
			exits++
		case backend.EventError:
			aborts++
		}
	}
	if exits != 1 {
		t.Fatalf("subprocess.exit count = %d, want 1", exits)
	}
	// The grace-path finalization must report the interrupt too.
	if aborts != 1 {
		t.Errorf("abort notice count = %d, want 1", aborts)
	}
}

func exitReason(t *testing.T, ev backend.Event) string {
	t.Helper()
	var p struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Raw, &p); err != nil {
		t.Fatalf("exit payload: %v", err)
	}
	return p.Reason
}

// An input an extension consumed starts no run, so the turn must end on the
// prompt response instead of waiting for an agent_settled that never comes.
func TestHandledPromptEndsWithoutAgentSettled(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"type":"prompt"'*) printf '{"type":"response","id":"%s","success":true,"data":{"disposition":"handled"}}\n' "$id" ;;
    *) printf '{"type":"response","id":"%s","success":true,"data":{}}\n' "$id" ;;
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
	r := &Runtime{workers: map[string]*worker{}, max: 1, logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	if err := r.add("s1", w); err != nil {
		t.Fatal(err)
	}
	ch, err := r.startTurn(context.Background(), "s1", w, "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	events := collectPi(t, ch, 3*time.Second)
	if last := events[len(events)-1]; last.Type != backend.EventProcessExit {
		t.Fatalf("last event = %s, want %s", last.Type, backend.EventProcessExit)
	}
	for _, ev := range events {
		if ev.Type == backend.EventError {
			t.Fatalf("handled prompt reported an error: %s", ev.Raw)
		}
	}
}

// An extension can queue a run usher never asked for; pi's own agent_start is
// the only word of it.
func TestAgentLoopHoldsWorkerAgainstEviction(t *testing.T) {
	r, w := fakePiWorker(t, false)
	fakeEvent(t, w, "agent_start")
	waitLoop(t, r, w, true)
	if err := r.add("other", &worker{last: time.Now()}); err == nil || !strings.Contains(err.Error(), "all busy") {
		t.Fatalf("add alongside a running loop = %v, want all busy", err)
	}
	fakeEvent(t, w, "agent_settled")
	waitLoop(t, r, w, false)
}

// fakeEvent has the fake pi emit one event of its own.
func fakeEvent(t *testing.T, w *worker, event string) {
	t.Helper()
	if _, err := w.c.request(context.Background(), "fake_"+event, nil); err != nil {
		t.Fatal(err)
	}
}

func waitLoop(t *testing.T, r *Runtime, w *worker, want bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		got := w.loop
		r.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("loop = %v, want %v", got, want)
		}
	}
}

func TestAutoPauseWaitsForAgentLoop(t *testing.T) {
	r, w := fakePiWorker(t, false)
	fakeEvent(t, w, "agent_start")
	waitLoop(t, r, w, true)
	now := time.Now()
	r.reapIdle(now, time.Minute)
	r.reapIdle(now.Add(time.Hour), time.Minute)
	if !r.Has("s1") {
		t.Fatal("worker paused mid-run")
	}

	fakeEvent(t, w, "agent_settled")
	waitLoop(t, r, w, false)
	now = time.Now()
	r.reapIdle(now, time.Minute)
	if !r.Has("s1") {
		t.Fatal("worker paused the moment its run settled")
	}
	r.reapIdle(now.Add(2*time.Minute), time.Minute)
	if r.Has("s1") {
		t.Fatal("idle worker kept past the pause period")
	}
}
