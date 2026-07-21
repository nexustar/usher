package appserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeAppServer(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "workers.log")
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
printf 'start %s\n' "$$" >> "$FAKE_LOG"
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"fake/1"}}'
IFS= read -r line
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"thread/start"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"new-thread"}}}' ;;
    *'"method":"thread/resume"'*)
      printf 'resume\n' >> "$FAKE_LOG"
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
    *'"method":"turn/start"'*)
      thread=$(printf '%s' "$line" | sed -n 's/.*"threadId":"\([^"]*\)".*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%s","turn":{"status":"completed"}}}\n' "$thread" ;;
    *'"method":"skills/list"'*)
      printf '%s\n' "$line" >> "$FAKE_LOG"
      if [ -n "$FAKE_SKILLS_DELAY" ]; then sleep "$FAKE_SKILLS_DELAY"; fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"cwd":"/tmp","skills":[{"name":"imagegen","description":"make images","path":"/skills/imagegen/SKILL.md","scope":"user","enabled":true}],"errors":[]}]}}\n' "$id" ;;
    *'"method":"thread/compact/start"'*|*'"method":"review/start"'*)
      printf '%s\n' "$line" >> "$FAKE_LOG"
      thread=$(printf '%s' "$line" | sed -n 's/.*"threadId":"\([^"]*\)".*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%s","turn":{"status":"completed"}}}\n' "$thread" ;;
    *'"method":"turn/interrupt"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, logPath
}

func TestManagerLRUEvictsIdleWorkerAndColdResumes(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath}, 1, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := m.StartThread(ctx, "/tmp", "")
	if err != nil {
		t.Fatal(err)
	}
	turn, _, err := m.StartTurn(ctx, id, "one", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	<-turn
	turn, _, err = m.StartTurn(ctx, "resumed-thread", "two", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	<-turn
	m.Shutdown()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "start ") != 2 || strings.Count(string(b), "resume") != 1 {
		t.Fatalf("worker lifecycle log = %q", b)
	}
	if m.Has(id) {
		t.Fatalf("LRU did not evict %s", id)
	}
}

func TestManagerConcurrentResumeStartsOneWorker(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath}, 2, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.getOrResume(ctx, "same-thread", "/tmp")
			errs <- err
		}()
	}
	wg.Wait()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	m.Shutdown()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "start ") != 1 || strings.Count(string(b), "resume") != 1 {
		t.Fatalf("concurrent resume log = %q", b)
	}
}

func TestManagerWorkerFailureIsIsolated(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath}, 2, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := m.getOrResume(ctx, "first", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.getOrResume(ctx, "second", "/tmp"); err != nil {
		t.Fatal(err)
	}
	first.client.mu.Lock()
	cmd := first.client.cmd
	first.client.mu.Unlock()
	first.client.stopProcess(cmd, context.Canceled)
	if m.Has("first") {
		t.Error("failed worker still reported live")
	}
	if !m.Has("second") {
		t.Error("unrelated worker was lost")
	}
	m.Shutdown()
}

func TestManagerMaxLiveRejectsWhenAllWorkersBusy(t *testing.T) {
	m := NewManager("unused", nil, nil, nil, nil, 1, nil)
	m.workers["busy"] = &worker{client: m.newClient(), busy: true, lastUsed: time.Now()}
	if _, err := m.reserve(); err == nil || !strings.Contains(err.Error(), "all busy") {
		t.Fatalf("reserve error = %v, want all busy", err)
	}
}

