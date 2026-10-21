package sender

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/core"
	"github.com/nexustar/usher/pkg/interaction"
)

// fakeClaude is a stdin-driven stub that completes whatever turn it is fed, so
// Start reaches the point where it hands the prompt to a live worker.
func fakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
[ -n "$FAKE_CLAUDE_ARGS" ] && printf '%s\n' "$@" > "$FAKE_CLAUDE_ARGS"
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

type approvalStore map[string]string

func (m approvalStore) Approval(id string) string { return m[id] }

func (m approvalStore) SetApproval(id, approval string) {
	if approval == "" {
		delete(m, id)
	} else {
		m[id] = approval
	}
}

// Start records the approval itself: the caller only learns the id after the
// first turn is already underway. The spawn carries Claude's name for it.
func TestStartRecordsApproval(t *testing.T) {
	h := interaction.New()
	s := New(fakeClaude(t), t.TempDir(), "", 1, false, h, nil)
	s.tail.appearWait = 50 * time.Millisecond
	t.Cleanup(s.Shutdown)
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_CLAUDE_ARGS", argsFile)

	id, ch, err := s.Start(context.Background(), backend.StartRequest{
		Cwd: t.TempDir(), Prompt: "hello", Approval: core.ApprovalAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if got := h.Approval(id); got != core.ApprovalAuto {
		t.Fatalf("session %s approval = %q, want auto", id, got)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--permission-mode\nauto\n") {
		t.Fatalf("spawn args = %q", args)
	}
}

// A session that never started must not leave its approval behind, where nothing
// would ever clear it: Forget only runs for ids that exist.
func TestStartForgetsApprovalWhenSessionFailsToStart(t *testing.T) {
	h := interaction.New()
	store := approvalStore{}
	h.SetApprovalStore(store)
	missing := filepath.Join(t.TempDir(), "no-such-claude")
	s := New(missing, t.TempDir(), "", 1, false, h, nil)
	t.Cleanup(s.Shutdown)

	if _, _, err := s.Start(context.Background(), backend.StartRequest{
		Cwd: t.TempDir(), Prompt: "hello", Approval: core.ApprovalFull,
	}); err == nil {
		t.Fatal("Start succeeded with a missing claude binary")
	}
	if len(store) != 0 {
		t.Errorf("failed start left approvals %v", store)
	}
}
