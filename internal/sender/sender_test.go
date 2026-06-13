package sender

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func collect(t *testing.T, ch <-chan StreamEvent, timeout time.Duration) []StreamEvent {
	t.Helper()
	var got []StreamEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %s; got %d events so far: %v", timeout, len(got), types(got))
		}
	}
}

// testSender wires a Sender to a fake tmux runner and a temp projects dir,
// with timings shrunk to milliseconds. Returns the sender and the jsonl path
// the session's file should live at.
func testSender(t *testing.T, runner tmuxRunner, id string) (*Sender, string) {
	t.Helper()
	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	tm := timing{
		spawnSettle:   10 * time.Millisecond,
		trustToInject: 5 * time.Millisecond,
		warmSettle:    5 * time.Millisecond,
		resumeReady:   100 * time.Millisecond,
		confirm:       1 * time.Second,
		poll:          10 * time.Millisecond,
	}
	p := newPool(runner, "claude", nil, nil, 8, quietLogger())
	s := &Sender{
		pool:        p,
		backend:     claudeBackend{p: p, t: tm, projectsDir: dir, claudeCmd: "claude"},
		projectsDir: dir,
		logger:      quietLogger(),
		t:           tm,
		tail:        tailConfig{poll: 10 * time.Millisecond, appearWait: 2 * time.Second},
	}
	return s, filepath.Join(sub, id+".jsonl")
}

var turnLines = []string{
	`{"type":"user","message":{"role":"user","content":"hi"}}`,
	`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}}`,
	`{"type":"system","subtype":"turn_duration","durationMs":42}`,
}

func TestSend_ResumeStreamsTurn(t *testing.T) {
	// Pane already at the idle input box: no resume chooser to answer.
	f := &fakeTmux{captureOut: "  " + inputReadyMarker + " · ← for agents\n"}
	s, path := testSender(t, f, "sess-1")
	// Pre-existing history so this is a resume with a non-zero offset.
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := s.Send(context.Background(), "sess-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	go appendLines(path, 30*time.Millisecond, turnLines...)

	got := collect(t, ch, 6*time.Second)
	want := []string{"subprocess.started", "user", "assistant", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
	// The pre-existing "mode" line must not leak (offset respected).
	for _, e := range got {
		if strings.Contains(string(e.Raw), `"mode"`) {
			t.Fatalf("history leaked: %s", e.Raw)
		}
	}
}

func TestSend_NewSessionWaitsForFileAndTrust(t *testing.T) {
	f := &fakeTmux{}
	s, path := testSender(t, f, "new-1")

	ch, err := s.SendNew(context.Background(), "new-1", "hi", "/work", "")
	if err != nil {
		t.Fatal(err)
	}
	// jsonl is created lazily, only after the prompt is submitted.
	go func() {
		time.Sleep(60 * time.Millisecond)
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			panic(err)
		}
		appendLines(path, 20*time.Millisecond, turnLines...)
	}()

	got := collect(t, ch, 6*time.Second)
	if !eq(types(got), []string{"subprocess.started", "user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
	// A fresh window spawns via new-session, runs claude with --session-id,
	// and receives a trust-accept Enter.
	if f.countCmd("new-session") != 1 {
		t.Fatalf("expected one new-session, got %d", f.countCmd("new-session"))
	}
	if !cmdMatches(f, "new-session", "--session-id") {
		t.Fatal("new session should launch claude with --session-id")
	}
	if !cmdMatches(f, "send-keys", "Enter") {
		t.Fatal("fresh window should receive a trust-accept Enter")
	}
}

func TestSend_ResumeAnswersChooserWithFullSession(t *testing.T) {
	// A long resume opens the chooser with the "summary" option highlighted;
	// usher must step the arrow down to "full session as-is", never a bare
	// Enter (which would pick the highlighted summary default).
	f := &fakeTmux{captureOut: "❯ 1. Resume from summary (recommended)\n" +
		"  2. " + resumeChooserMarker + "\n  3. Don't ask me again\n"}
	s, path := testSender(t, f, "resume-1")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := s.Send(context.Background(), "resume-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	// Write the turn only after the prompt is injected, as claude would — so the
	// pre-inject offset is captured before these lines (the chooser wait delays
	// the inject, which would otherwise race the turn past the offset).
	go func() {
		for f.countCmd("paste-buffer") < 1 {
			time.Sleep(2 * time.Millisecond)
		}
		appendLines(path, 10*time.Millisecond, turnLines...)
	}()

	got := collect(t, ch, 6*time.Second)
	if !eq(types(got), []string{"subprocess.started", "user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
	// Down is unique to the chooser path (inject never sends it), so its
	// presence proves the chooser was detected and answered toward full-session.
	if !cmdMatches(f, "send-keys", "Down") {
		t.Fatalf("resume chooser should be answered by stepping to full-session; cmds=%v", f.cmds)
	}
}

func TestChooserArrowOn(t *testing.T) {
	// The chooser's option strings appear verbatim in a session's own
	// transcript; a loose Contains(marker) match fired keystrokes into the
	// prompt box during the boot frames before the input footer rendered. The
	// arrow-row match must require the selection arrow on the SAME line.
	transcript := "  we replaced the blind Down with Resume full session as-is handling\n" +
		"❯ \n" + // the idle prompt's own arrow, on its own line
		"  ? for shortcuts"
	if chooserArrowOn(transcript, resumeChooserMarker) {
		t.Fatal("must not match option text in transcript that lacks the arrow on its line")
	}
	chooser := "❯ 1. Resume from summary (recommended)\n" +
		"  2. Resume full session as-is\n  3. Don't ask me again"
	if !chooserArrowOn(chooser, resumeSummaryMarker) {
		t.Fatal("must match the arrow on the highlighted summary default")
	}
	if chooserArrowOn(chooser, resumeChooserMarker) {
		t.Fatal("must not report full-session selected while the arrow is on summary")
	}
}

func TestSend_InjectsOnceNoRetry(t *testing.T) {
	// The landed-oracle/re-inject loop is gone: exactly one paste per send, even
	// when the user turn lands slowly (here, after a delay).
	f := &fakeTmux{captureOut: "  " + inputReadyMarker + "\n"}
	s, path := testSender(t, f, "once-1")
	if err := os.WriteFile(path, []byte(`{"type":"mode"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ch, err := s.Send(context.Background(), "once-1", "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	go appendLines(path, 120*time.Millisecond, turnLines...)

	got := collect(t, ch, 6*time.Second)
	if !eq(types(got), []string{"subprocess.started", "user", "assistant", "subprocess.exit"}) {
		t.Fatalf("got %v", types(got))
	}
	if n := f.countCmd("paste-buffer"); n != 1 {
		t.Fatalf("expected exactly one inject, got %d", n)
	}
}

func TestSend_SpawnErrorPropagates(t *testing.T) {
	f := &fakeTmux{failSpawn: true}
	s, _ := testSender(t, f, "boom")
	if _, err := s.Send(context.Background(), "boom", "hi", "/work"); err == nil {
		t.Fatal("expected error when window spawn fails")
	}
}

// cmdMatches reports whether some recorded command starting with verb has an
// argument containing sub.
func cmdMatches(f *fakeTmux, verb, sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if len(c) == 0 || c[0] != verb {
			continue
		}
		for _, a := range c {
			if strings.Contains(a, sub) {
				return true
			}
		}
	}
	return false
}
