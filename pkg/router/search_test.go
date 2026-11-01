package router

import (
	"reflect"
	"testing"

	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/core"
	"github.com/nexustar/usher/pkg/transcript"
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
	turns, _, err := (transcript.Claude{}).ReadBefore(path, "", 0)
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
		{Role: "user", Cursor: "c0", Content: "deploy the auth service"},
		{Role: "assistant", Cursor: "c1", Parts: []core.TurnPart{{Type: "text", Content: "AUTH done"}}},
		{Role: "user", Content: "unrelated"},
		{Role: "assistant", Parts: []core.TurnPart{
			{Type: "tool", ToolName: "Bash", ToolTarget: "auth.sh"}, // tool part must NOT match
		}},
	}
	// case-insensitive; matches turns 0 and 1 (prose), not turn 3 (tool only).
	hits, matched := scanTurnsForQuery(turns, searchTerms("auth"), 10, 40)
	if matched != 2 {
		t.Fatalf("matched = %d, want 2 (tool part excluded)", matched)
	}
	if len(hits) != 2 || hits[0].Cursor != turns[0].Cursor || hits[1].Cursor != turns[1].Cursor {
		t.Fatalf("hits = %+v", hits)
	}
	// maxHits caps returned snippets but not the matched count.
	hits, matched = scanTurnsForQuery(turns, searchTerms("auth"), 1, 40)
	if matched != 2 || len(hits) != 1 {
		t.Fatalf("capped: matched=%d hits=%d, want 2 and 1", matched, len(hits))
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

func TestSearchTerms(t *testing.T) {
	cases := map[string][]string{
		"auth":             {"auth"},
		"  auth\ttimeout ": {"auth", "timeout"},
		"迁移　记录":            {"迁移", "记录"}, // ideographic space
		`"auth service"`:   {"auth service"},
		`deploy "a b" x`:   {"deploy", "a b", "x"},
		`"a b"c`:           {"a b", "c"},
		`say "hi`:          {"say", `"hi`},
		`it"s fine`:        {`it"s`, "fine"},
		`""`:               nil,
		"   ":              nil,
	}
	for query, want := range cases {
		var got []string
		for _, term := range searchTerms(query) {
			got = append(got, string(term))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("searchTerms(%q) = %q, want %q", query, got, want)
		}
	}
}

func TestScanTurnsRequiresEveryTermInOneTurn(t *testing.T) {
	turns := []core.Turn{
		{Role: "user", Cursor: "c0", Content: "the timeout came from auth"},
		{Role: "user", Cursor: "c1", Content: "auth only, auth again"},
		{Role: "user", Cursor: "c2", Content: "timeout only"},
		{Role: "user", Cursor: "c3", Content: "auth service down"},
		{Role: "assistant", Cursor: "c4", Parts: []core.TurnPart{
			{Type: "text", Content: "AUTH"}, {Type: "text", Content: "TIMEOUT"},
		}},
	}
	hits, matched := scanTurnsForQuery(turns, searchTerms("auth timeout"), 10, 3)
	if matched != 2 || hits[0].Cursor != "c0" || hits[1].Cursor != "c4" {
		t.Fatalf("matched %d, hits %+v", matched, hits)
	}
	// The snippet opens at whichever term comes first, in any query order.
	if hits[0].Occurrences != 2 || hits[0].Snippet != "…he timeout ca…" {
		t.Errorf("hit = %+v", hits[0])
	}
	if hits, matched := scanTurnsForQuery(turns, searchTerms(`"auth service"`), 10, 3); matched != 1 || hits[0].Cursor != "c3" {
		t.Errorf("phrase: matched %d, hits %+v", matched, hits)
	}
	if _, matched := scanTurnsForQuery(turns, searchTerms(`"auth timeout"`), 10, 3); matched != 0 {
		t.Errorf("phrase must stay contiguous: matched %d", matched)
	}
}

type panickyTranscript struct{ backend.Transcript }

func (panickyTranscript) ReadBefore(string, string, int) ([]core.Turn, bool, error) {
	panic("bad log")
}

func TestSearchSessionSurvivesPanic(t *testing.T) {
	r := &Router{backends: map[string]backend.Backend{"bad": {Transcript: panickyTranscript{}}}}
	if res := r.searchSession(core.Session{ID: "s"}, "log", "bad", searchTerms("x"), 10); res != nil {
		t.Fatalf("res = %+v", res)
	}
}
