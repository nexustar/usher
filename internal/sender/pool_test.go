package sender

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// fakeTmux models just enough tmux server state for pool tests: an ordered
// list of window names within the single "usher" session, plus a log of the
// inject-path commands. Guarded by mu because the Sender drives the runner
// from a goroutine while tests inspect cmds.
type fakeTmux struct {
	mu         sync.Mutex
	windows    []string
	exists     bool
	cmds       [][]string
	stdins     []string // stdin passed to each runStdin call
	failSpawn  bool     // when set, new-session/new-window return an error
	captureOut string   // canned capture-pane output
}

func (f *fakeTmux) runStdin(in string, args ...string) (string, error) {
	f.mu.Lock()
	f.stdins = append(f.stdins, in)
	f.mu.Unlock()
	return f.run(args...)
}

func (f *fakeTmux) run(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, args)
	switch args[0] {
	case "has-session":
		if f.exists {
			return "", nil
		}
		return "", errors.New("no session")
	case "new-session":
		if f.failSpawn {
			return "", errors.New("spawn failed")
		}
		f.exists = true
		f.addWindow(flagVal(args, "-n"))
		return "", nil
	case "new-window":
		if f.failSpawn {
			return "", errors.New("spawn failed")
		}
		f.addWindow(flagVal(args, "-n"))
		return "", nil
	case "list-windows":
		return strings.Join(f.windows, "\n"), nil
	case "kill-window":
		f.delWindow(idFromTarget(flagVal(args, "-t")))
		return "", nil
	case "kill-session":
		f.windows = nil
		f.exists = false
		return "", nil
	case "capture-pane":
		return f.captureOut, nil
	default: // set-window-option, set-buffer, paste-buffer, send-keys
		return "", nil
	}
}

func (f *fakeTmux) addWindow(name string) {
	if name != "" && !contains(f.windows, name) {
		f.windows = append(f.windows, name)
	}
}
func (f *fakeTmux) delWindow(name string) {
	for i, w := range f.windows {
		if w == name {
			f.windows = append(f.windows[:i], f.windows[i+1:]...)
			return
		}
	}
}

// countCmd returns how many recorded commands start with verb (thread-safe).
func (f *fakeTmux) countCmd(verb string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}

func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func idFromTarget(t string) string {
	if i := strings.IndexByte(t, ':'); i >= 0 {
		return t[i+1:]
	}
	return t
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// spawnCmd returns the claude command string from the most recent
// new-session/new-window invocation (it is the final argument tmux runs).
func (f *fakeTmux) spawnCmd(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.cmds) - 1; i >= 0; i-- {
		if c := f.cmds[i]; len(c) > 0 && (c[0] == "new-session" || c[0] == "new-window") {
			return c[len(c)-1]
		}
	}
	t.Fatal("no spawn command recorded")
	return ""
}

func TestPool_SpawnSetsModelOnNewSessionOnly(t *testing.T) {
	// Brand-new session (--session-id): --model is set.
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	if _, err := p.ensure("s1", "/tmp", "opus", false); err != nil {
		t.Fatal(err)
	}
	if cmd := f.spawnCmd(t); !strings.Contains(cmd, "--model 'opus'") {
		t.Fatalf("new session should carry --model 'opus', got: %s", cmd)
	}

	// Resume (--resume): --model is ignored, since claude keeps the session's
	// original model on resume.
	f2 := &fakeTmux{}
	p2 := newPool(f2, "claude", nil, nil, 8, quietLogger())
	if _, err := p2.ensure("s2", "/tmp", "opus", true); err != nil {
		t.Fatal(err)
	}
	if cmd := f2.spawnCmd(t); strings.Contains(cmd, "--model") {
		t.Fatalf("resume should not carry --model, got: %s", cmd)
	}

	// Empty model: no flag even on a new session.
	f3 := &fakeTmux{}
	p3 := newPool(f3, "claude", nil, nil, 8, quietLogger())
	if _, err := p3.ensure("s3", "/tmp", "", false); err != nil {
		t.Fatal(err)
	}
	if cmd := f3.spawnCmd(t); strings.Contains(cmd, "--model") {
		t.Fatalf("empty model should add no flag, got: %s", cmd)
	}
}

func TestPool_SpawnScrubsNestedClaudeEnv(t *testing.T) {
	// The window command must unset claude's per-session context markers
	// before exec'ing claude: with any of them inherited (usher or the tmux
	// server started from inside a claude session), the spawned claude runs
	// but silently persists nothing, blinding the jsonl tailer.
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	if _, err := p.ensure("s1", "/tmp", "", true); err != nil {
		t.Fatal(err)
	}
	cmd := f.spawnCmd(t)
	if !strings.HasPrefix(cmd, "env ") {
		t.Fatalf("spawn command should start with env -u scrub, got: %s", cmd)
	}
	for _, v := range []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID"} {
		if !strings.Contains(cmd, "-u "+v+" ") {
			t.Fatalf("spawn command should unset %s, got: %s", v, cmd)
		}
	}
	// The scrub must come BEFORE the claude invocation, not swallow it.
	if !strings.Contains(cmd, " 'claude' --resume 's1'") {
		t.Fatalf("claude invocation should follow the scrub, got: %s", cmd)
	}
}

