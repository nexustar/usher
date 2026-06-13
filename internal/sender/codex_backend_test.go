package sender

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newCodexBackend(sessionsDir string, extra ...string) codexBackend {
	return codexBackend{codexCmd: "codex", sessionsDir: sessionsDir, extraArgs: extra}
}

func TestCodexSpawnCommand(t *testing.T) {
	b := newCodexBackend("/sessions", "--sandbox", "workspace-write")

	// New session: no id flag (Codex generates its own), model via -c override.
	got := b.spawnCommand("ignored-id", "/tmp/p", "gpt-5.5", false)
	if strings.Contains(got, "ignored-id") {
		t.Errorf("new spawn must not pass a session id, got %q", got)
	}
	if !strings.Contains(got, "-c 'model=gpt-5.5'") {
		t.Errorf("new spawn missing model override: %q", got)
	}
	if strings.Contains(got, "resume") {
		t.Errorf("new spawn should not be a resume: %q", got)
	}
	if !strings.Contains(got, "'--sandbox' 'workspace-write'") {
		t.Errorf("extra args missing: %q", got)
	}
	if !strings.HasPrefix(got, "env -u CODEX_THREAD_ID") {
		t.Errorf("env scrub prefix missing: %q", got)
	}

	// Resume: `codex resume <id>`, no model (resumed keeps its own).
	got = b.spawnCommand("sess-123", "/tmp/p", "gpt-5.5", true)
	if !strings.Contains(got, "resume 'sess-123'") {
		t.Errorf("resume command malformed: %q", got)
	}
	if strings.Contains(got, "model=") {
		t.Errorf("resume must not set model: %q", got)
	}
}

func TestCodexPreAssignsID(t *testing.T) {
	if newCodexBackend("/s").preAssignsID() {
		t.Error("codex generates its own id; preAssignsID must be false")
	}
}

// writeRollout creates <root>/2026/06/14/rollout-<ts>-<id>.jsonl with a
// session_meta header carrying cwd, at the given mtime.
func writeRollout(t *testing.T, root, id, cwd string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "06", "14")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-06-14T00-00-00-"+id+".jsonl")
	line := `{"timestamp":"2026-06-14T00:00:00Z","type":"session_meta","payload":{"id":"` +
		id + `","cwd":"` + cwd + `","timestamp":"2026-06-14T00:00:00Z"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexLocate(t *testing.T) {
	root := t.TempDir()
	want := writeRollout(t, root, "aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", "/tmp/p", time.Now())
	b := newCodexBackend(root)

	if got := b.locate("aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"); got != want {
		t.Errorf("locate = %q, want %q", got, want)
	}
	if got := b.locate("ffff0000-0000-0000-0000-000000000000"); got != "" {
		t.Errorf("locate of unknown id = %q, want empty", got)
	}
}

func TestCodexDiscoverNewID(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := "11111111-1111-1111-1111-111111111111"
	fresh := "22222222-2222-2222-2222-222222222222"
	other := "33333333-3333-3333-3333-333333333333"
	writeRollout(t, root, old, "/tmp/p", now.Add(-2*time.Hour))    // known
	writeRollout(t, root, fresh, "/tmp/p", now)                    // the one we want
	writeRollout(t, root, other, "/tmp/other", now.Add(time.Hour)) // newer but wrong cwd

	b := newCodexBackend(root)
	known := map[string]bool{old: true}

	if got := b.discoverNewID("/tmp/p", known); got != fresh {
		t.Errorf("discoverNewID = %q, want %q (newest unknown rollout in cwd)", got, fresh)
	}
	// Once fresh is also known, nothing new remains for that cwd.
	known[fresh] = true
	if got := b.discoverNewID("/tmp/p", known); got != "" {
		t.Errorf("discoverNewID after all known = %q, want empty", got)
	}
}
