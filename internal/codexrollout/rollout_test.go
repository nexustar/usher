package codexrollout

import (
	"strings"
	"testing"
)

const toolFixture = "testdata/rollout-tool.jsonl"
const helloFixture = "testdata/rollout-hello.jsonl"

func TestSessionIDFromPath(t *testing.T) {
	got := SessionIDFromPath("rollout-2026-06-14T00-32-29-019ec19c-f453-7153-a442-3d7239446e01.jsonl")
	want := "019ec19c-f453-7153-a442-3d7239446e01"
	if got != want {
		t.Fatalf("SessionIDFromPath = %q, want %q", got, want)
	}
	if got := SessionIDFromPath("not-a-rollout.jsonl"); got != "" {
		t.Errorf("expected empty id for nameless file, got %q", got)
	}
}

func TestReadSessionMeta(t *testing.T) {
	meta, err := ReadSessionMeta(toolFixture)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "019ec19c-f453-7153-a442-3d7239446e01" {
		t.Errorf("ID = %q", meta.ID)
	}
	if meta.Cwd != "/tmp/codex-probe" {
		t.Errorf("Cwd = %q", meta.Cwd)
	}
	if !strings.HasPrefix(meta.Title, "Read the file sample.txt") {
		t.Errorf("Title = %q, want it to start with the user prompt", meta.Title)
	}
	if meta.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if meta.LastEventAt.Before(meta.StartedAt) {
		t.Errorf("LastEventAt %v before StartedAt %v", meta.LastEventAt, meta.StartedAt)
	}
}

func TestReadTurns_ToolSession(t *testing.T) {
	turns, total, err := ReadTurns(toolFixture, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(turns) != 2 {
		t.Fatalf("got %d turns (total %d), want 2; turns=%+v", len(turns), total, turns)
	}

	// Turn 1: the real user prompt, sourced from event_msg user_message (clean,
	// no <environment_context> noise from the response_item stream).
	u := turns[0]
	if u.Role != "user" {
		t.Fatalf("turn 0 role = %q, want user", u.Role)
	}
	if !strings.Contains(u.Content, "how many lines") {
		t.Errorf("user content = %q", u.Content)
	}
	if strings.Contains(u.Content, "environment_context") {
		t.Errorf("injected context leaked into user turn: %q", u.Content)
	}

	// Turn 2: assistant text, then a tool call+output folded into one part by
	// call_id, then the final assistant text — in chronological order.
	a := turns[1]
	if a.Role != "assistant" {
		t.Fatalf("turn 1 role = %q, want assistant", a.Role)
	}
	if len(a.Parts) != 3 {
		t.Fatalf("assistant parts = %d, want 3; parts=%+v", len(a.Parts), a.Parts)
	}
	if a.Parts[0].Type != "text" || !strings.Contains(a.Parts[0].Content, "read") {
		t.Errorf("part 0 = %+v, want text mentioning read", a.Parts[0])
	}
	tool := a.Parts[1]
	if tool.Type != "tool" {
		t.Errorf("part 1 type = %q, want tool", tool.Type)
	}
	if tool.ToolName != "Shell" {
		t.Errorf("tool name = %q, want Shell", tool.ToolName)
	}
	if tool.ToolTarget != "wc -l sample.txt" {
		t.Errorf("tool target = %q, want 'wc -l sample.txt'", tool.ToolTarget)
	}
	if !strings.Contains(tool.Content, "3 sample.txt") {
		t.Errorf("tool output = %q, want it to contain the wc result", tool.Content)
	}
	if a.Parts[2].Type != "text" || !strings.Contains(a.Parts[2].Content, "3 lines") {
		t.Errorf("part 2 = %+v, want final text with '3 lines'", a.Parts[2])
	}
}

func TestReadTurns_HelloSession(t *testing.T) {
	// A no-tool session must still parse into at least one user + assistant turn
	// without panicking on the absence of function_call lines.
	turns, total, err := ReadTurns(helloFixture, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("no turns parsed from hello session")
	}
	var sawAssistantText bool
	for _, tn := range turns {
		for _, p := range tn.Parts {
			if p.Type == "text" {
				sawAssistantText = true
			}
		}
	}
	if !sawAssistantText {
		t.Error("expected at least one assistant text part")
	}
}

func TestAssemblerModelFromTurnContext(t *testing.T) {
	asm := NewAssembler()
	for _, ln := range []string{
		`{"timestamp":"2026-06-15T00:00:00Z","type":"turn_context","payload":{"turn_id":"t1","model":"gpt-5.5"}}`,
		`{"timestamp":"2026-06-15T00:00:01Z","type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`,
		`{"timestamp":"2026-06-15T00:00:02Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
	} {
		asm.Feed([]byte(ln))
	}
	if got := asm.Model(); got != "gpt-5.5" {
		t.Errorf("Model() = %q, want gpt-5.5 (from turn_context)", got)
	}
	turns, _, err := ReadTurns(helloFixture, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's assistant turns should carry the model stamped from its own
	// turn_context lines (proving the file path, not just the synthetic feed).
	var stamped bool
	for _, tn := range turns {
		if tn.Role == "assistant" && tn.Model != "" {
			stamped = true
		}
	}
	if !stamped {
		t.Error("no assistant turn carried a model from the rollout's turn_context")
	}
}

func TestIsTurnComplete(t *testing.T) {
	complete := `{"timestamp":"2026-06-14T00:00:00Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"x"}}`
	if !IsTurnComplete([]byte(complete)) {
		t.Error("task_complete not detected as turn-complete")
	}
	for _, neg := range []string{
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`,
		`{"type":"response_item","payload":{"type":"message"}}`,
		`not json`,
	} {
		if IsTurnComplete([]byte(neg)) {
			t.Errorf("false positive on %q", neg)
		}
	}
}
