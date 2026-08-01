package sender

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/interaction"
)

// fakeClaude is a stdin-driven stub that completes whatever turn it is fed, so
// Start reaches the point where it hands the prompt to a live worker.
func fakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"subtype":"initialize"'*)
      request_id=$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"commands":[]}}}\n' "$request_id"
      ;;
    *control_request*) ;;
    *)
      uuid=$(printf '%s\n' "$line" | sed -n 's/.*"uuid":"\([^"]*\)".*/\1/p')
      printf '{"type":"command_lifecycle","command_uuid":"%s","state":"completed"}\n' "$uuid"
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// Start records the decision itself: the caller only learns the id after the
// first turn is already underway.
func TestStartRecordsAutoApprove(t *testing.T) {
	h := interaction.New("")
	s := New(fakeClaude(t), "", t.TempDir(), "", 1, false, h, nil)
	// The stub writes no jsonl, so don't sit through the real confirm wait.
	s.t.confirm = 50 * time.Millisecond
	s.tail.appearWait = 50 * time.Millisecond
	t.Cleanup(s.Shutdown)

	id, ch, err := s.Start(context.Background(), backend.StartRequest{
		Cwd: t.TempDir(), Prompt: "hello", AutoApprove: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !h.IsAutoApprove(id) {
		t.Fatalf("session %s did not get blanket-allow", id)
	}
	for range ch {
	}
}

// A session that never started must not leave blanket-allow in the persisted
// file, where nothing would ever clear it: Forget only runs for ids that exist.
func TestStartClearsAutoApproveWhenSessionFailsToStart(t *testing.T) {
	h := interaction.New("")
	missing := filepath.Join(t.TempDir(), "no-such-claude")
	s := New(missing, "", t.TempDir(), "", 1, false, h, nil)
	t.Cleanup(s.Shutdown)

	id, _, err := s.Start(context.Background(), backend.StartRequest{
		Cwd: t.TempDir(), Prompt: "hello", AutoApprove: true,
	})
	if err == nil {
		t.Fatal("Start succeeded with a missing claude binary")
	}
	if h.IsAutoApprove(id) {
		t.Errorf("failed start left blanket-allow on %s", id)
	}
}

// Not asking for auto-approve must leave the session alone rather than writing
// an explicit false, which would grow the persisted file for every session.
func TestStartWithoutAutoApproveRecordsNothing(t *testing.T) {
	h := interaction.New("")
	s := New(fakeClaude(t), "", t.TempDir(), "", 1, false, h, nil)
	// The stub writes no jsonl, so don't sit through the real confirm wait.
	s.t.confirm = 50 * time.Millisecond
	s.tail.appearWait = 50 * time.Millisecond
	t.Cleanup(s.Shutdown)

	id, ch, err := s.Start(context.Background(), backend.StartRequest{
		Cwd: t.TempDir(), Prompt: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.IsAutoApprove(id) {
		t.Errorf("session %s got blanket-allow without asking", id)
	}
	for range ch {
	}
}
