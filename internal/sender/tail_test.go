package sender

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/codexrollout"
)

func types(evs []StreamEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func collect(t *testing.T, ch <-chan StreamEvent, timeout time.Duration) []StreamEvent {
	t.Helper()
	var out []StreamEvent
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
			t.Fatal("timed out waiting for stream")
		}
	}
}

// appendLines appends each line (a '\n' is added) to path with a small delay
// between them, simulating claude writing the turn out over time. Runs in a
// goroutine, so it panics rather than calling t.Fatal on error.
func appendLines(path string, gap time.Duration, lines ...string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	for _, ln := range lines {
		time.Sleep(gap)
		if _, err := f.WriteString(ln + "\n"); err != nil {
			panic(err)
		}
	}
}

func fastCfg() tailConfig {
	return tailConfig{poll: 10 * time.Millisecond, appearWait: 2 * time.Second}
}

func TestTailTurn_SimpleTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	off, _ := os.Stat(path)

	go appendLines(path, 20*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"system","subtype":"turn_duration","durationMs":42}`,
	)

	ch := tailTurn(context.Background(), path, off.Size(), nil, fastCfg())
	got := collect(t, ch, 5*time.Second)

	want := []string{"user", "assistant", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
}

func TestTailTurnSkipsQueuedSpontaneousCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go appendLines(path, 10*time.Millisecond,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"scheduled"}]}}`,
		`{"type":"system","subtype":"turn_duration"}`,
		`{"type":"user","message":{"role":"user","content":"queued"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}`,
		`{"type":"system","subtype":"turn_duration"}`)
	cfg := fastCfg()
	cfg.skipCompletions = 1
	got := collect(t, tailTurn(context.Background(), path, 0, nil, cfg), 3*time.Second)
	if want := []string{"assistant", "system", "user", "assistant", "subprocess.exit"}; !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
}

func TestTailTurn_EndTurnStopReasonDoesNotEndEarly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Interactive claude stamps stop_reason "end_turn" on the intermediate
	// thinking/tool_use messages too. The turn must NOT end there — only at the
	// system/turn_duration marker after the tool ran and the final answer.
	go appendLines(path, 15*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"make a file"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"tool_use","name":"Write"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"system","subtype":"turn_duration","durationMs":42}`,
	)

	ch := tailTurn(context.Background(), path, 0, nil, fastCfg())
	got := collect(t, ch, 5*time.Second)

	// All four turn events flow before the exit — the early end_turn messages
	// did not cut it short.
	want := []string{"user", "assistant", "user", "assistant", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
}

func TestTailTurn_ByteOffsetSkipsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	old := `{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"OLD"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	off, _ := os.Stat(path)

	go appendLines(path, 15*time.Millisecond,
		`{"type":"user","message":{"role":"user","content":"new"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"NEW"}]}}`,
		`{"type":"system","subtype":"turn_duration","durationMs":42}`,
	)

	ch := tailTurn(context.Background(), path, off.Size(), nil, fastCfg())
	got := collect(t, ch, 5*time.Second)

	for _, e := range got {
		if strings.Contains(string(e.Raw), "OLD") {
			t.Fatalf("tailer leaked pre-offset history: %s", e.Raw)
		}
	}
	if !eq(types(got), []string{"user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
}

func TestTailTurn_FileNeverAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.jsonl")
	ch := tailTurn(context.Background(), path, 0, nil, tailConfig{poll: 10 * time.Millisecond, appearWait: 80 * time.Millisecond})
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"error"}) {
		t.Fatalf("got %v, want [error]", types(got))
	}
}

func TestTailTurn_CodexAbortTerminatesAfterActivityLatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := tailConfig{poll: 10 * time.Millisecond, appearWait: 2 * time.Second,
		turnComplete: codexrollout.IsTurnComplete, turnAborted: codexrollout.IsTurnAborted}
	go appendLines(path, 5*time.Millisecond,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"turn_aborted"}}`,
	)
	got := collect(t, tailTurn(context.Background(), path, 0, nil, cfg), 2*time.Second)
	if !eq(types(got), []string{"event_msg", "event_msg", "error", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
}

func TestTailTurn_SeekErrorEmitsErrorAndExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := collect(t, tailTurn(context.Background(), path, -1, nil, fastCfg()), 2*time.Second)
	if !eq(types(got), []string{"error", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
}

func TestTailTurn_CancelBeforeFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	// Long appearWait so the only way out is the cancel, not the timeout.
	ch := tailTurn(ctx, path, 0, nil, tailConfig{poll: 10 * time.Millisecond, appearWait: 10 * time.Second})
	cancel()
	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"subprocess.exit"}) {
		t.Fatalf("got %v, want [subprocess.exit]", types(got))
	}
}

func TestTailTurn_CancelDrainsPersistedFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := tailTurn(ctx, path, 0, nil, tailConfig{poll: time.Hour})

	appendLines(path, 0, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"final"}]}}`)
	cancel()

	got := collect(t, ch, 2*time.Second)
	if !eq(types(got), []string{"assistant", "subprocess.exit"}) {
		t.Fatalf("got %v, want persisted final line before exit", types(got))
	}
}

func TestTailTurn_ContentOnlyIgnoresLogCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := fastCfg()
	cfg.contentOnly = true
	ch := tailTurn(ctx, path, 0, nil, cfg)
	appendLines(path, 0, `{"type":"system","subtype":"turn_duration"}`)

	select {
	case ev := <-ch:
		if ev.Type != "system" {
			t.Fatalf("event = %s, want transcript line", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("completion line was not streamed as content")
	}
	select {
	case ev := <-ch:
		t.Fatalf("log marker ended content-only tail: %s", ev.Type)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	if ev := <-ch; ev.Type != "subprocess.exit" {
		t.Fatalf("cancel event = %s, want subprocess.exit", ev.Type)
	}
}

func TestIsClaudeTurnAborted(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"interrupt marker", `{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`, true},
		{"tool-use variant", `{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}`, true},
		{"normal user prompt", `{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}`, false},
		// A prompt merely quoting the phrase mid-text is not the marker.
		{"phrase quoted mid-text", `{"type":"user","message":{"content":[{"type":"text","text":"why did you log [Request interrupted by user] here?"}]}}`, false},
		// A tool_result block that happens to contain the phrase is not the marker.
		{"tool result containing phrase", `{"type":"user","message":{"content":[{"type":"tool_result","content":"[Request interrupted by user]"}]}}`, false},
		{"assistant line", `{"type":"assistant","message":{"content":[]}}`, false},
		{"turn_duration is not an abort", `{"type":"system","subtype":"turn_duration"}`, false},
	}
	for _, c := range cases {
		if got := isClaudeTurnAborted([]byte(c.line)); got != c.want {
			t.Errorf("%s: isClaudeTurnAborted = %v, want %v", c.name, got, c.want)
		}
	}
}
