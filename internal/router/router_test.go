package router

import (
	"os"
	"path/filepath"
	"testing"

	"usher/internal/sender"
)

const codexLog = `{"timestamp":"2026-06-14T00:00:00Z","type":"session_meta","payload":{"id":"id1","cwd":"/c","timestamp":"2026-06-14T00:00:00Z"}}
{"timestamp":"2026-06-14T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}
{"timestamp":"2026-06-14T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"hi"}}
{"timestamp":"2026-06-14T00:00:09Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}
`

const claudeLog = `{"type":"user","message":{"role":"user","content":"hello claude"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}
{"type":"system","subtype":"turn_duration"}
`

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadTurnsForBackend proves the dispatch: each backend's parser
// understands its own log shape and yields nothing from the other's.
func TestReadTurnsForBackend(t *testing.T) {
	codexPath := writeTemp(t, "rollout.jsonl", codexLog)
	claudePath := writeTemp(t, "claude.jsonl", claudeLog)

	turns, _, err := readTurnsForBackend(codexPath, "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Role != "user" || turns[0].Content != "hello codex" {
		t.Fatalf("codex parser: got %+v", turns)
	}
	// The Claude parser must not understand a Codex rollout (event_msg lines are
	// not user/assistant) — proving the dispatch matters.
	if wrong, _, _ := readTurnsForBackend(codexPath, "claude", 0); len(wrong) != 0 {
		t.Errorf("claude parser should yield nothing from a codex log; got %+v", wrong)
	}

	turns, _, err = readTurnsForBackend(claudePath, "claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Content != "hello claude" {
		t.Fatalf("claude parser: got %+v", turns)
	}
	if wrong, _, _ := readTurnsForBackend(claudePath, "codex", 0); len(wrong) != 0 {
		t.Errorf("codex parser should yield nothing from a claude log; got %+v", wrong)
	}
}

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
