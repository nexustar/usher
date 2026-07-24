package jsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLine_User(t *testing.T) {
	line := []byte(`{"type":"user","sessionId":"s1","cwd":"/tmp/x","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"user","content":"hi"},"uuid":"u1"}`)
	ev, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "user" {
		t.Errorf("Type = %q, want user", ev.Type)
	}
	if ev.SessionID != "s1" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
	if ev.Cwd != "/tmp/x" {
		t.Errorf("Cwd = %q", ev.Cwd)
	}
	if ev.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
	if len(ev.Raw) == 0 {
		t.Error("Raw should not be empty")
	}
}

func TestParseLine_Malformed(t *testing.T) {
	if _, err := ParseLine([]byte(`not json`)); err == nil {
		t.Error("expected error")
	}
}

func TestExtractUserContent_StringContent(t *testing.T) {
	got := extractUserContent([]byte(`{"role":"user","content":"hello world"}`))
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestExtractUserContent_BlockArray(t *testing.T) {
	got := extractUserContent([]byte(`{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}`))
	if got != "first" {
		t.Errorf("got %q", got)
	}
}

func TestExtractUserContent_NoText(t *testing.T) {
	got := extractUserContent([]byte(`{"role":"user","content":[{"type":"image","source":{}}]}`))
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestReadSessionMeta(t *testing.T) {
	meta, err := ReadSessionMeta("testdata/sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "sample" {
		t.Errorf("ID = %q, want sample", meta.ID)
	}
	if meta.Cwd != "/tmp/test-project" {
		t.Errorf("Cwd = %q", meta.Cwd)
	}
	if meta.Title != "Hand-titled Session" {
		t.Errorf("Title = %q", meta.Title)
	}
	if meta.StartedAt.IsZero() {
		t.Error("StartedAt should not be zero")
	}
	if meta.LastEventAt.Before(meta.StartedAt) {
		t.Errorf("LastEventAt %v before StartedAt %v", meta.LastEventAt, meta.StartedAt)
	}
}

func TestReadSessionMeta_TitleFallback(t *testing.T) {
	meta, err := ReadSessionMeta("testdata/no-title.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "" {
		t.Errorf("Title = %q, want empty (no ai-title)", meta.Title)
	}
	if meta.Prompt != "Reply with exactly: APPLE" {
		t.Errorf("Prompt = %q", meta.Prompt)
	}
}

func TestRenameSessionUsesClaudeCustomTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"user","timestamp":"2026-07-24T00:00:00Z","message":{"role":"user","content":"first prompt"}}` + "\n" +
		`{"type":"ai-title","aiTitle":"AI title"}` + "\n" +
		`{"type":"other-metadata","customTitle":"Field title"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := ReadSessionMeta(path)
	if err != nil || meta.Title != "Field title" {
		t.Fatalf("field title meta = %+v, err=%v", meta, err)
	}
	if err := RenameSession(path, "session-1", "Native title"); err != nil {
		t.Fatal(err)
	}
	meta, err = ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "Native title" {
		t.Fatalf("Title = %q, want Native title", meta.Title)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte(`{"type":"other-metadata","customTitle":""}`+"\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err = ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "AI title" || meta.Prompt != "first prompt" {
		t.Fatalf("empty custom title meta = %+v", meta)
	}
}

func TestReadSessionMeta_Missing(t *testing.T) {
	if _, err := ReadSessionMeta("testdata/does-not-exist.jsonl"); err == nil {
		t.Error("expected error")
	}
}

// TestReadSessionMeta_LastInputAt pins the sidebar sort key: LastInputAt tracks
// the last genuine user prompt and ignores tool_result echoes, the interrupt
// marker, and the untimed metadata claude appends on pause/kill — so a paused
// session does not jump to the top of the list.
func TestReadSessionMeta_LastInputAt(t *testing.T) {
	lines := []string{
		`{"type":"user","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"user","content":"first prompt"}}`,
		`{"type":"assistant","timestamp":"2026-04-26T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}`,
		// tool_result comes back as a user-role line — must NOT count as input.
		`{"type":"user","timestamp":"2026-04-26T10:00:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		// the last genuine prompt — this is the expected LastInputAt.
		`{"type":"user","timestamp":"2026-04-26T10:00:10.000Z","message":{"role":"user","content":"second prompt"}}`,
		// interrupt marker (user-role, timestamped) — must NOT count.
		`{"type":"user","timestamp":"2026-04-26T10:05:00.000Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}`,
		// untimed metadata claude writes on pause/kill — must NOT count.
		`{"type":"last-prompt"}`,
		`{"type":"permission-mode"}`,
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-04-26T10:00:10.000Z"
	if got := meta.LastInputAt.UTC().Format("2006-01-02T15:04:05.000Z"); got != want {
		t.Errorf("LastInputAt = %s, want %s (the last genuine prompt)", got, want)
	}
	// LastEventAt still tracks the last timestamped line (the interrupt marker),
	// confirming the two clocks diverge exactly where it matters.
	if !meta.LastEventAt.After(meta.LastInputAt) {
		t.Errorf("LastEventAt %v should be after LastInputAt %v", meta.LastEventAt, meta.LastInputAt)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short: got %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "abcde…" {
		t.Errorf("long: got %q", got)
	}
	// rune-aware: each Chinese char is one rune
	if got := truncate("一二三四五六七八九十", 3); got != "一二三…" {
		t.Errorf("rune: got %q", got)
	}
}

func TestReadTurns(t *testing.T) {
	turns, _, err := ReadTurns("testdata/sample.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	// sample.jsonl has 2 user prompts and 1 assistant response → 3 grouped turns.
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Content != "first prompt" {
		t.Errorf("turns[0] = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || len(turns[1].Parts) == 0 {
		t.Errorf("turns[1] = %+v", turns[1])
	}
	if turns[1].Parts[0].Type != "text" || turns[1].Parts[0].Content != "first response" {
		t.Errorf("turns[1].Parts[0] = %+v", turns[1].Parts[0])
	}
	if turns[2].Role != "user" || turns[2].Content != "second prompt" {
		t.Errorf("turns[2] = %+v", turns[2])
	}
}

func TestReadTurns_Grouped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"user","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"user","content":"run ls"}}`,
		`{"type":"assistant","timestamp":"2026-04-26T10:00:01.000Z","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","timestamp":"2026-04-26T10:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"file1.txt"}]}}`,
		`{"type":"assistant","timestamp":"2026-04-26T10:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, _, err := ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Should produce 2 turns: user + assistant (with tool + text parts).
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Content != "run ls" {
		t.Errorf("turns[0] = %+v", turns[0])
	}
	at := turns[1]
	if at.Role != "assistant" {
		t.Fatalf("turns[1].Role = %q", at.Role)
	}
	if at.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q", at.Model)
	}
	if len(at.Parts) != 2 {
		t.Fatalf("got %d parts, want 2: %+v", len(at.Parts), at.Parts)
	}
	if at.Parts[0].Type != "tool" || at.Parts[0].ToolName != "Bash" {
		t.Errorf("parts[0] = %+v", at.Parts[0])
	}
	if at.Parts[0].ToolTarget != "ls" {
		t.Errorf("parts[0].ToolTarget = %q, want ls", at.Parts[0].ToolTarget)
	}
	if at.Parts[1].Type != "text" || at.Parts[1].Content != "done" {
		t.Errorf("parts[1] = %+v", at.Parts[1])
	}
	// Time is the turn's first event; EndTime its last.
	if want := time.Date(2026, 4, 26, 10, 0, 1, 0, time.UTC); !at.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", at.Time, want)
	}
	if want := time.Date(2026, 4, 26, 10, 0, 3, 0, time.UTC); !at.EndTime.Equal(want) {
		t.Errorf("EndTime = %v, want %v", at.EndTime, want)
	}
}

func TestReadTurns_RichToolResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		// Edit: tool_use carries name+file_path; toolUseResult carries the diff.
		`{"type":"assistant","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/repo/foo.go"}}]}}`,
		`{"type":"user","timestamp":"2026-04-26T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"e1","content":"The file has been updated."}]},"toolUseResult":{"filePath":"/repo/foo.go","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":2,"lines":["-old","+new1","+new2"]}]}}`,
		// Read: file content lives in toolUseResult.file.content.
		`{"type":"assistant","timestamp":"2026-04-26T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/repo/bar.go"}}]}}`,
		`{"type":"user","timestamp":"2026-04-26T10:00:03.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"r1","content":"1\tpackage bar"}]},"toolUseResult":{"type":"text","file":{"filePath":"/repo/bar.go","content":"package bar\n"}}}`,
		// Bash: stdout in toolUseResult; summary uses the command from input.
		`{"type":"assistant","timestamp":"2026-04-26T10:00:04.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"go test ./...\nsecond line"}}]}}`,
		`{"type":"user","timestamp":"2026-04-26T10:00:05.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1","content":"ok"}]},"toolUseResult":{"stdout":"ok  usher  0.01s","stderr":""}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, _, err := ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// All events belong to one assistant turn with 3 tool parts.
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1: %+v", len(turns), turns)
	}
	at := turns[0]
	if at.Role != "assistant" {
		t.Fatalf("Role = %q", at.Role)
	}
	var tools []TurnPart
	for _, p := range at.Parts {
		if p.Type == "tool" {
			tools = append(tools, p)
		}
	}
	if len(tools) != 3 {
		t.Fatalf("got %d tool parts, want 3: %+v", len(tools), at.Parts)
	}

	edit := tools[0]
	if edit.ToolName != "Edit" {
		t.Errorf("edit.ToolName = %q", edit.ToolName)
	}
	if !strings.Contains(edit.Content, "```diff") || !strings.Contains(edit.Content, "@@ -1,1 +1,2 @@") {
		t.Errorf("edit content missing diff fence/hunk: %q", edit.Content)
	}
	if !strings.Contains(edit.Content, "+new1") || !strings.Contains(edit.Content, "-old") {
		t.Errorf("edit content missing diff lines: %q", edit.Content)
	}

	read := tools[1]
	if read.ToolName != "Read" {
		t.Errorf("read.ToolName = %q", read.ToolName)
	}
	if !strings.Contains(read.Content, "package bar") || strings.Contains(read.Content, "```diff") {
		t.Errorf("read content = %q", read.Content)
	}

	bash := tools[2]
	if bash.ToolName != "Bash" {
		t.Errorf("bash.ToolName = %q", bash.ToolName)
	}
	if bash.ToolTarget != "go test ./..." {
		t.Errorf("bash.ToolTarget = %q", bash.ToolTarget)
	}
	if !strings.Contains(bash.Content, "ok  usher  0.01s") {
		t.Errorf("bash content = %q", bash.Content)
	}
}

func TestRenderToolResult_FallbackUnknownShape(t *testing.T) {
	// A tool with no special-cased toolUseResult shape falls back to the inline
	// tool_result text.
	ev, _ := ParseLine([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"g1","content":"match.go\nother.go"}]},"toolUseResult":{"mode":"files_with_matches","numFiles":2}}`))
	if body := renderToolResult(ev); !strings.Contains(body, "match.go") {
		t.Errorf("fallback body = %q", body)
	}
}

func TestFence_WidensPastBackticks(t *testing.T) {
	// Body containing a ``` run must be wrapped in a longer fence.
	out := fence("", "a\n```\nb")
	if !strings.HasPrefix(out, "````\n") || !strings.HasSuffix(out, "\n````") {
		t.Errorf("fence did not widen: %q", out)
	}
}