func TestPool_EnsureSpawnsAndIsIdempotent(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())

	if _, err := p.ensure("s1", "/tmp", "", true); err != nil {
		t.Fatal(err)
	}
	if !p.has("s1") {
		t.Fatal("s1 should be live after ensure")
	}
	// Second ensure must not create a second window.
	if _, err := p.ensure("s1", "/tmp", "", true); err != nil {
		t.Fatal(err)
	}
	if len(f.windows) != 1 {
		t.Fatalf("expected 1 window, got %v", f.windows)
	}
}

func TestPool_KillRemovesWindowAndLRU(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())

	mustEnsure(t, p, "a")
	mustEnsure(t, p, "b")

	if err := p.kill("a"); err != nil {
		t.Fatal(err)
	}
	if p.has("a") {
		t.Fatal("a should be gone after kill")
	}
	if !p.has("b") {
		t.Fatal("kill must not touch other sessions")
	}
	if contains(p.lru, "a") {
		t.Fatalf("a should be dropped from lru, got %v", p.lru)
	}
	if f.countCmd("kill-window") != 1 {
		t.Fatalf("expected one kill-window, got %d", f.countCmd("kill-window"))
	}
}

func TestPool_KillUnknownSessionIsNoop(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())

	// No live window for "ghost": kill must not issue a kill-window (which would
	// error against tmux) and must not fail.
	if err := p.kill("ghost"); err != nil {
		t.Fatal(err)
	}
	if f.countCmd("kill-window") != 0 {
		t.Fatalf("expected no kill-window for an absent session, got %d", f.countCmd("kill-window"))
	}
}

func TestPool_LRUEviction(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 2, quietLogger())

	mustEnsure(t, p, "a")
	mustEnsure(t, p, "b")
	// Touch a so b is now the least-recently-used.
	mustEnsure(t, p, "a")
	// c exceeds cap (2) -> evict LRU, which is b.
	mustEnsure(t, p, "c")

	if p.has("b") {
		t.Fatal("b should have been evicted")
	}
	if !p.has("a") || !p.has("c") {
		t.Fatalf("a and c should be live, windows=%v", f.windows)
	}
	if len(f.windows) != 2 {
		t.Fatalf("cap is 2, got %v", f.windows)
	}
}

func TestPool_EvictionSkipsBusy(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 2, quietLogger())

	mustEnsure(t, p, "a")
	mustEnsure(t, p, "b")
	// a is the LRU (b was touched last), but a is mid-turn: eviction must skip
	// it and take b instead.
	busy := map[string]bool{"a": true}
	p.isBusy = func(id string) bool { return busy[id] }

	mustEnsure(t, p, "c")

	if !p.has("a") {
		t.Fatal("busy session a must not be evicted")
	}
	if p.has("b") {
		t.Fatal("b (next-oldest, idle) should have been evicted")
	}
	if !p.has("c") {
		t.Fatalf("c should be live, windows=%v", f.windows)
	}
}

func TestPool_AllBusySpawnsOverCap(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 2, quietLogger())

	mustEnsure(t, p, "a")
	mustEnsure(t, p, "b")
	// Both live sessions are mid-turn: nothing is evictable, so c spawns over
	// the cap rather than killing a running turn (soft limit).
	p.isBusy = func(id string) bool { return id == "a" || id == "b" }

	mustEnsure(t, p, "c")

	if !p.has("a") || !p.has("b") || !p.has("c") {
		t.Fatalf("all three should be live (over cap), windows=%v", f.windows)
	}
	if len(f.windows) != 3 {
		t.Fatalf("expected 3 windows over the cap of 2, got %v", f.windows)
	}
}

func TestPool_AdoptExistingWindows(t *testing.T) {
	f := &fakeTmux{exists: true, windows: []string{"old1", "old2"}}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	if !p.has("old1") || !p.has("old2") {
		t.Fatal("adopt should pick up pre-existing windows")
	}
	if len(p.lru) != 2 {
		t.Fatalf("lru should be seeded with 2, got %v", p.lru)
	}
}

