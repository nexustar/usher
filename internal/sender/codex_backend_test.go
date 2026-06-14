package sender

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newCodexBackend(sessionsDir string, extra ...string) codexBackend {
	return codexBackend{codexCmd: "codex", sessionsDir: sessionsDir, extraArgs: extra}
}

// testCodexSender builds a Sender wired to a codexBackend over a fake tmux, with
// fast timings — the Codex analog of testSender.
func testCodexSender(runner tmuxRunner, sessionsDir string) *Sender {
	tm := timing{
		spawnSettle:   10 * time.Millisecond,
		trustToInject: 5 * time.Millisecond,
		warmSettle:    5 * time.Millisecond,
		resumeReady:   200 * time.Millisecond,
		confirm:       1 * time.Second,
		poll:          10 * time.Millisecond,
	}
	p := newPool(runner, "codex", nil, nil, 8, quietLogger())
	b := codexBackend{p: p, t: tm, codexCmd: "codex", sessionsDir: sessionsDir}
	p.spawnOverride = b.spawnCommand
	return &Sender{
		pool:        p,
		backend:     b,
		projectsDir: sessionsDir,
		logger:      quietLogger(),
		t:           tm,
		tail:        tailConfig{poll: 10 * time.Millisecond, appearWait: 2 * time.Second, turnComplete: b.turnComplete},
	}
}

// codexTurnLines are real-shaped rollout lines for one turn: the user prompt, an
// agent reply, and the task_complete end-of-turn marker.
var codexTurnLines = []string{
	`{"timestamp":"2026-06-14T00:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"hi"}}`,
	`{"timestamp":"2026-06-14T00:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"hello","phase":"final_answer"}}`,
	`{"timestamp":"2026-06-14T00:00:09Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
}

func TestCodexSend_ResumeStreamsTurn(t *testing.T) {
	root := t.TempDir()
	id := "019ec1ab-6a76-7e32-9499-040331a92a4b"
	path := writeRollout(t, root, id, "/work", time.Now())
	// Pane already at the composer (banner + cwd footer): waitReady returns at once.
	f := &fakeTmux{captureOut: codexBannerMarker + "0.139.0)\n  gpt-5.5 default · /work\n"}
	s := testCodexSender(f, root)

	ch, err := s.Send(context.Background(), id, "hi", "/work")
	if err != nil {
		t.Fatal(err)
	}
	// Append the turn only after the prompt is pasted, so the pre-inject offset is
	// captured first (matches the Claude resume test's ordering).
	go func() {
		for f.countCmd("paste-buffer") < 1 {
			time.Sleep(2 * time.Millisecond)
		}
		appendLines(path, 10*time.Millisecond, codexTurnLines...)
	}()

	got := collect(t, ch, 6*time.Second)
	// task_complete ends the turn (it is not itself streamed); the two event_msg
	// lines before it are.
	want := []string{"subprocess.started", "event_msg", "event_msg", "subprocess.exit"}
	if !eq(types(got), want) {
		t.Fatalf("got %v, want %v", types(got), want)
	}
	// Resume launched `codex resume <id>` — no chooser, so no Down is sent.
	if !cmdMatches(f, "new-session", "resume") {
		t.Fatalf("resume should spawn `codex resume`; cmds=%v", f.cmds)
	}
	if cmdMatches(f, "send-keys", "Down") {
		t.Error("codex resume has no chooser; no Down should be sent")
	}
}

func TestNewCodexWiring(t *testing.T) {
	s := NewCodex("codex", "/home/u/.codex/sessions", "", "", []string{"--sandbox", "workspace-write"}, 8, quietLogger())
	if s.pool.spawnOverride == nil {
		t.Error("NewCodex must route spawn through codexBackend.spawnCommand")
	}
	if s.backend.preAssignsID() {
		t.Error("codex backend must not pre-assign ids")
	}
	cmd := s.backend.spawnCommand("x", "/c", "", true)
	if !strings.Contains(cmd, "resume 'x'") || !strings.Contains(cmd, "codex") {
		t.Errorf("unexpected resume command: %q", cmd)
	}
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

func TestCodexKnownSessionIDs(t *testing.T) {
	root := t.TempDir()
	writeRollout(t, root, "aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", "/c", time.Now())
	writeRollout(t, root, "bbbb2222-cccc-dddd-eeee-ffffffffffff", "/c", time.Now())
	known := newCodexBackend(root).knownSessionIDs()
	if len(known) != 2 ||
		!known["aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"] ||
		!known["bbbb2222-cccc-dddd-eeee-ffffffffffff"] {
		t.Fatalf("knownSessionIDs = %v", known)
	}
}

func TestPoolRenameUpdatesWindowAndLRU(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "codex", nil, nil, 8, quietLogger())
	p.lru = []string{"other", "temp"}
	if err := p.rename("temp", "real-id"); err != nil {
		t.Fatal(err)
	}
	if !cmdMatches(f, "rename-window", "real-id") {
		t.Errorf("expected rename-window to real-id; cmds=%v", f.cmds)
	}
	if len(p.lru) != 2 || p.lru[1] != "real-id" || p.lru[0] != "other" {
		t.Errorf("LRU not re-keyed: %v", p.lru)
	}
}

func TestPreAssignsID(t *testing.T) {
	cx := testCodexSender(&fakeTmux{}, t.TempDir())
	if cx.PreAssignsID() {
		t.Error("codex sender must not pre-assign ids")
	}
	cl, _ := testSender(t, &fakeTmux{}, "x")
	if !cl.PreAssignsID() {
		t.Error("claude sender must pre-assign ids")
	}
}

func TestLockCwdSerializesSameCwdNotDifferent(t *testing.T) {
	s := &Sender{}
	release := s.lockCwd("/a") // hold /a

	got := make(chan string, 2)
	go func() { u := s.lockCwd("/a"); got <- "a"; u() }() // must block until release
	go func() { u := s.lockCwd("/b"); got <- "b"; u() }() // different cwd: must proceed

	// While /a is held, only the /b goroutine can make progress.
	if first := <-got; first != "b" {
		t.Fatalf("a different cwd must not block; got %q first", first)
	}
	release()
	if second := <-got; second != "a" {
		t.Fatalf("same cwd must proceed only after release; got %q", second)
	}
}
