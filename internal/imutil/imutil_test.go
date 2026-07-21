package imutil

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTurnUserText(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"role":"user","content":"do the thing"}`, "do the thing"},
		{`{"role":"assistant","content":"nope"}`, ""},
		{`{"message":{"content":"old shape"}}`, ""},
		{`not json`, ""},
	}
	for _, c := range cases {
		if got := TurnUserText(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("TurnUserText(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestSamePromptEchoAllowsCodexSlashSkillNormalization(t *testing.T) {
	tests := []struct {
		sent, observed string
		want           bool
	}{
		{"hello", "hello", true},
		{"/imagegen draw", "$imagegen draw", true},
		{"/imagegen draw", "$imagegen paint", false},
		{"$imagegen draw", "/imagegen draw", false},
		{"/", "$", false},
		{"/imagegen", "$imagegen", true},
		{"/imagegen", "$imagegen draw", false},
		// The rewrite matches names exactly, so a cased echo is a real
		// mismatch, not the rewrite — do not swallow it.
		{"/ImageGen draw", "$imagegen draw", false},
	}
	for _, tt := range tests {
		if got := SamePromptEcho(tt.sent, tt.observed); got != tt.want {
			t.Errorf("SamePromptEcho(%q, %q) = %v, want %v", tt.sent, tt.observed, got, tt.want)
		}
	}
}

func TestPartText(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"role":"assistant","part":{"type":"text","content":"hello"}}`, "hello"},
		{`{"role":"assistant","part":{"type":"tool","content":"ignored"}}`, ""},
		{`{"role":"user","part":{"type":"text","content":"ignored"}}`, ""},
		{`not json`, ""},
	}
	for _, c := range cases {
		if got := PartText(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("PartText(%s) = %q, want %q", c.raw, got, c.want)
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
	// assistant_end_ts wins over assistant_ts.
	raw = json.RawMessage(`{"user_ts":"2026-06-25T03:00:00Z","assistant_ts":"2026-06-25T03:00:08Z","assistant_end_ts":"2026-06-25T03:02:00Z"}`)
	if d, ok := TurnDuration(raw); !ok || d != 2*time.Minute {
		t.Fatalf("TurnDuration with end ts = %v,%v want 2m0s,true", d, ok)
	}
	// Missing / out-of-order timestamps → no duration.
	if _, ok := TurnDuration(json.RawMessage(`{}`)); ok {
		t.Error("empty exit event should yield no duration")
	}
	if _, ok := TurnDuration(json.RawMessage(`{"user_ts":"2026-06-25T03:01:00Z","assistant_ts":"2026-06-25T03:00:00Z"}`)); ok {
		t.Error("out-of-order timestamps should yield no duration")
	}
}

func TestPartImageRefs(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`{"role":"assistant","part":{"type":"tool","toolName":"mcp__usher__show_image","toolTarget":"out/chart.png"}}`, []string{"out/chart.png"}},
		{`{"role":"assistant","part":{"type":"tool","toolName":"show_image","toolTarget":"/abs/a.jpg"}}`, []string{"/abs/a.jpg"}},
		{`{"role":"assistant","part":{"type":"tool","toolName":"Bash","toolTarget":"ls"}}`, nil},
		{`{"role":"assistant","part":{"type":"text","content":"hi"}}`, nil},
		{`not json`, nil},
	}
	for _, c := range cases {
		got := PartImageRefs(json.RawMessage(c.raw))
		if len(got) != len(c.want) {
			t.Errorf("PartImageRefs(%s) = %v, want %v", c.raw, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("PartImageRefs(%s)[%d] = %q, want %q", c.raw, i, got[i], c.want[i])
			}
		}
	}
}
