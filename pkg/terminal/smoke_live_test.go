package terminal

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLiveTmuxSmoke exercises the full lifecycle against a real tmux server.
// Opt-in via USHER_TMUX_SMOKE=1; uses a dedicated socket and cleans up after.
func TestLiveTmuxSmoke(t *testing.T) {
	if os.Getenv("USHER_TMUX_SMOKE") != "1" {
		t.Skip("set USHER_TMUX_SMOKE=1 to run against real tmux")
	}
	const socket = "usher-smoke"
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	m := New("tmux", socket, "bash", slog.Default())
	if !m.Available() {
		t.Fatal("tmux not available")
	}
	id := "0af0c1d2-3e4f-5678-9abc-def012345678"
	if err := m.Open(id, "/tmp", 100, 24); err != nil {
		t.Fatal(err)
	}
	if !m.Has(id) {
		t.Fatal("window not live after Open")
	}
	if err := m.Submit(id, "req-1", "echo smoke-$((6*7)) && pwd"); err != nil {
		t.Fatal(err)
	}
	if err := m.Submit(id, "req-1", "echo dup-should-not-run"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	out, err := m.Capture(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("captured:\n%s", strings.TrimRight(out, "\n "))
	if !strings.Contains(out, "smoke-42") || !strings.Contains(out, "/tmp") {
		t.Fatal("expected command output missing from capture")
	}
	if strings.Contains(out, "dup-should-not-run") {
		t.Fatal("duplicate request_id was not deduped")
	}
	if err := m.Resize(id, 90, 20); err != nil {
		t.Fatal(err)
	}
	if err := m.SendControl(id, "C-c"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(id); err != nil {
		t.Fatal(err)
	}
	if m.Has(id) {
		t.Fatal("window still live after Close")
	}
	if _, err := m.Capture(id); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Capture after Close = %v, want ErrNotOpen", err)
	}
}
