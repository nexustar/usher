package router

import (
	"testing"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/transcript"
)

// The assistant text lives in Parts, not Content — the pre-fix mapping dropped
// it. flattenTurnText must recover it, and inline tool parts only when asked.
func TestFlattenTurnText(t *testing.T) {
	assistant := core.Turn{
		Role: "assistant",
		Parts: []core.TurnPart{
			{Type: "text", Content: "let me check the config"},
			{Type: "tool", ToolName: "Read", ToolTarget: "config.go"},
			{Type: "text", Content: "found it"},
		},
	}
	user := core.Turn{Role: "user", Content: "what does config.go do?"}

	if got := flattenTurnText(user, true); got != "what does config.go do?" {
		t.Errorf("user turn: got %q", got)
	}

	noTools := flattenTurnText(assistant, false)
	if noTools != "let me check the config\nfound it" {
		t.Errorf("assistant (no tools): got %q", noTools)
	}

	withTools := flattenTurnText(assistant, true)
	want := "let me check the config\n[tool: Read config.go]\nfound it"
	if withTools != want {
		t.Errorf("assistant (with tools): got %q want %q", withTools, want)
	}
}

// Proves the read-path fix against a real Claude log: the assistant turn's
// text ("hi") must survive parsing, where mapping Turn.Content alone lost it.
func TestFlattenRecoversAssistantTextFromLog(t *testing.T) {
	path := writeTemp(t, "claude.jsonl", claudeLog)
	turns, _, err := (transcript.Claude{}).ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	var assistantText string
	for _, tn := range turns {
		if tn.Role == "assistant" {
			assistantText = flattenTurnText(tn, false)
		}
	}
	if assistantText != "hi" {
		t.Fatalf("assistant text not recovered: got %q", assistantText)
	}
}

func TestFoldFindAll(t *testing.T) {
	cases := []struct {
		hay, needle string
		wantFirst   int
		wantCount   int
	}{
		{"the Timeout bug and a timeout retry", "timeout", 4, 2}, // case-insensitive, 2 hits
		{"no match here", "xyz", -1, 0},
		{"迁移到 tmux 的迁移记录", "迁移", 0, 2}, // CJK runes
		{"aaaa", "aa", 0, 2},           // non-overlapping count
		{"anything", "", -1, 0},        // empty needle
	}
	for _, c := range cases {
		first, count := foldFindAll([]rune(c.hay), []rune(c.needle))
		if first != c.wantFirst || count != c.wantCount {
			t.Errorf("foldFindAll(%q,%q) = (%d,%d), want (%d,%d)",
				c.hay, c.needle, first, count, c.wantFirst, c.wantCount)
		}
	}
}

func TestScanTurnsForQuery(t *testing.T) {
	turns := []core.Turn{
		{Role: "user", Content: "deploy the auth service"},
		{Role: "assistant", Parts: []core.TurnPart{{Type: "text", Content: "AUTH done"}}},
		{Role: "user", Content: "unrelated"},
		{Role: "assistant", Parts: []core.TurnPart{
			{Type: "tool", ToolName: "Bash", ToolTarget: "auth.sh"}, // tool part must NOT match
		}},
	}
	// case-insensitive; matches turns 0 and 1 (prose), not turn 3 (tool only).
	hits, matched := scanTurnsForQuery(turns, []rune("auth"), 10, 40)
	if matched != 2 {
		t.Fatalf("matched = %d, want 2 (tool part excluded)", matched)
	}
	if len(hits) != 2 || hits[0].TurnIndex != 0 || hits[1].TurnIndex != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	// maxHits caps returned snippets but not the matched count.
	hits, matched = scanTurnsForQuery(turns, []rune("auth"), 1, 40)
	if matched != 2 || len(hits) != 1 {
		t.Fatalf("capped: matched=%d hits=%d, want 2 and 1", matched, len(hits))
	}
}

func TestPageBounds(t *testing.T) {
	cases := []struct {
		offset, limit, total int
		wantStart, wantEnd   int
	}{
		{offset: -1, limit: 20, total: 100, wantStart: 80, wantEnd: 100},   // recent page
		{offset: 10, limit: 20, total: 100, wantStart: 10, wantEnd: 30},    // absolute page
		{offset: 95, limit: 20, total: 100, wantStart: 95, wantEnd: 100},   // last page shorter
		{offset: 200, limit: 20, total: 100, wantStart: 100, wantEnd: 100}, // past end → empty
		{offset: -1, limit: 50, total: 10, wantStart: 0, wantEnd: 10},      // page larger than total
		{offset: -1, limit: 20, total: 0, wantStart: 0, wantEnd: 0},        // empty transcript
	}
	for _, c := range cases {
		start, end := pageBounds(c.offset, c.limit, c.total)
		if start != c.wantStart || end != c.wantEnd {
			t.Errorf("pageBounds(%d,%d,%d) = (%d,%d), want (%d,%d)",
				c.offset, c.limit, c.total, start, end, c.wantStart, c.wantEnd)
		}
	}
}

func TestSnippetAround(t *testing.T) {
	text := []rune("the quick brown fox jumps")
	// match "brown" at rune index 10, len 5, ctx 3 → "…ck brown fo…"
	if got := snippetAround(text, 10, 5, 3); got != "…ck brown fo…" {
		t.Errorf("mid snippet: got %q", got)
	}
	// ctx reaching both ends → no ellipses
	if got := snippetAround(text, 0, 3, 100); got != "the quick brown fox jumps" {
		t.Errorf("full snippet: got %q", got)
	}
	// newlines collapse to spaces
	nl := []rune("line one\nline two")
	if got := snippetAround(nl, 0, 4, 100); got != "line one line two" {
		t.Errorf("newline collapse: got %q", got)
	}
}
