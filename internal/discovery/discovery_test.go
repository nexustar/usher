package discovery

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/core"
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
	writeJSONLAt(t, path, sessionID, cwd, prompt, "2026-04-26T10:00:00.000Z")
}

func writeJSONLAt(t *testing.T, path, sessionID, cwd, prompt, ts string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd +
		`","timestamp":"` + ts + `","message":{"role":"user","content":"` +
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

// TestDiscovery_LateAITitle covers ai-title surfacing on a live session.
// Claude writes the ai-title some turns AFTER the first user prompt, so upsert
// must keep re-reading while Title is empty and pick it up on a later Write —
// without waiting for a full re-scan.
func TestDiscovery_LateAITitle(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "-tmp-x", "late.jsonl")
	writeJSONL(t, path, "late", "/tmp/x", "first prompt")

	d := newTestDiscovery(t, tmp)
	d.Upsert(path) // first ingest: only the prompt is on disk

	sess, ok := d.Get("late")
	if !ok {
		t.Fatal("session late not found")
	}
	if sess.Title != "first prompt" {
		t.Fatalf("after first ingest Title = %q, want prompt fallback", sess.Title)
	}

	// ai-title lands later, as it does in a real session.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"ai-title","sessionId":"late","aiTitle":"Real AI Title","timestamp":"2026-04-26T10:05:00.000Z"}` + "\n")
	f.Close()

	d.Upsert(path) // a normal fsnotify Write after the ai-title line

	sess, _ = d.Get("late")
	if sess.Title != "Real AI Title" {
		t.Errorf("ai-title not surfaced on later Write: Title = %q, want %q", sess.Title, "Real AI Title")
	}
}

func TestDiscovery_NativeTitleChange(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "-tmp-x", "rename.jsonl")
	writeJSONL(t, path, "rename", "/tmp/x", "first prompt")

	d := newTestDiscovery(t, tmp)
	d.Upsert(path)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"custom-title","sessionId":"rename","customTitle":"Renamed"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	d.Upsert(path)

	sess, _ := d.Get("rename")
	if sess.Title != "Renamed" {
		t.Fatalf("Title = %q, want Renamed", sess.Title)
	}
}

func TestDiscoveryRefreshesCodexNativeTitleIndex(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "sessions")
	path := filepath.Join(root, "2026", "07", "24",
		"rollout-2026-07-24T00-00-00-019f8abc-def0-7123-8456-789abcdef012.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"2026-07-24T00:00:00Z","type":"session_meta","payload":{"id":"019f8abc-def0-7123-8456-789abcdef012","cwd":"/work"}}` + "\n" +
		`{"timestamp":"2026-07-24T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"first prompt"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(home, "session_index.jsonl")
	if err := os.WriteFile(indexPath, []byte(`{"id":"019f8abc-def0-7123-8456-789abcdef012","thread_name":"First"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := NewMulti(nil, NewCodexSource(root))
	if err != nil {
		t.Fatal(err)
	}
	d.Upsert(path)
	sess, _ := d.Get("019f8abc-def0-7123-8456-789abcdef012")
	if sess.Title != "First" {
		t.Fatalf("initial Title = %q", sess.Title)
	}
	f, err := os.OpenFile(indexPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"019f8abc-def0-7123-8456-789abcdef012","thread_name":"Second"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !d.refreshMetadataFile(indexPath) {
		t.Fatal("metadata file was not recognized")
	}
	sess, _ = d.Get("019f8abc-def0-7123-8456-789abcdef012")
	if sess.Title != "Second" {
		t.Fatalf("refreshed Title = %q", sess.Title)
	}
}

// TestDiscovery_ConcurrentUpsert exercises upsert/List/Get from several
// goroutines at once. Run under -race it guards the map accesses in upsert
// against the watch goroutine racing a synchronous Upsert (e.g. ForkSession).
func TestDiscovery_ConcurrentUpsert(t *testing.T) {
	tmp := t.TempDir()
	d := newTestDiscovery(t, tmp)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		id := "s" + strconv.Itoa(i)
		path := filepath.Join(tmp, "-p", id+".jsonl")
		writeJSONL(t, path, id, "/tmp/p", "hi")
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				d.Upsert(path)
				d.List()
				d.Get(id)
			}
		}()
	}
	wg.Wait()
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

