package discovery

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestDiscovery(t *testing.T, root string) *Discovery {
	t.Helper()
	d, err := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewClaudeSource(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.watcher.Close() })
	return d
}

func writeJSONL(t *testing.T, path, sessionID, cwd, prompt string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd +
		`","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"user","content":"` +
		prompt + `"},"uuid":"u1"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscovery_ScanFindsExisting(t *testing.T) {
	tmp := t.TempDir()
	writeJSONL(t, filepath.Join(tmp, "-tmp-a", "abc1234.jsonl"), "abc1234", "/tmp/a", "hello A")
	writeJSONL(t, filepath.Join(tmp, "-tmp-b", "def5678.jsonl"), "def5678", "/tmp/b", "hello B")

	d := newTestDiscovery(t, tmp)
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}
	got := d.List()
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}

	byID := map[string]bool{}
	for _, s := range got {
		byID[s.ID] = true
	}
	for _, want := range []string{"abc1234", "def5678"} {
		if !byID[want] {
			t.Errorf("missing session %q", want)
		}
	}
}

func TestDiscovery_GetAndPath(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "-tmp-x", "abc.jsonl")
	writeJSONL(t, path, "abc", "/tmp/x", "hi")

	d := newTestDiscovery(t, tmp)
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}

	sess, ok := d.Get("abc")
	if !ok {
		t.Fatal("session abc not found")
	}
	if sess.Cwd != "/tmp/x" {
		t.Errorf("Cwd = %q", sess.Cwd)
	}

	gotPath, ok := d.Path("abc")
	if !ok || gotPath != path {
		t.Errorf("Path: got %q ok=%v, want %q", gotPath, ok, path)
	}

	if _, ok := d.Get("missing"); ok {
		t.Error("Get returned ok for unknown id")
	}
}

func TestDiscovery_Remove(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "-tmp-x", "abc.jsonl")
	writeJSONL(t, path, "abc", "/tmp/x", "hi")

	d := newTestDiscovery(t, tmp)
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Get("abc"); !ok {
		t.Fatal("precondition: abc should be known")
	}

	d.Remove("abc")
	if _, ok := d.Get("abc"); ok {
		t.Error("Get returned ok after Remove")
	}
	if _, ok := d.Path("abc"); ok {
		t.Error("Path returned ok after Remove")
	}
	d.Remove("abc") // idempotent / unknown id is a no-op
}

func TestDiscovery_WatchPicksUpNewFile(t *testing.T) {
	tmp := t.TempDir()
	d := newTestDiscovery(t, tmp)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Create a project subdir; fsnotify should observe it and add a watch.
	proj := filepath.Join(tmp, "-tmp-watch")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	// Give the goroutine a beat to add the new subdir to its watch set.
	time.Sleep(150 * time.Millisecond)

	writeJSONL(t, filepath.Join(proj, "watched.jsonl"), "watched", "/tmp/watch", "ping")

	// Poll up to 2s for the new file to be discovered.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.Get("watched"); ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("watched session not discovered; current list=%v", d.List())
}

func TestDiscovery_SkipsNestedJSONL(t *testing.T) {
	tmp := t.TempDir()
	// Real session.
	writeJSONL(t, filepath.Join(tmp, "-p", "real.jsonl"), "real", "/tmp/p", "hi")
	// Subagent transcript Claude Code writes alongside.
	writeJSONL(t, filepath.Join(tmp, "-p", "real", "subagents", "agent-deadbeef.jsonl"),
		"real", "/tmp/p", "subagent work")
	// Future-proof: a hypothetical jsonl in some other nested subdir.
	writeJSONL(t, filepath.Join(tmp, "-p", "real", "tool-results", "boia1ypmh.jsonl"),
		"real", "/tmp/p", "tool result")

	d := newTestDiscovery(t, tmp)
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}
	got := d.List()
	if len(got) != 1 {
		t.Fatalf("expected only the real session; got %d: %v", len(got), got)
	}
	if got[0].ID != "real" {
		t.Errorf("got id %q, want %q", got[0].ID, "real")
	}
}

func TestDiscovery_RereadsMetaWhenInitiallyEmpty(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "-tmp-late", "late.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The jsonl is created empty first (claude makes the file before writing
	// the turn) — the initial read can't find cwd/title.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDiscovery(t, tmp)
	d.upsert(path)
	if s, _ := d.Get("late"); s.Cwd != "" || s.Title != "" {
		t.Fatalf("expected empty meta on first read; got cwd=%q title=%q", s.Cwd, s.Title)
	}

	// claude writes the turn; a Write event re-upserts. cwd/title must now fill
	// in without waiting for a restart.
	writeJSONL(t, path, "late", "/tmp/late", "hello late")
	d.upsert(path)

	s, ok := d.Get("late")
	if !ok {
		t.Fatal("session late missing")
	}
	if s.Cwd != "/tmp/late" {
		t.Errorf("Cwd = %q, want /tmp/late after re-read", s.Cwd)
	}
	if s.Title == "" {
		t.Error("Title still empty after re-read; want first-prompt fallback")
	}
}

func TestDiscovery_ListSorted(t *testing.T) {
	tmp := t.TempDir()
	older := filepath.Join(tmp, "-p", "older.jsonl")
	newer := filepath.Join(tmp, "-p", "newer.jsonl")
	writeJSONL(t, older, "older", "/tmp/p", "old")
	writeJSONL(t, newer, "newer", "/tmp/p", "new")

	// Set distinct mtimes so sort order is deterministic.
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}

	d := newTestDiscovery(t, tmp)
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}
	got := d.List()
	if len(got) != 2 || got[0].ID != "newer" {
		t.Fatalf("expected newer first; got %v", got)
	}
}
