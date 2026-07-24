package sender

import (
	"context"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/appserver"
	"github.com/nexustar/usher/internal/backend"
)

func TestDrainTailCancelsAndKeepsFinalParts(t *testing.T) {
	old := finalDrainQuiet
	finalDrainQuiet = 30 * time.Millisecond
	t.Cleanup(func() { finalDrainQuiet = old })
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

	drainTail(context.Background(), out, events, cancel, nil, false)

	if tailCtx.Err() == nil {
		t.Fatal("tail was not cancelled for its final EOF drain")
	}
	if first, second := (<-out).Type, (<-out).Type; first != "part" || second != "subprocess.exit" {
		t.Fatalf("events = [%s %s], want [part subprocess.exit]", first, second)
	}
}

// A tail that keeps growing past completion — a concurrent turn from another
// frontend writing into the same jsonl — resets the quiet timer forever. Only
// the hard ceiling stops the drain; without it the goroutine leaks and the
// session is stuck "running".
func TestDrainTailHardCeilingStopsUnboundedGrowth(t *testing.T) {
	oldQuiet, oldCeil := finalDrainQuiet, finalDrainCeiling
	finalDrainQuiet = 30 * time.Millisecond
	finalDrainCeiling = 150 * time.Millisecond
	t.Cleanup(func() { finalDrainQuiet = oldQuiet; finalDrainCeiling = oldCeil })

	events := make(chan StreamEvent)
	out := make(chan StreamEvent, 4096)
	tailCtx, cancel := context.WithCancel(context.Background())

	// Emit faster than the quiet window until the drain cancels the tailer,
	// exactly as a live tailer would while the file keeps growing.
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tailCtx.Done():
				close(events)
				return
			case <-tick.C:
				select {
				case events <- StreamEvent{Type: "part"}:
				case <-tailCtx.Done():
					close(events)
					return
				}
			}
		}
	}()

	done := make(chan struct{})
	go func() { drainTail(context.Background(), out, events, cancel, nil, false); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainTail never returned under continuous growth: hard ceiling missing")
	}
	if tailCtx.Err() == nil {
		t.Fatal("tail was not cancelled")
	}
}

// The terminal marker is the deterministic "content finished" signal: drainTail
// must stop the instant it forwards one, not wait out the quiet window. With a
// long quiet window, only the fast path can return promptly.
func TestDrainTailStopsOnTerminalMarker(t *testing.T) {
	oldQuiet := finalDrainQuiet
	finalDrainQuiet = 5 * time.Second // so a quiet-based stop would blow the deadline
	t.Cleanup(func() { finalDrainQuiet = oldQuiet })

	events := make(chan StreamEvent, 3)
	out := make(chan StreamEvent, 8)
	tailCtx, cancel := context.WithCancel(context.Background())
	marker := []byte(`{"type":"system","subtype":"turn_duration"}`)
	go func() {
		events <- StreamEvent{Type: "assistant", Raw: []byte(`{"type":"assistant"}`)}
		events <- StreamEvent{Type: "system", Raw: marker}
		// The tailer emits its own exit once cancelled for the final EOF read.
		<-tailCtx.Done()
		events <- StreamEvent{Type: backend.EventProcessExit, Raw: []byte(`{}`)}
		close(events)
	}()

	done := make(chan bool, 1)
	go func() { done <- drainTail(context.Background(), out, events, cancel, isTurnComplete, false) }()
	select {
	case exited := <-done:
		if !exited {
			t.Fatal("drainTail reported no exit after the terminal marker")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drainTail waited out the quiet window instead of stopping on the marker")
	}
	if tailCtx.Err() == nil {
		t.Fatal("tail was not cancelled")
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