func TestDiscovery_MarkInputOrdersList(t *testing.T) {
	tmp := t.TempDir()
	// Both fixtures carry the same content timestamp, so initial order is
	// unspecified; MarkInput must then deterministically float "b" to the top.
	writeJSONL(t, filepath.Join(tmp, "-tmp-a", "aaa.jsonl"), "aaa", "/tmp/a", "hi a")
	writeJSONL(t, filepath.Join(tmp, "-tmp-b", "bbb.jsonl"), "bbb", "/tmp/b", "hi b")

	d := newTestDiscovery(t, tmp)
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}

	d.MarkInput("bbb", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	if got := d.List(); got[0].ID != "bbb" {
		t.Errorf("List()[0] = %q, want bbb (most recent input first)", got[0].ID)
	}

	// Monotonic: an earlier stamp must not move the clock backwards.
	d.MarkInput("bbb", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if s, _ := d.Get("bbb"); !s.LastInputAt.Equal(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("LastInputAt regressed to %v", s.LastInputAt)
	}

	// Unknown id and zero time are no-ops, not panics.
	d.MarkInput("nope", time.Now())
	d.MarkInput("bbb", time.Time{})
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
	all := d.ListAll()
	if len(all) != 2 {
		t.Fatalf("ListAll = %d sessions, want root + subagent: %v", len(all), all)
	}
	var sub core.Session
	for _, s := range all {
		if s.IsSubagent {
			sub = s
		}
	}
	if sub.ID != "real::agent-deadbeef" || sub.ParentID != "real" || sub.AgentName != "agent-deadbeef" {
		t.Fatalf("subagent metadata = %+v", sub)
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

func TestDiscovery_RereadsLateCodexSubagentMeta(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "2026", "07", "13", "rollout-2026-07-13T00-00-00-019f5723-e850-7c71-b2e5-7735abe145fb.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewCodexSource(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.watcher.Close() })
	d.upsert(path)
	id := "019f5723-e850-7c71-b2e5-7735abe145fb"
	if got, ok := d.Get(id); !ok || got.IsSubagent {
		t.Fatalf("initial empty session = %+v, found=%v", got, ok)
	}
	raw := `{"timestamp":"2026-07-13T00:00:00Z","type":"session_meta","payload":{"id":"019f5723-e850-7c71-b2e5-7735abe145fb","cwd":"/tmp/project","thread_source":"subagent","parent_thread_id":"parent-1","agent_nickname":"Archimedes"}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	d.upsert(path)
	got, ok := d.Get(id)
	if !ok || !got.IsSubagent || got.ParentID != "parent-1" || got.AgentName != "Archimedes" || got.Title != "Archimedes" {
		t.Fatalf("late subagent metadata not applied: %+v, found=%v", got, ok)
	}
}

// TestDiscovery_WatchPicksUpNestedMkdirAll covers the case where a backend
// creates deeply nested directories in one shot (MkdirAll) and writes a session
// file before fsnotify has delivered the intermediate directory Create events.
// This is the exact scenario for Codex on the first day of a new month:
// the watcher knows about <root>/2026/ but 07/01/ doesn't exist yet, and
// MkdirAll + WriteFile can land before the 07/ Create event is processed.
func TestDiscovery_WatchPicksUpNestedMkdirAll(t *testing.T) {
	tmp := t.TempDir()
	d, err := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewCodexSource(tmp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.watcher.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate Codex creating a brand-new month directory and writing a session
	// in one shot, as MkdirAll + WriteFile would.
	roll := filepath.Join(tmp, "2026", "07", "01",
		"rollout-2026-07-01T00-00-00-"+codexUUID+".jsonl")
	writeFile(t, roll, codexRollout)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.Get(codexUUID); ok {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("codex session in new month dir not discovered; list=%v", d.List())
}

func TestDiscovery_ListSorted(t *testing.T) {
	tmp := t.TempDir()
	older := filepath.Join(tmp, "-p", "older.jsonl")
	newer := filepath.Join(tmp, "-p", "newer.jsonl")
	// Distinct user-input timestamps: List sorts by LastInputAt (sortKey), so
	// the prompt times — not mtime — must differ for a deterministic order.
	writeJSONLAt(t, older, "older", "/tmp/p", "old", "2026-04-26T09:00:00.000Z")
	writeJSONLAt(t, newer, "newer", "/tmp/p", "new", "2026-04-26T11:00:00.000Z")

	d := newTestDiscovery(t, tmp)
	if err := d.scan(); err != nil {
		t.Fatal(err)
	}
	got := d.List()
	if len(got) != 2 || got[0].ID != "newer" {
		t.Fatalf("expected newer first; got %v", got)
	}
}
