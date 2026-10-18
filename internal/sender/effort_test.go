package sender

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/interaction"
)

// A create-time effort is a spawn flag, remembered for later respawns.
func TestStartSpawnsClaudeWithEffort(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_CLAUDE_ARGS", argsPath)
	s := New(fakeClaude(t), "", t.TempDir(), "", 1, false, interaction.New(""), nil)
	s.tail.appearWait = 50 * time.Millisecond
	t.Cleanup(s.Shutdown)

	_, ch, err := s.Start(context.Background(), backend.StartRequest{Cwd: t.TempDir(), Prompt: "hello", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	args, _ := os.ReadFile(argsPath)
	if !strings.Contains(string(args), "--effort\nhigh\n") {
		t.Fatalf("spawn args = %q", args)
	}
}
