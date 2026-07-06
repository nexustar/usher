package imutil

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAssistantText(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{`{"message":{"content":[{"type":"text","text":"hello"}]}}`, "hello"},
		{`{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`, "a\n\nb"},
		{`{"message":{"content":[{"type":"tool_use","id":"x"}]}}`, ""},
		{`{"message":{"content":[{"type":"text","text":"keep"},{"type":"tool_use"}]}}`, "keep"},
		{`not json`, ""},
	}
	for _, c := range cases {
		if got := AssistantText(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("AssistantText(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestExtractUserText(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"message":{"content":"do the thing"}}`, "do the thing"},
		{`{"message":{"content":[{"type":"text","text":"hello"}]}}`, "hello"},
		// tool_result user event carries no prompt text → not echoed.
		{`{"message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"out"}]}}`, ""},
		{`not json`, ""},
	}
	for _, c := range cases {
		if got := ExtractUserText(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("ExtractUserText(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestSplitMessage(t *testing.T) {
	const max = 4096
	if got := SplitMessage("short", max); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short text should be one chunk, got %v", got)
	}
	long := strings.Repeat("x", max+50)
	chunks := SplitMessage(long, max)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len([]rune(c)) > max {
			t.Fatalf("chunk over limit: %d", len([]rune(c)))
		}
	}
	if chunks[0]+chunks[1] != long {
		t.Fatal("chunks should reassemble to original when no newline cut")
	}
}

func TestTurnDuration(t *testing.T) {
	raw := json.RawMessage(`{"user_ts":"2026-06-25T03:00:00Z","assistant_ts":"2026-06-25T03:01:12Z"}`)
	d, ok := TurnDuration(raw)
	if !ok || d != 72*time.Second {
		t.Fatalf("TurnDuration = %v,%v want 72s,true", d, ok)
	}
	if got := HumanizeDuration(d); got != "1m12s" {
		t.Errorf("HumanizeDuration = %q, want 1m12s", got)
	}
	// Missing / out-of-order timestamps → no duration.
	if _, ok := TurnDuration(json.RawMessage(`{}`)); ok {
		t.Error("empty exit event should yield no duration")
	}
	if _, ok := TurnDuration(json.RawMessage(`{"user_ts":"2026-06-25T03:01:00Z","assistant_ts":"2026-06-25T03:00:00Z"}`)); ok {
		t.Error("out-of-order timestamps should yield no duration")
	}
}

func TestImageRefs(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`{"message":{"content":[{"type":"tool_use","name":"mcp__usher__show_image","input":{"file_path":"out/chart.png"}}]}}`, []string{"out/chart.png"}},
		{`{"message":{"content":[{"type":"tool_use","name":"show_image","input":{"file_path":"/abs/a.jpg"}}]}}`, []string{"/abs/a.jpg"}},
		{`{"message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`, nil},
		{`{"message":{"content":[{"type":"tool_use","name":"mcp__x__show_image","input":{}}]}}`, nil}, // empty path skipped
		{`not json`, nil},
	}
	for _, c := range cases {
		got := ImageRefs(json.RawMessage(c.raw))
		if len(got) != len(c.want) {
			t.Errorf("ImageRefs(%s) = %v, want %v", c.raw, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ImageRefs(%s)[%d] = %q, want %q", c.raw, i, got[i], c.want[i])
			}
		}
	}
}
