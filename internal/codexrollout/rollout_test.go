package codexrollout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/core"
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
	if meta.Title != "" {
		t.Errorf("Title = %q, want empty (codex has no ai-title)", meta.Title)
	}
	if !strings.HasPrefix(meta.Prompt, "Read the file sample.txt") {
		t.Errorf("Prompt = %q, want it to start with the user prompt", meta.Prompt)
	}
	if meta.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if meta.LastEventAt.Before(meta.StartedAt) {
		t.Errorf("LastEventAt %v before StartedAt %v", meta.LastEventAt, meta.StartedAt)
	}
}

func TestRenameSessionUsesCodexIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_index.jsonl")
	if err := RenameSession(path, "thread-1", "First"); err != nil {
		t.Fatal(err)
	}
	if err := RenameSession(path, "thread-2", "Other"); err != nil {
		t.Fatal(err)
	}
	if err := RenameSession(path, "thread-1", "Latest"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadThreadName(path, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Latest" {
		t.Fatalf("name = %q, want Latest", got)
	}
	names, err := ReadThreadNames(path)
	if err != nil {
		t.Fatal(err)
	}
	if names["thread-1"] != "Latest" || names["thread-2"] != "Other" {
		t.Fatalf("names = %#v", names)
	}
}

func TestReadSessionMetaSubagent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-07-13T00-00-00-019f5723-e850-7c71-b2e5-7735abe145fb.jsonl")
	raw := `{"timestamp":"2026-07-13T00:00:00Z","type":"session_meta","payload":{"id":"019f5723-e850-7c71-b2e5-7735abe145fb","cwd":"/tmp/project","thread_source":"subagent","parent_thread_id":"019f5228-434c-76d1-8459-4279b2d87ba3","agent_nickname":"Archimedes","agent_path":"/root/review"}}`
	if err := os.WriteFile(path, []byte(raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.IsSubagent || meta.ParentID != "019f5228-434c-76d1-8459-4279b2d87ba3" || meta.AgentName != "Archimedes" {
		t.Fatalf("subagent meta = %+v", meta)
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

func TestAssemblerTurnEndTime(t *testing.T) {
	asm := NewAssembler()
	var done []core.Turn
	for _, ln := range []string{
		`{"timestamp":"2026-06-15T00:00:01Z","type":"event_msg","payload":{"type":"agent_message","message":"working"}}`,
		`{"timestamp":"2026-06-15T00:00:05Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"command\":\"ls\"}"}}`,
		`{"timestamp":"2026-06-15T00:00:09Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}`,
		`{"timestamp":"2026-06-15T00:00:12Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
	} {
		completed, _ := asm.Feed([]byte(ln))
		done = append(done, completed...)
	}
	if len(done) != 1 {
		t.Fatalf("got %d completed turns, want 1", len(done))
	}
	if want := time.Date(2026, 6, 15, 0, 0, 1, 0, time.UTC); !done[0].Time.Equal(want) {
		t.Errorf("Time = %v, want %v (turn start)", done[0].Time, want)
	}
	if want := time.Date(2026, 6, 15, 0, 0, 12, 0, time.UTC); !done[0].EndTime.Equal(want) {
		t.Errorf("EndTime = %v, want %v (task_complete)", done[0].EndTime, want)
	}
}

// The assembler must honor the announced v2 rename of the end-of-turn event
// like the tailer's IsTurnComplete does: turn_complete stamps UUID (the fork
// anchor) and EndTime and flushes the turn — not just task_complete.
func TestAssemblerTurnCompleteV2Rename(t *testing.T) {
	asm := NewAssembler()
	var done []core.Turn
	for _, ln := range []string{
		`{"timestamp":"2026-06-15T00:00:01Z","type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`,
		`{"timestamp":"2026-06-15T00:00:05Z","type":"event_msg","payload":{"type":"turn_complete","turn_id":"t9"}}`,
	} {
		completed, _ := asm.Feed([]byte(ln))
		done = append(done, completed...)
	}
	if len(done) != 1 {
		t.Fatalf("got %d completed turns, want 1", len(done))
	}
	if done[0].UUID != "t9" {
		t.Errorf("UUID = %q, want t9 (fork anchor from turn_complete)", done[0].UUID)
	}
	if want := time.Date(2026, 6, 15, 0, 0, 5, 0, time.UTC); !done[0].EndTime.Equal(want) {
		t.Errorf("EndTime = %v, want %v", done[0].EndTime, want)
	}
}

func TestIsTaskCompleteV2Rename(t *testing.T) {
	v2 := `{"type":"event_msg","payload":{"type":"turn_complete","turn_id":"t1"}}`
	if !isTaskComplete([]byte(v2), "t1") {
		t.Error("turn_complete with matching turn_id not recognized as fork end")
	}
	if isTaskComplete([]byte(v2), "other") {
		t.Error("turn_id mismatch must not match")
	}
}

func TestIsTurnComplete(t *testing.T) {
	complete := `{"timestamp":"2026-06-14T00:00:00Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"x"}}`
	if !IsTurnComplete([]byte(complete)) {
		t.Error("task_complete not detected as turn-complete")
	}
	v2 := `{"type":"event_msg","payload":{"type":"turn_complete","turn_id":"x"}}`
	if !IsTurnComplete([]byte(v2)) {
		t.Error("turn_complete (announced v2 rename) not detected as turn-complete")
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

func TestIsTurnActivity(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		// Submit-time records: written whether or not the model ever runs.
		{`{"type":"turn_context","payload":{"model":"gpt-5"}}`, false},
		{`{"type":"event_msg","payload":{"type":"user_message","message":"hi"}}`, false},
		{`{"type":"response_item","payload":{"type":"message","role":"user"}}`, false},
		// task_complete is completion, not activity — turnComplete's job.
		{`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"x"}}`, false},
		{`not json`, false},
		// Model output: proof a real turn is in flight.
		{`{"type":"event_msg","payload":{"type":"task_started"}}`, true},
		{`{"type":"event_msg","payload":{"type":"turn_started"}}`, true}, // announced v2 rename
		{`{"type":"event_msg","payload":{"type":"agent_message","message":"hello"}}`, true},
		{`{"type":"event_msg","payload":{"type":"agent_reasoning","text":"…"}}`, true},
		{`{"type":"response_item","payload":{"type":"message","role":"assistant"}}`, true},
		{`{"type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1"}}`, true},
		{`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1"}}`, true},
		{`{"type":"response_item","payload":{"type":"reasoning"}}`, true},
	} {
		if got := IsTurnActivity([]byte(tc.line)); got != tc.want {
			t.Errorf("IsTurnActivity(%s) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestTurnAbortedAndMalformedTimestampCompletion(t *testing.T) {
	for _, raw := range []string{
		`{"type":"event_msg","payload":{"type":"turn_aborted"}}`,
		`{"type":"event_msg","payload":{"type":"task_aborted"}}`,
	} {
		if !IsTurnAborted([]byte(raw)) {
			t.Errorf("IsTurnAborted(%s) = false", raw)
		}
	}
	// Errors may be retried and followed by a normal completion: not an abort.
	for _, raw := range []string{
		`{"type":"error","payload":{"message":"rate limited"}}`,
		`{"type":"event_msg","payload":{"type":"error"}}`,
	} {
		if IsTurnAborted([]byte(raw)) {
			t.Errorf("IsTurnAborted(%s) = true", raw)
		}
	}

	a := NewAssembler()
	a.Feed([]byte(`{"timestamp":"2026-01-01T00:00:00Z","type":"event_msg","payload":{"type":"agent_message","message":"done"}}`))
	done, _ := a.Feed([]byte(`{"timestamp":"v2-not-rfc3339","type":"event_msg","payload":{"type":"turn_complete","turn_id":"t-v2"}}`))
	if len(done) != 1 || done[0].UUID != "t-v2" {
		t.Fatalf("malformed timestamp completion = %+v, want one turn with UUID t-v2", done)
	}
}

func TestAssemblerAppServerMCPToolCall(t *testing.T) {
	a := NewAssembler()
	raw := `{"timestamp":"2026-07-11T17:39:19Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"mcp-1","invocation":{"server":"usher","tool":"show_image","arguments":{"file_path":"/tmp/grassland.png"}},"result":{"Ok":{"content":[{"type":"text","text":"{\"w\":1536,\"h\":1024}"}],"isError":false}}}}`
	_, part := a.Feed([]byte(raw))
	if part == nil {
		t.Fatal("mcp_tool_call_end did not produce a turn part")
	}
	if part.Type != "tool" || part.ToolName != "mcp__usher__show_image" || part.ToolTarget != "/tmp/grassland.png" {
		t.Fatalf("unexpected part: %+v", *part)
	}
	if !strings.Contains(part.Content, `"w":1536`) {
		t.Fatalf("missing MCP result dimensions: %q", part.Content)
	}
}

func TestAssemblerProjectsContextCompacted(t *testing.T) {
	a := NewAssembler()
	a.Feed([]byte(`{"timestamp":"2026-07-14T14:51:25Z","type":"event_msg","payload":{"type":"agent_message","message":"before"}}`))
	completed, _ := a.Feed([]byte(`{"timestamp":"2026-07-14T14:51:26Z","type":"event_msg","payload":{"type":"context_compacted"}}`))
	if len(completed) != 2 || completed[0].Role != "assistant" || completed[1].Role != "system" {
		t.Fatalf("context compacted completed = %+v", completed)
	}
	if completed[1].Content != "Context compacted" {
		t.Fatalf("system content = %q", completed[1].Content)
	}
}

func TestAssemblerCanonicalMCPItemAndLegacyDedup(t *testing.T) {
	a := NewAssembler()
	canonical := `{"timestamp":"2026-07-11T17:39:19Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"mcp_tool_call","id":"mcp-1","server":"usher","tool":"show_image","arguments":{"file_path":"/tmp/a.png"},"status":"completed","result":{"content":[{"type":"text","text":"{\"w\":10,\"h\":20}"}],"isError":false}}}}`
	_, part := a.Feed([]byte(canonical))
	if part == nil || part.ToolName != "mcp__usher__show_image" || part.ToolTarget != "/tmp/a.png" {
		t.Fatalf("canonical part: %+v", part)
	}
	legacy := `{"timestamp":"2026-07-11T17:39:20Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"mcp-1","invocation":{"server":"usher","tool":"show_image","arguments":{"file_path":"/tmp/a.png"}},"result":{"Ok":{"content":[{"type":"text","text":"duplicate"}]}}}}`
	if _, duplicate := a.Feed([]byte(legacy)); duplicate != nil {
		t.Fatalf("legacy duplicate emitted: %+v", duplicate)
	}
	turn := a.Flush()
	if turn == nil || len(turn.Parts) != 1 {
		t.Fatalf("deduped turn: %+v", turn)
	}
}

func TestAssemblerNamespacedMCPResponseItemDedup(t *testing.T) {
	call := `{"timestamp":"2026-07-22T16:53:40.966Z","type":"response_item","payload":{"type":"function_call","name":"show_image","namespace":"mcp__usher","arguments":"{\"file_path\":\"/tmp/a.png\"}","call_id":"mcp-1"}}`
	legacy := `{"timestamp":"2026-07-22T16:53:40.983Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"mcp-1","invocation":{"server":"usher","tool":"show_image","arguments":{"file_path":"/tmp/a.png"}},"result":{"Ok":{"content":[{"type":"text","text":"{\"w\":10,\"h\":20}"}],"isError":false}}}}`
	output := `{"timestamp":"2026-07-22T16:53:41.025Z","type":"response_item","payload":{"type":"function_call_output","call_id":"mcp-1","output":"[{\"type\":\"text\",\"text\":\"{\\\"w\\\":10,\\\"h\\\":20}\"}]"}}`

	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"legacy completion first", []string{call, legacy, output}},
		{"response output first", []string{call, output, legacy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAssembler()
			var parts []*core.TurnPart
			for _, line := range tc.lines {
				_, part := a.Feed([]byte(line))
				if part != nil {
					parts = append(parts, part)
				}
			}
			if len(parts) != 1 {
				t.Fatalf("emitted %d parts, want 1: %+v", len(parts), parts)
			}
			if parts[0].ToolName != "mcp__usher__show_image" || parts[0].ToolTarget != "/tmp/a.png" {
				t.Fatalf("unexpected part: %+v", parts[0])
			}
			turn := a.Flush()
			if turn == nil || len(turn.Parts) != 1 {
				t.Fatalf("deduped turn: %+v", turn)
			}
		})
	}
}

func TestAssemblerCurrentCodexToolEvents(t *testing.T) {
	a := NewAssembler()
	lines := []string{
		`{"timestamp":"2026-07-12T00:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"const r = await tools.exec_command({cmd:\"git status --short\"}); text(r.output)"}}`,
		`{"timestamp":"2026-07-12T00:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","name":"exec","output":[{"type":"input_text","text":" M file.go"}]}}`,
		`{"timestamp":"2026-07-12T00:00:02Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"p1","success":true,"status":"completed","changes":{"internal/a.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-old\n+new"}}}}`,
		`{"timestamp":"2026-07-12T00:00:03Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"e1","command":["go","test","./..."],"aggregated_output":"ok"}}`,
		`{"timestamp":"2026-07-12T00:00:04Z","type":"event_msg","payload":{"type":"web_search_end","call_id":"w1","query":"Codex protocol","action":{"type":"search"}}}`,
		`{"timestamp":"2026-07-12T00:00:05Z","type":"event_msg","payload":{"type":"image_generation_end","call_id":"i1","status":"completed","result":"very-large-base64-must-not-render","saved_path":"/tmp/generated.png"}}`,
	}
	for _, line := range lines {
		a.Feed([]byte(line))
	}
	turn := a.Flush()
	if turn == nil || len(turn.Parts) != 5 {
		t.Fatalf("parts = %+v, want 5", turn)
	}
	checks := []struct{ name, target, content string }{
		{"Shell", "git status --short", "M file.go"},
		{"Edit", "internal/a.go", "+new"},
		{"Shell", "go test ./...", "ok"},
		{"WebSearch", "Codex protocol", "search"},
		{"ImageGeneration", "/tmp/generated.png", "completed"},
	}
	for i, want := range checks {
		got := turn.Parts[i]
		if got.ToolName != want.name || got.ToolTarget != want.target || !strings.Contains(got.Content, want.content) {
			t.Errorf("part %d = %+v, want name=%q target=%q content~%q", i, got, want.name, want.target, want.content)
		}
	}
	if strings.Contains(turn.Parts[4].Content, "very-large-base64") {
		t.Error("image base64 leaked into transcript")
	}
}

func TestAssemblerCustomWrapperDeduplicatesCanonicalPatch(t *testing.T) {
	a := NewAssembler()
	a.Feed([]byte(`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"outer","name":"exec","input":"text(await tools.apply_patch(\"*** Begin Patch\"))"}}`))
	a.Feed([]byte(`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"outer","output":"done"}}`))
	_, part := a.Feed([]byte(`{"type":"event_msg","payload":{"type":"patch_apply_end","call_id":"patch","success":true,"changes":{"a.go":{"unified_diff":"+x"}}}}`))
	turn := a.Flush()
	if part == nil || turn == nil || len(turn.Parts) != 1 || turn.Parts[0].ToolName != "Edit" {
		t.Fatalf("canonical patch dedup failed: part=%+v turn=%+v", part, turn)
	}
}

func TestReadSessionMetaUsesLatestCodexUsageSnapshot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := strings.Join([]string{
		`{"type":"turn_context","payload":{"model":"gpt-5.5","effort":"high"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":3,"output_tokens":2,"reasoning_output_tokens":1,"total_tokens":12}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":20,"cached_input_tokens":8,"output_tokens":5,"reasoning_output_tokens":2,"total_tokens":25},"last_token_usage":{"total_tokens":14},"model_context_window":100}}}`,
	}, "\n")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := ReadSessionMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	u := meta.Runtime
	if u.ContextTokens != 14 || u.ContextWindow != 100 || u.Model != "gpt-5.5" || u.Effort != "high" {
		t.Fatalf("usage = %+v", u)
	}
}
