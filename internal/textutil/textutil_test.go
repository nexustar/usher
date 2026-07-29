package textutil

import (
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	if got := Truncate("abc", 10); got != "abc" {
		t.Errorf("short: got %q", got)
	}
	if got := Truncate("abcdefghij", 5); got != "abcde…" {
		t.Errorf("long: got %q", got)
	}
	// rune-aware: each Chinese char is one rune
	if got := Truncate("一二三四五六七八九十", 3); got != "一二三…" {
		t.Errorf("rune: got %q", got)
	}
}

func TestFirstLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  git status  ", "git status"},
		{"git status\ngit log", "git status"},
		{"\ngit log", ""},
		{"", ""},
	} {
		if got := FirstLine(tc.in); got != tc.want {
			t.Errorf("FirstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("4d924b55-a868-4225"); got != "4d924b55" {
		t.Errorf("long: got %q", got)
	}
	if got := ShortID("abc"); got != "abc" {
		t.Errorf("short: got %q", got)
	}
}

func TestFence_WidensPastBackticks(t *testing.T) {
	// Body containing a ``` run must be wrapped in a longer fence.
	out := Fence("", "a\n```\nb")
	if !strings.HasPrefix(out, "````\n") || !strings.HasSuffix(out, "\n````") {
		t.Errorf("fence did not widen: %q", out)
	}
	if out := Fence("diff", "x"); !strings.HasPrefix(out, "```diff\n") {
		t.Errorf("lang not applied: %q", out)
	}
}

func TestClampBody(t *testing.T) {
	if got := ClampBody(strings.Repeat("x", 40*1024)); !strings.HasSuffix(got, "\n… (truncated)") {
		t.Error("oversize body not clamped")
	}
	got := ClampBody(strings.Repeat("line\n", 500))
	if lines := strings.Split(got, "\n"); len(lines) != 401 {
		t.Errorf("line count = %d, want 401", len(lines))
	}
	if in := "small\nbody"; ClampBody(in) != in {
		t.Error("small body altered")
	}
}
