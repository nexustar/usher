package jsonl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const srcID = "77de84e0-adf0-4879-9561-1f597d44771b"
const newID = "4d924b55-a868-4225-b935-28c66e68f223"

// forkFixture builds a two-turn session file shaped like real claude jsonl
// (chain lines interleaved with bookkeeping), returning its path and the
// line count. Turn 1 ends at uuid "a1" (turn_duration follows); turn 2 at
// "a2". When openTail is set, turn 2 has no turn_duration (still running).
func forkFixture(t *testing.T, openTail bool) (path string, lines []string) {
	t.Helper()
	l := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	sid := `"sessionId":"` + srcID + `"`
	l(`{"type":"mode","mode":"default",%s}`, sid)
	l(`{"type":"file-history-snapshot","messageId":"m1","snapshot":{}}`) // no sessionId, like the real thing
	l(`{"type":"user",%s,"uuid":"u1","parentUuid":null,"timestamp":"2026-06-12T00:00:01Z","cwd":"/tmp/p","message":{"role":"user","content":"first question"}}`, sid)
	l(`{"type":"ai-title",%s,"aiTitle":"t"}`, sid)
	l(`{"type":"assistant",%s,"uuid":"a1","parentUuid":"u1","timestamp":"2026-06-12T00:00:02Z","message":{"role":"assistant","model":"opus","content":[{"type":"text","text":"first answer mentioning %s"}]}}`, sid, srcID)
	l(`{"type":"system","subtype":"turn_duration",%s,"uuid":"s1","parentUuid":"a1","durationMs":1000}`, sid)
	l(`{"type":"last-prompt",%s,"lastPrompt":"first question","leafUuid":"a1"}`, sid)
	l(`{"type":"user",%s,"uuid":"u2","parentUuid":"s1","timestamp":"2026-06-12T00:00:03Z","message":{"role":"user","content":"second question"}}`, sid)
	l(`{"type":"assistant",%s,"uuid":"a2","parentUuid":"u2","timestamp":"2026-06-12T00:00:04Z","message":{"role":"assistant","model":"opus","content":[{"type":"text","text":"second answer"}]}}`, sid)
	if !openTail {
		l(`{"type":"system","subtype":"turn_duration",%s,"uuid":"s2","parentUuid":"a2","durationMs":1000}`, sid)
	}
	path = filepath.Join(t.TempDir(), srcID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, lines
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestForkCopy_CutsBeforeNextUserPrompt(t *testing.T) {
	src, srcLines := forkFixture(t, false)
	dst := filepath.Join(filepath.Dir(src), newID+".jsonl")

	if err := ForkCopy(src, dst, "a1", newID); err != nil {
		t.Fatal(err)
	}
	got := readLines(t, dst)
	// Keep through turn 1's trailing bookkeeping (last-prompt), cut before u2.
	if len(got) != 7 {
		t.Fatalf("fork should keep 7 lines, got %d:\n%s", len(got), strings.Join(got, "\n"))
	}
	for i, ln := range got {
		if strings.Contains(ln, `"sessionId":"`+srcID+`"`) {
			t.Fatalf("line %d still carries the old sessionId: %s", i, ln)
		}
	}
	// The old id inside message CONTENT must survive (only the field is rewritten).
	if !strings.Contains(got[4], "first answer mentioning "+srcID) {
		t.Fatalf("message content was mangled: %s", got[4])
	}
	// Lines without a sessionId field are byte-identical.
	if got[1] != srcLines[1] {
		t.Fatalf("no-sessionId line changed:\n got %s\nwant %s", got[1], srcLines[1])
	}
	// Source untouched.
	if n := len(readLines(t, src)); n != len(srcLines) {
		t.Fatalf("source file changed: %d lines, want %d", n, len(srcLines))
	}
	// The fork must parse as a session whose transcript is exactly turn 1.
	turns, _, err := ReadTurns(dst, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Content != "first question" || turns[1].Role != "assistant" {
		t.Fatalf("fork transcript should be turn 1 only, got %+v", turns)
	}
	if turns[1].UUID != "a1" {
		t.Fatalf("assistant turn should carry its fork point uuid, got %q", turns[1].UUID)
	}
}

func TestForkCopy_TipForkKeepsWholeFile(t *testing.T) {
	src, srcLines := forkFixture(t, false)
	dst := filepath.Join(filepath.Dir(src), newID+".jsonl")
	if err := ForkCopy(src, dst, "a2", newID); err != nil {
		t.Fatal(err)
	}
	if got := readLines(t, dst); len(got) != len(srcLines) {
		t.Fatalf("tip fork should keep all %d lines, got %d", len(srcLines), len(got))
	}
}

func TestForkCopy_RejectsInFlightTurn(t *testing.T) {
	src, _ := forkFixture(t, true) // turn 2 has no turn_duration yet
	dst := filepath.Join(filepath.Dir(src), newID+".jsonl")
	err := ForkCopy(src, dst, "a2", newID)
	if err == nil || !strings.Contains(err.Error(), "not completed") {
		t.Fatalf("want in-flight turn error, got %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatal("failed fork must not leave a file behind")
	}
}

func TestForkCopy_Errors(t *testing.T) {
	src, _ := forkFixture(t, false)
	dst := filepath.Join(filepath.Dir(src), newID+".jsonl")

	if err := ForkCopy(src, dst, "nope", newID); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want fork-point-not-found, got %v", err)
	}
	if err := ForkCopy(src, dst, "", newID); err == nil {
		t.Fatal("empty fork point must error")
	}
	if err := ForkCopy(src, dst, "a1", newID); err != nil {
		t.Fatal(err)
	}
	if err := ForkCopy(src, dst, "a1", newID); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("existing target must error, got %v", err)
	}
}
