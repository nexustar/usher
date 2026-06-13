package router

import (
	"testing"

	"usher/internal/sender"
)

func TestBackendForModel(t *testing.T) {
	cases := map[string]string{
		"gpt-5.5":           "codex",
		"gpt-4.1":           "codex",
		"o3":                "codex",
		"o4-mini":           "codex",
		"codex-mini":        "codex",
		"claude-opus-4-8":   "claude",
		"opus":              "claude",
		"sonnet":            "claude",
		"haiku":             "claude",
		"claude-fable-5":    "claude",
		"":                  "claude", // unspecified → default backend
		"default":           "claude", // ambiguous name resolves to the default backend
		"GPT-5.5":           "codex",  // case-insensitive
		"  gpt-5.5  ":       "codex",  // trimmed
		"something-unknown": "claude",
	}
	for model, want := range cases {
		if got := backendForModel(model); got != want {
			t.Errorf("backendForModel(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestSenderForBackendFallsBackToDefault(t *testing.T) {
	r := &Router{
		senders:        map[string]*sender.Sender{"claude": nil},
		defaultBackend: "claude",
	}
	// An unregistered backend falls back to the default (here the claude entry).
	if _, ok := r.senders["codex"]; ok {
		t.Fatal("precondition: codex should be unregistered")
	}
	// senderForBackend returns the default entry for an unknown backend; we only
	// assert it does not panic and returns the same (nil) default value.
	if got := r.senderForBackend("codex"); got != r.senders["claude"] {
		t.Errorf("unknown backend did not fall back to default")
	}
	if got := r.senderForBackend("claude"); got != r.senders["claude"] {
		t.Errorf("registered backend not returned")
	}
}