func TestPool_InjectUsesBracketedPaste(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	mustEnsure(t, p, "s1")
	f.cmds = nil
	if err := p.inject("s1", "hello\nworld"); err != nil {
		t.Fatal(err)
	}
	var sawLoad, sawPaste, sawEnter bool
	for _, c := range f.cmds {
		// The prompt must be loaded via stdin (load-buffer -), never as a
		// set-buffer argument — a long paste there is "command too long".
		if c[0] == "set-buffer" {
			t.Fatalf("inject must not pass the prompt as a set-buffer arg; cmds=%v", f.cmds)
		}
		if c[0] == "load-buffer" && contains(c, "-") {
			sawLoad = true
		}
		if c[0] == "paste-buffer" && contains(c, "-p") {
			sawPaste = true
		}
		if c[0] == "send-keys" && contains(c, "Enter") {
			sawEnter = true
		}
	}
	if !sawLoad || !sawPaste || !sawEnter {
		t.Fatalf("inject should load-buffer (stdin), bracketed-paste, then Enter; cmds=%v", f.cmds)
	}
	if !contains(f.stdins, "hello\nworld") {
		t.Fatalf("inject should feed the prompt via stdin; stdins=%v", f.stdins)
	}
}

func TestPool_SpawnPropagatesEnv(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, []string{"USHER_HOOK_SOCK=/tmp/x.sock"}, 8, quietLogger())
	mustEnsure(t, p, "s1")
	// The new-session command must carry -e USHER_HOOK_SOCK=... so the
	// spawned claude's permission hooks route back to this instance.
	var ok bool
	f.mu.Lock()
	for _, c := range f.cmds {
		if len(c) == 0 || c[0] != "new-session" {
			continue
		}
		for i, a := range c {
			if a == "-e" && i+1 < len(c) && c[i+1] == "USHER_HOOK_SOCK=/tmp/x.sock" {
				ok = true
			}
		}
	}
	f.mu.Unlock()
	if !ok {
		t.Fatalf("spawn should pass -e USHER_HOOK_SOCK; cmds=%v", f.cmds)
	}
}

func TestPool_CapturePaneTargetsWindow(t *testing.T) {
	f := &fakeTmux{captureOut: "\x1b[7mselected\x1b[0m row\n"}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	mustEnsure(t, p, "s1")
	f.cmds = nil

	out, err := p.capturePane("s1")
	if err != nil {
		t.Fatal(err)
	}
	if out != "\x1b[7mselected\x1b[0m row\n" {
		t.Fatalf("capturePane should return the frame verbatim (escapes intact); got %q", out)
	}
	// Must use -e (colour escapes, for the selection highlight) and target the
	// session's window.
	var c []string
	for _, cmd := range f.cmds {
		if len(cmd) > 0 && cmd[0] == "capture-pane" {
			c = cmd
		}
	}
	if c == nil {
		t.Fatalf("expected a capture-pane command; cmds=%v", f.cmds)
	}
	if !contains(c, "-e") || flagVal(c, "-t") != "usher:s1" {
		t.Fatalf("capture-pane should pass -e and -t usher:s1; got %v", c)
	}
}

func TestPool_SendKeysForwardsNames(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	mustEnsure(t, p, "s1")
	f.cmds = nil

	if err := p.sendKeys("s1", "Up"); err != nil {
		t.Fatal(err)
	}
	var c []string
	for _, cmd := range f.cmds {
		if len(cmd) > 0 && cmd[0] == "send-keys" {
			c = cmd
		}
	}
	if c == nil {
		t.Fatalf("expected a send-keys command; cmds=%v", f.cmds)
	}
	// The key name must be forwarded as a bare argument (not -l literal text),
	// targeting the session window.
	if flagVal(c, "-t") != "usher:s1" || !contains(c, "Up") || contains(c, "-l") {
		t.Fatalf("send-keys should forward the key name to usher:s1; got %v", c)
	}
}

func TestPool_ResizeCanvasSetsColsAndRestoresLatest(t *testing.T) {
	f := &fakeTmux{}
	p := newPool(f, "claude", nil, nil, 8, quietLogger())
	mustEnsure(t, p, "s1")
	f.cmds = nil

	if err := p.resizeCanvas("s1", 120, 40); err != nil {
		t.Fatal(err)
	}
	var resize, restore []string
	for _, c := range f.cmds {
		if len(c) == 0 {
			continue
		}
		if c[0] == "resize-window" {
			resize = c
		}
		if c[0] == "set-option" && contains(c, "window-size") {
			restore = c
		}
	}
	// Must resize the window to the requested cols × rows…
	if resize == nil || flagVal(resize, "-t") != "usher:s1" ||
		flagVal(resize, "-x") != "120" || flagVal(resize, "-y") != "40" {
		t.Fatalf("resizeCanvas should resize-window usher:s1 to 120x40; got %v", resize)
	}
	// …then restore window-size to latest so a later manual attach stays
	// full-size (resize-window flips the window to manual).
	if restore == nil || !contains(restore, "latest") {
		t.Fatalf("resizeCanvas should restore window-size latest; got %v", restore)
	}
}

func mustEnsure(t *testing.T, p *pool, id string) {
	t.Helper()
	if _, err := p.ensure(id, "/tmp", "", true); err != nil {
		t.Fatal(err)
	}
}