func TestReadTurns_Limit(t *testing.T) {
	turns, total, err := ReadTurns("testdata/sample.jsonl", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d, want 1", len(turns))
	}
	// total reflects the full count before the limit trim, so the client can
	// tell that older turns exist beyond the 1-turn window.
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if turns[0].Role != "user" || turns[0].Content != "second prompt" {
		t.Errorf("limited turn = %+v", turns[0])
	}
}

func TestReadTurns_Missing(t *testing.T) {
	if _, _, err := ReadTurns("testdata/does-not-exist.jsonl", 0); err == nil {
		t.Error("expected error")
	}
}

func TestReadTurns_ToolTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"assistant","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/repo/foo.go"}}]}}`,
		`{"type":"user","timestamp":"2026-04-26T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"e1","content":"ok"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, _, err := ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || len(turns[0].Parts) != 1 {
		t.Fatalf("unexpected turns: %+v", turns)
	}
	p := turns[0].Parts[0]
	if p.ToolName != "Edit" || p.ToolTarget != "/repo/foo.go" {
		t.Errorf("got toolName=%q target=%q", p.ToolName, p.ToolTarget)
	}
}

func TestReadTurns_IsMetaSkillContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"user","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"user","content":"load skill"}}`,
		`{"type":"assistant","timestamp":"2026-04-26T10:00:01.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"sk1","name":"Skill","input":{"skill":"zero-stack"}}]}}`,
		`{"type":"user","timestamp":"2026-04-26T10:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"sk1","content":"Launching skill: zero-stack"}]}}`,
		`{"type":"user","timestamp":"2026-04-26T10:00:03.000Z","isMeta":true,"sourceToolUseID":"sk1","message":{"role":"user","content":[{"type":"text","text":"# zero-stack\n\nFull skill content here."}]}}`,
		`{"type":"assistant","timestamp":"2026-04-26T10:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"skill loaded"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, _, err := ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// user + assistant (tool+text) = 2 turns; isMeta must NOT create a user turn.
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Content != "load skill" {
		t.Errorf("turns[0] = %+v", turns[0])
	}
	at := turns[1]
	if at.Role != "assistant" || len(at.Parts) != 2 {
		t.Fatalf("turns[1] role=%q parts=%d", at.Role, len(at.Parts))
	}
	tp := at.Parts[0]
	if tp.Type != "tool" || tp.ToolName != "Skill" {
		t.Errorf("parts[0] = %+v", tp)
	}
	if !strings.Contains(tp.Content, "Full skill content here.") {
		t.Errorf("isMeta content not appended to tool part: %q", tp.Content)
	}
	if at.Parts[1].Type != "text" || at.Parts[1].Content != "skill loaded" {
		t.Errorf("parts[1] = %+v", at.Parts[1])
	}
}

// TestAssembler_MatchesReadTurns feeds the sample fixtures line-by-line
// through an Assembler and checks the streamed view (completed turns + the
// final flush, with every emitted part) reproduces exactly what ReadTurns
// returns in batch — the invariant the live "part" stream relies on.
func TestAssembler_MatchesReadTurns(t *testing.T) {
	for _, fixture := range []string{"testdata/sample.jsonl", "testdata/no-title.jsonl"} {
		batch, _, err := ReadTurns(fixture, 0)
		if err != nil {
			t.Fatalf("%s: ReadTurns: %v", fixture, err)
		}

		data, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatalf("%s: read: %v", fixture, err)
		}
		asm := NewAssembler()
		var stream []Turn
		var parts []TurnPart
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			ev, err := ParseLine([]byte(line))
			if err != nil {
				continue
			}
			completed, part := asm.Feed(ev)
			stream = append(stream, completed...)
			if part != nil {
				parts = append(parts, *part)
			}
		}
		if final := asm.Flush(); final != nil {
			stream = append(stream, *final)
		}

		if len(stream) != len(batch) {
			t.Fatalf("%s: stream %d turns, batch %d", fixture, len(stream), len(batch))
		}
		for i := range batch {
			a, b := batch[i], stream[i]
			if a.Role != b.Role || a.Content != b.Content || !a.Time.Equal(b.Time) ||
				a.Model != b.Model || len(a.Parts) != len(b.Parts) {
				t.Errorf("%s: turn %d differs:\n batch %+v\nstream %+v", fixture, i, a, b)
				continue
			}
			for j := range a.Parts {
				if a.Parts[j] != b.Parts[j] {
					t.Errorf("%s: turn %d part %d differs:\n batch %+v\nstream %+v",
						fixture, i, j, a.Parts[j], b.Parts[j])
				}
			}
		}

		// Every part emitted during streaming must appear in some assistant
		// turn, in order — concat(parts of assistant turns) == emitted parts.
		var want []TurnPart
		for _, tu := range batch {
			want = append(want, tu.Parts...)
		}
		if len(want) != len(parts) {
			t.Fatalf("%s: emitted %d parts, turns hold %d", fixture, len(parts), len(want))
		}
		for i := range want {
			if want[i] != parts[i] {
				t.Errorf("%s: part %d differs:\n turn %+v\nemitted %+v", fixture, i, want[i], parts[i])
			}
		}
	}
}

// Claude Code injects <task-notification> prompts when background work
// (workflows, subagents) finishes — a one-line summary plus a machine
// payload. Transcripts show the TUI-style summary, not the blob.
func TestCompactTaskNotification(t *testing.T) {
	blob := "<task-notification>\n<task-id>abc</task-id>\n<status>completed</status>\n" +
		"<summary>Dynamic workflow \"Deep research\" completed</summary>\n<result>{\"huge\":\"json\"}</result>\n</task-notification>"
	if got := compactTaskNotification(blob); got != `[task notification] Dynamic workflow "Deep research" completed` {
		t.Errorf("got %q", got)
	}
	noSummary := "<task-notification>\n<status>completed</status>\n</task-notification>"
	if got := compactTaskNotification(noSummary); got != "[task notification] background task completed" {
		t.Errorf("no-summary got %q", got)
	}
	plain := "just a normal prompt with <summary>weird text</summary>"
	if got := compactTaskNotification(plain); got != plain {
		t.Errorf("plain text altered: %q", got)
	}
}

// End to end through the Assembler: the notification turn's Content is the
// compact form, so transcript reads and live turn.user events agree.
func TestAssemblerCompactsTaskNotification(t *testing.T) {
	a := NewAssembler()
	line := `{"type":"user","timestamp":"2026-07-04T10:13:27Z","message":{"role":"user","content":"<task-notification>\n<summary>workflow done</summary>\n<result>{}</result>\n</task-notification>"}}`
	completed, _ := a.FeedLine([]byte(line))
	if len(completed) != 1 || completed[0].Role != "user" {
		t.Fatalf("completed = %+v", completed)
	}
	if completed[0].Content != "[task notification] workflow done" {
		t.Errorf("content = %q", completed[0].Content)
	}
}

func TestAssemblerProjectsCompactBoundaryAndKeepsSummary(t *testing.T) {
	a := NewAssembler()
	a.FeedLine([]byte(`{"type":"assistant","timestamp":"2026-07-14T14:51:25Z","message":{"role":"assistant","content":[{"type":"text","text":"before"}]}}`))
	completed, _ := a.FeedLine([]byte(`{"type":"system","subtype":"compact_boundary","timestamp":"2026-07-14T14:51:26Z","content":"Conversation compacted"}`))
	if len(completed) != 2 || completed[0].Role != "assistant" || completed[1].Role != "system" {
		t.Fatalf("compact boundary completed = %+v", completed)
	}
	if completed[1].Content != "Context compacted" {
		t.Fatalf("system content = %q", completed[1].Content)
	}
	summary, _ := a.FeedLine([]byte(`{"type":"user","isCompactSummary":true,"timestamp":"2026-07-14T14:51:27Z","message":{"role":"user","content":"This session is being continued from a previous conversation."}}`))
	if len(summary) != 1 || summary[0].Role != "user" || !strings.HasPrefix(summary[0].Content, "This session") {
		t.Fatalf("compact summary = %+v", summary)
	}
}

func TestReadSessionMetaUsesLatestClaudeContext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.jsonl")
	data := strings.Join([]string{
		`{"type":"assistant","uuid":"u1","message":{"id":"m1","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`,
		`{"type":"assistant","uuid":"u2","message":{"id":"m1","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`,
		`{"type":"assistant","uuid":"u3","message":{"id":"m2","model":"claude-opus-4-6","usage":{"input_tokens":13,"output_tokens":17}}}`,
		`{"type":"assistant","uuid":"u4","message":{"id":"synthetic","content":[]}}`,
	}, "\n")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := ReadSessionMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	u := meta.Runtime
	if u.ContextTokens != 30 || u.Model != "claude-opus-4-6" {
		t.Fatalf("usage = %+v", u)
	}
}
