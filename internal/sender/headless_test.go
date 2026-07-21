package sender

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/appserver"
	"github.com/nexustar/usher/internal/backend"
)

func TestDrainTailKeepsFinalPartsAfterProtocolResult(t *testing.T) {
	events := make(chan StreamEvent, 2)
	out := make(chan StreamEvent, 2)
	tailCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Model a protocol result arriving just before the tailer's next poll.
		time.Sleep(20 * time.Millisecond)
		events <- StreamEvent{Type: "part"}
		events <- StreamEvent{Type: "subprocess.exit"}
		close(events)
	}()

	drainTail(context.Background(), out, events, cancel, time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)), "timeout", "session_id", "s1")

	if tailCtx.Err() != nil {
		t.Fatal("tail was cancelled before its completion marker")
	}
	if first, second := (<-out).Type, (<-out).Type; first != "part" || second != "subprocess.exit" {
		t.Fatalf("events = [%s %s], want [part subprocess.exit]", first, second)
	}
}

func TestCodexSlashCommandSyntax(t *testing.T) {
	tests := []struct {
		prompt, command, args string
		ok                    bool
	}{
		{prompt: "/compact", command: "/compact", ok: true},
		{prompt: "/review check auth", command: "/review", args: "check auth", ok: true},
		{prompt: "/review\ncheck auth", command: "/review", args: "check auth", ok: true},
		{prompt: "/my-skill_2 go", command: "/my-skill_2", args: "go", ok: true},
		{prompt: "$imagegen draw", ok: false},
		{prompt: "normal prompt", ok: false},
		// Pasted absolute paths reach Codex as prompts, not as commands that
		// codexPrompt would reject with "unknown command".
		{prompt: "/home/dev/x.go is broken", ok: false},
		{prompt: "/usr/lib", ok: false},
		{prompt: "/etc/hosts", ok: false},
		{prompt: "/", ok: false},
		{prompt: "/ compact", ok: false},
		{prompt: "//not-a-command", ok: false},
		{prompt: " /compact", ok: false},
	}
	for _, tt := range tests {
		command, args, ok := backend.ParseSlashCommand(tt.prompt)
		if command != tt.command || args != tt.args || ok != tt.ok {
			t.Errorf("ParseSlashCommand(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.prompt, command, args, ok, tt.command, tt.args, tt.ok)
		}
	}
}

func TestCodexSlashSkillPrompt(t *testing.T) {
	skills := []appserver.Skill{
		{Name: "imagegen", Enabled: true},
		{Name: "disabled", Enabled: false},
	}
	tests := []struct {
		command, args, want string
		ok                  bool
	}{
		{command: "/imagegen", args: "draw an icon", want: "$imagegen draw an icon", ok: true},
		{command: "/disabled"},
		{command: "/missing"},
		// Codex resolves $mentions case-sensitively; matching loosely here
		// would make /IMAGEGEN work in usher and $IMAGEGEN fail in the TUI.
		{command: "/IMAGEGEN"},
	}
	for _, tt := range tests {
		got, ok := codexSlashSkillPrompt(tt.command, tt.args, skills)
		if got != tt.want || ok != tt.ok {
			t.Errorf("codexSlashSkillPrompt(%q, %q) = (%q, %v), want (%q, %v)", tt.command, tt.args, got, ok, tt.want, tt.ok)
		}
	}
}