func TestManagerDiscoversSkillsAndRunsCodexCommands(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath}, 1, nil)
	defer m.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Composer completion must not start a worker just to fill a popup.
	cold, complete, err := m.SkillsIfLive(ctx, "thread-1", "/tmp")
	if err != nil || cold != nil || complete {
		t.Fatalf("SkillsIfLive on cold session = (%#v, %v, %v), want (nil, false, nil)", cold, complete, err)
	}
	m.mu.Lock()
	spawned := len(m.workers)
	m.mu.Unlock()
	if spawned != 0 {
		t.Fatalf("SkillsIfLive registered %d worker(s) for a cold session", spawned)
	}

	skills, err := m.Skills(ctx, "thread-1", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	wantSkills := []Skill{{Name: "imagegen", Description: "make images", Path: "/skills/imagegen/SKILL.md", Enabled: true}}
	if !reflect.DeepEqual(skills, wantSkills) {
		t.Fatalf("Skills = %#v, want %#v", skills, wantSkills)
	}
	// Now that the worker is up, the cheap path answers too.
	warm, complete, err := m.SkillsIfLive(ctx, "thread-1", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(warm, wantSkills) {
		t.Fatalf("SkillsIfLive on live session = %#v, want %#v", warm, wantSkills)
	}
	if !complete {
		t.Fatal("SkillsIfLive on live session reported incomplete catalog")
	}
	compact, _, err := m.Compact(ctx, "thread-1", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if got := (<-compact).Status; got != "completed" {
		t.Fatalf("compact status = %q", got)
	}
	review, _, err := m.Review(ctx, "thread-1", "/tmp", "check auth")
	if err != nil {
		t.Fatal(err)
	}
	if got := (<-review).Status; got != "completed" {
		t.Fatalf("review status = %q", got)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, `"method":"thread/compact/start"`) {
		t.Fatalf("compact request missing: %s", log)
	}
	if !strings.Contains(log, `"method":"review/start"`) || !strings.Contains(log, `"type":"custom"`) || !strings.Contains(log, `"instructions":"check auth"`) {
		t.Fatalf("custom review request missing: %s", log)
	}
}

// A skills lookup is not a turn, so Client.Busy stays false for its duration.
// Without an explicit lease the worker looks idle and reserve will shut it down
// mid-RPC — on the send path that turns a /skill into a plain prose prompt.
func TestSkillsLookupLeasesWorkerAgainstEviction(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath, "FAKE_SKILLS_DELAY=0.3"}, 1, nil)
	defer m.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := m.getOrResume(ctx, "thread-1", "/tmp"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.Skills(ctx, "thread-1", "/tmp")
		done <- err
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		w := m.workers["thread-1"]
		inFlight := w != nil && w.leases > 0
		m.mu.Unlock()
		if inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("skills lookup never leased its worker")
		}
		time.Sleep(time.Millisecond)
	}

	// maxLive is 1 and the only worker is mid-lookup: reserve has to refuse
	// rather than hand back a client it is about to shut down.
	if _, err := m.reserve(); err == nil {
		t.Fatal("reserve evicted a worker with a skills lookup in flight")
	}
	if err := <-done; err != nil {
		t.Fatalf("skills lookup failed: %v", err)
	}
}

// startOperation's window is too narrow to hit deterministically, so assert the
// invariant it leans on instead: a worker handed back by leaseWorker() is already
// safe from eviction, before the caller has had a chance to mark it busy.
func TestLeasedWorkerIsUnevictableBeforeCallerMarksItBusy(t *testing.T) {
	script, logPath := fakeAppServer(t)
	m := NewManager(script, nil, nil, nil, []string{"FAKE_LOG=" + logPath}, 1, nil)
	defer m.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w, err := m.leaseWorker(ctx, "thread-1", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	busy := w.busy
	m.mu.Unlock()
	if busy {
		t.Fatal("leaseWorker() marked the worker busy; the lease should be the only guard here")
	}
	if _, err := m.reserve(); err == nil {
		t.Fatal("reserve evicted a leased worker that was not yet busy")
	}

	// Releasing the lease without a turn in flight hands it back to the LRU.
	m.releaseWorker(w)
	if _, err := m.reserve(); err != nil {
		t.Fatalf("released idle worker stayed unevictable: %v", err)
	}
}
