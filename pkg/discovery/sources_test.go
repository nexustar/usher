package discovery

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const codexUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// a minimal but real-shaped rollout: header + a user prompt + turn-complete.
const codexRollout = `{"timestamp":"2026-06-14T00:00:00Z","type":"session_meta","payload":{"id":"` + codexUUID + `","cwd":"/tmp/proj","timestamp":"2026-06-14T00:00:00Z"}}
{"timestamp":"2026-06-14T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}
{"timestamp":"2026-06-14T00:00:09Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}
`

// TestCodexSource_DiscoversDatePartitioned proves the Source abstraction lets
// Discovery find a Codex rollout nested three levels deep
// (<root>/YYYY/MM/DD/rollout-…), which the Claude depth-1 filter would reject.
func TestCodexSource_DiscoversDatePartitioned(t *testing.T) {
	tmp := t.TempDir()
	roll := filepath.Join(tmp, "2026", "06", "14",
		"rollout-2026-06-14T00-00-00-"+codexUUID+".jsonl")
	writeFile(t, roll, codexRollout)
	// A stray non-rollout file in the tree must be ignored.
	writeFile(t, filepath.Join(tmp, "2026", "06", "14", "notes.jsonl"), "{}\n")

	d, err := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewCodexSource(tmp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.watcher.Close() })
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}

	got := d.List()
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1 (rollout only); %+v", len(got), got)
	}
	s := got[0]
	if s.ID != codexUUID {
		t.Errorf("ID = %q, want %q", s.ID, codexUUID)
	}
	if s.Cwd != "/tmp/proj" {
		t.Errorf("Cwd = %q, want /tmp/proj", s.Cwd)
	}
	if s.Title != "hello codex" {
		t.Errorf("Title = %q, want 'hello codex'", s.Title)
	}

	gotPath, ok := d.Path(codexUUID)
	if !ok || gotPath != roll {
		t.Errorf("Path = %q ok=%v, want %q", gotPath, ok, roll)
	}
}

// TestMultiSource_CoexistClaudeAndCodex proves one Discovery scans a Claude
// projects tree and a Codex rollout tree at once, merging both into the session
// list and tagging each with the backend that found it.
func TestMultiSource_CoexistClaudeAndCodex(t *testing.T) {
	claudeRoot := t.TempDir()
	codexRoot := t.TempDir()
	writeJSONL(t, filepath.Join(claudeRoot, "-tmp-a", "claudesess.jsonl"),
		"claudesess", "/tmp/a", "hi claude")
	writeFile(t, filepath.Join(codexRoot, "2026", "06", "14",
		"rollout-2026-06-14T00-00-00-"+codexUUID+".jsonl"), codexRollout)

	d, err := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewClaudeSource(claudeRoot), NewCodexSource(codexRoot))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.watcher.Close() })
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}

	got := d.List()
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (one per backend); %+v", len(got), got)
	}
	backendByID := map[string]string{}
	for _, s := range got {
		backendByID[s.ID] = s.Backend
	}
	if backendByID["claudesess"] != "claude" {
		t.Errorf("claude session backend = %q, want claude", backendByID["claudesess"])
	}
	if backendByID[codexUUID] != "codex" {
		t.Errorf("codex session backend = %q, want codex", backendByID[codexUUID])
	}
}

// TestCodexSource_IsSessionFile guards the filename predicate.
func TestCodexSource_IsSessionFile(t *testing.T) {
	s := NewCodexSource("/root")
	cases := map[string]bool{
		"/root/2026/06/14/rollout-2026-06-14T00-00-00-" + codexUUID + ".jsonl": true,
		"/root/2026/06/14/notes.jsonl":                                         false,
		"/root/2026/06/14/rollout-no-uuid-here.jsonl":                          false,
		"/root/2026/06/14/rollout-x.txt":                                       false,
	}
	for path, want := range cases {
		if got := s.IsSessionFile(path); got != want {
			t.Errorf("IsSessionFile(%q) = %v, want %v", path, got, want)
		}
	}
}
