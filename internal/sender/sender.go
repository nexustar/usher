// Package sender drives one long-lived interactive `claude` process per
// session — spawned in a window of a dedicated tmux server socket — and
// reports each turn's output by tailing the session's jsonl file.
//
// This replaces the original headless `claude -p --resume` design: `-p` is
// moving to metered billing, whereas interactive claude runs under the user's
// Claude subscription. Discovery and transcript reads stay jsonl-based; only
// the send path changed. See the pool (process lifecycle) and tailTurn (turn
// streaming + end-of-turn detection) for the two halves.
//
// Concurrency note: usher keeps ONE process per session and serializes sends
// at the router. If the user also has the same session open elsewhere (their
// IDE), interactive resume forks the jsonl into a tree — a known corner case
// usher does not handle in v0.x (linear tail only).
package sender

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// StreamEvent is one event for a turn. Type is the jsonl line's "type"
// (e.g. "user", "assistant", "system"), or one of the synthesized values
// "subprocess.started" / "subprocess.exit" / "error". The names are kept from
// the headless era so the broker/web layer needs minimal change; the payloads
// are now whole jsonl lines (message granularity), not stream-json token
// deltas.
type StreamEvent struct {
	Type string
	Raw  json.RawMessage
}

// timing groups the tunable delays for driving the TUI. Defaults are set in
// New; tests override them for speed.
type timing struct {
	spawnSettle   time.Duration // after spawning a fresh window, before first keystroke (claude boot)
	trustToInject time.Duration // after the trust-accept Enter, before pasting the prompt
	warmSettle    time.Duration // before pasting into an already-running window
	resumeReady   time.Duration // max wait for a fresh resume's input box (incl. answering the resume chooser)
	confirm       time.Duration // how long to wait for a brand-new session's jsonl to appear
	poll          time.Duration // pane/file poll interval
}

type Sender struct {
	pool        *pool
	backend     backend
	projectsDir string
	logger      *slog.Logger
	t           timing
	tail        tailConfig

	// busy holds session ids with an in-flight turn. The pool consults it
	// (via isBusy) so LRU eviction never kills a session mid-turn. Marked
	// synchronously in run() before the turn goroutine starts and cleared when
	// that goroutine exits (any path), so the flag can't leak.
	busyMu sync.Mutex
	busy   map[string]struct{}

	// cwdLocks serialize new Codex-session creation per cwd so two concurrent
	// same-cwd creates can't mis-identify each other's rollout in discoverNewID.
	cwdLocksMu sync.Mutex
	cwdLocks   map[string]*sync.Mutex
}

// lockCwd acquires the per-cwd creation lock and returns its unlock func.
func (s *Sender) lockCwd(cwd string) func() {
	s.cwdLocksMu.Lock()
	if s.cwdLocks == nil {
		s.cwdLocks = map[string]*sync.Mutex{}
	}
	m := s.cwdLocks[cwd]
	if m == nil {
		m = &sync.Mutex{}
		s.cwdLocks[cwd] = m
	}
	s.cwdLocksMu.Unlock()
	m.Lock()
	return m.Unlock
}

// New builds a Sender. claudeCmd is the claude binary; permissionMode (if
// non-empty) is passed through as --permission-mode; projectsDir is Claude
// Code's projects root (used to locate session jsonl files by their globally
// unique id); socket is the dedicated tmux server socket name; hookSock, if
// non-empty, is set as USHER_HOOK_SOCK on spawned claude processes so their
// permission hooks route back to this instance; maxLive caps concurrent live
// processes (LRU-evicted beyond it).
func New(claudeCmd, permissionMode, projectsDir, socket, hookSock string, maxLive int, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	if socket == "" {
		socket = tmuxSessionName
	}
	var extra []string
	if permissionMode != "" {
		extra = []string{"--permission-mode", permissionMode}
	}
	// Suppress claude's post-turn feedback survey: left on the pane it eats the
	// next send's keystrokes. Disabling at spawn beats per-send detection.
	env := []string{"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1"}
	if hookSock != "" {
		env = append(env, "USHER_HOOK_SOCK="+hookSock)
	}
	runner := execRunner{bin: "tmux", socket: socket}
	t := timing{
		spawnSettle:   5 * time.Second,
		trustToInject: 1500 * time.Millisecond,
		warmSettle:    400 * time.Millisecond,
		resumeReady:   30 * time.Second,
		confirm:       8 * time.Second,
		poll:          150 * time.Millisecond,
	}
	p := newPool(runner, claudeCmd, extra, env, maxLive, logger)
	b := claudeBackend{p: p, t: t, projectsDir: projectsDir, claudeCmd: claudeCmd, extraArgs: extra}
	s := &Sender{
		pool:        p,
		backend:     b,
		projectsDir: projectsDir,
		logger:      logger,
		busy:        make(map[string]struct{}),
		t:           t,
		tail:        tailConfig{poll: 150 * time.Millisecond, appearWait: 20 * time.Second, turnComplete: b.turnComplete},
	}
	s.pool.isBusy = s.isBusy
	return s
}

// NewCodex builds a Sender that drives interactive `codex` instead of `claude`.
// codexCmd is the codex binary; sessionsDir is ~/.codex/sessions (the rollout
// root, used to locate logs); sandboxArgs are extra codex flags (e.g.
// --sandbox workspace-write); hookSock, if set, routes the codex permission hook
// back to this instance; maxLive caps live processes.
//
// Resume goes straight in (`codex resume <id>`, no chooser); a brand-new session
// (Codex assigns its own id) is created via StartCodexSession.
func NewCodex(codexCmd, sessionsDir, socket, hookSock string, sandboxArgs []string, maxLive int, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	if socket == "" {
		socket = tmuxSessionName
	}
	var env []string
	if hookSock != "" {
		env = append(env, "USHER_HOOK_SOCK="+hookSock)
	}
	runner := execRunner{bin: "tmux", socket: socket}
	t := timing{
		spawnSettle:   5 * time.Second,
		trustToInject: 1500 * time.Millisecond,
		warmSettle:    400 * time.Millisecond,
		resumeReady:   30 * time.Second,
		confirm:       8 * time.Second,
		poll:          150 * time.Millisecond,
	}
	p := newPool(runner, codexCmd, nil, env, maxLive, logger)
	b := codexBackend{p: p, t: t, codexCmd: codexCmd, sessionsDir: sessionsDir, extraArgs: sandboxArgs}
	// Codex's command differs from the Claude default; route spawn through it.
	p.spawnOverride = b.spawnCommand
	s := &Sender{
		pool:        p,
		backend:     b,
		projectsDir: sessionsDir,
		logger:      logger,
		busy:        make(map[string]struct{}),
		t:           t,
		tail:        tailConfig{poll: 150 * time.Millisecond, appearWait: 20 * time.Second, turnComplete: b.turnComplete},
	}
	s.pool.isBusy = s.isBusy
	return s
}

// isBusy reports whether sessionID has an in-flight turn (the pool's eviction
// guard). markBusy/clearBusy bracket a tracked turn in run().
func (s *Sender) isBusy(sessionID string) bool {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	_, ok := s.busy[sessionID]
	return ok
}

func (s *Sender) markBusy(sessionID string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busy == nil {
		s.busy = make(map[string]struct{})
	}
	s.busy[sessionID] = struct{}{}
}

func (s *Sender) clearBusy(sessionID string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	delete(s.busy, sessionID)
}

// Send injects prompt into the session's live interactive claude (resuming /
// spawning it as needed) and streams the resulting turn's events. The channel
// closes when the turn ends or ctx is cancelled.
func (s *Sender) Send(ctx context.Context, sessionID, prompt, cwd string) (<-chan StreamEvent, error) {
	// Resumes keep their original model, so no model is threaded here.
	return s.run(ctx, sessionID, prompt, cwd, "", true)
}

// SendNew is like Send but starts a brand-new session with the given id
// (`--session-id`). The jsonl is created lazily once claude writes the first
// turn; the tailer waits for it to appear.
func (s *Sender) SendNew(ctx context.Context, sessionID, prompt, cwd, model string) (<-chan StreamEvent, error) {
	return s.run(ctx, sessionID, prompt, cwd, model, false)
}

// PreAssignsID reports whether usher picks a new session's id up front (Claude,
// via --session-id) or the backend assigns its own to be discovered after spawn
// (Codex). The router uses it to choose the new-session path.
func (s *Sender) PreAssignsID() bool { return s.backend.preAssignsID() }

// StartCodexSession spawns a brand-new session whose id the backend assigns
// itself (Codex has no --session-id flag). It spawns under the temporary window
// handle tempID, gets the TUI ready, injects prompt, discovers the id the
// backend just wrote, renames the window to it, and returns that real id with
// the turn's event stream. It blocks only until the id is known (the session log
// is flushed at start, so this is quick); the turn then streams in the returned
// channel. Callers gate on PreAssignsID()==false.
func (s *Sender) StartCodexSession(ctx context.Context, tempID, prompt, cwd, model string, discoverTimeout time.Duration) (string, <-chan StreamEvent, error) {
	// Serialize same-cwd creates across snapshot→spawn→discover (released on
	// return, before the tail goroutine, which runs unlocked).
	defer s.lockCwd(cwd)()

	known := s.backend.knownSessionIDs()
	fresh, err := s.pool.ensure(tempID, cwd, model, false)
	if err != nil {
		return "", nil, err
	}
	s.markBusy(tempID)

	if !s.backend.waitReady(ctx, tempID, cwd, fresh, false) {
		s.clearBusy(tempID)
		return "", nil, ctx.Err()
	}
	if err := s.pool.inject(tempID, prompt); err != nil {
		s.clearBusy(tempID)
		return "", nil, err
	}

	realID := s.discoverWait(ctx, cwd, known, discoverTimeout)
	if realID == "" {
		s.clearBusy(tempID)
		return "", nil, errors.New("codex did not create a session log after the prompt")
	}
	if err := s.pool.rename(tempID, realID); err != nil {
		s.logger.Warn("rename codex window", "from", tempID, "to", realID, "err", err)
	}
	// The window is now named realID; move the eviction guard with it.
	s.clearBusy(tempID)
	s.markBusy(realID)

	path := s.backend.locate(realID)
	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		defer s.clearBusy(realID)
		started, _ := json.Marshal(struct {
			Cwd   string `json:"cwd"`
			Fresh bool   `json:"fresh"`
		}{cwd, true})
		if !sendEvent(ctx, out, StreamEvent{Type: "subprocess.started", Raw: started}) {
			return
		}
		if path == "" {
			emitError(ctx, out, "codex session log not found: "+realID)
			return
		}
		// Offset 0: a brand-new rollout — tail the whole first turn from the top.
		for ev := range tailTurn(ctx, path, 0, s.logger, s.tail) {
			if !sendEvent(ctx, out, ev) {
				return
			}
		}
	}()
	return realID, out, nil
}

// discoverWait polls the backend for the id of the session just spawned in cwd
// (the newest log not in the pre-spawn snapshot), until it appears or the
// deadline/ctx fires.
func (s *Sender) discoverWait(ctx context.Context, cwd string, known map[string]bool, timeout time.Duration) string {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(s.t.poll)
	defer ticker.Stop()
	for {
		if id := s.backend.discoverNewID(cwd, known); id != "" {
			return id
		}
		select {
		case <-ctx.Done():
			return ""
		case <-deadline.C:
			return ""
		case <-ticker.C:
		}
	}
}

// Inject readies the session's window (resuming if cold) and pastes prompt
// without tailing a turn — for input that drives the TUI directly instead of
// starting a model turn (a leading '!', claude's own bash mode), which logs no
// turn_duration and would otherwise wedge the tailer. Never starts a session.
func (s *Sender) Inject(ctx context.Context, sessionID, prompt, cwd string) error {
	fresh, err := s.pool.ensure(sessionID, cwd, "", true)
	if err != nil {
		return err
	}
	if !s.backend.waitReady(ctx, sessionID, cwd, fresh, true) {
		return ctx.Err()
	}
	return s.pool.inject(sessionID, prompt)
}

// Has reports whether usher currently holds a live interactive process for
// sessionID.
func (s *Sender) Has(sessionID string) bool { return s.pool.has(sessionID) }

// LiveSessions returns the ids of all sessions usher currently holds a live
// interactive process for. One tmux query; use it to decorate session lists.
func (s *Sender) LiveSessions() []string { return s.pool.liveSessions() }

// Interrupt stops the in-flight turn for sessionID without killing the
// process (Ctrl-C into the pane).
func (s *Sender) Interrupt(sessionID string) error { return s.pool.interrupt(sessionID) }

// Kill tears down usher's live window for sessionID, if any (its claude
// exits). Used when deleting a session; a no-op when nothing is live.
func (s *Sender) Kill(sessionID string) error { return s.pool.kill(sessionID) }

// CapturePane returns the current rendered contents (with colour escapes) of
// the session's interactive pane, for the read-only terminal mirror. Errors
// if usher holds no live window for sessionID.
func (s *Sender) CapturePane(sessionID string) (string, error) {
	return s.pool.capturePane(sessionID)
}

// SendKeys forwards tmux key names to the session's pane, powering the
// terminal mirror's soft keys.
func (s *Sender) SendKeys(sessionID string, keys ...string) error {
	return s.pool.sendKeys(sessionID, keys...)
}

// ResizeCanvas sets the session pane to cols×rows for the terminal mirror (also
// repairs any manual-attach drift). Called when the mirror opens.
func (s *Sender) ResizeCanvas(sessionID string, cols, rows int) error {
	return s.pool.resizeCanvas(sessionID, cols, rows)
}

// Shutdown tears down usher's tmux server (all live windows). Call on exit if
// you do NOT want processes to survive for the next usher run.
func (s *Sender) Shutdown() { s.pool.shutdown() }

func (s *Sender) run(ctx context.Context, sessionID, prompt, cwd, model string, resume bool) (<-chan StreamEvent, error) {
	fresh, err := s.pool.ensure(sessionID, cwd, model, resume)
	if err != nil {
		return nil, err
	}

	// Protect this session from LRU eviction for the duration of the turn.
	// Marked here (not inside the goroutine) so a concurrent ensure for another
	// session can't pick it as a victim in the gap before the goroutine runs.
	s.markBusy(sessionID)

	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		defer s.clearBusy(sessionID)

		started, _ := json.Marshal(struct {
			Cwd   string `json:"cwd"`
			Fresh bool   `json:"fresh"`
		}{cwd, fresh})
		if !sendEvent(ctx, out, StreamEvent{Type: "subprocess.started", Raw: started}) {
			return
		}

		// Get the TUI ready to receive the prompt.
		if !s.backend.waitReady(ctx, sessionID, cwd, fresh, resume) {
			return
		}

		// Resolve the jsonl path and capture the pre-inject size so the tailer
		// reports only this turn. For a brand-new session the file may not
		// exist yet (created on first write); offset stays 0 and we resolve
		// the path after injecting.
		path := s.locate(sessionID)
		var offset int64
		if path != "" {
			if fi, statErr := os.Stat(path); statErr == nil {
				offset = fi.Size()
			}
		}

		// Inject once, then tail — no re-inject-on-miss: the old "did a user line
		// appear in time" oracle false-negatived on a slow flush and
		// double-submitted. A missed paste now shows in the live mirror instead.
		if err := s.pool.inject(sessionID, prompt); err != nil {
			emitError(ctx, out, "inject prompt: "+err.Error())
			return
		}
		if path == "" {
			// Brand-new session: jsonl is created on first write; resolve its
			// path now so the tailer has one (discovery, not a retry).
			if path = s.locateWait(ctx, sessionID, s.t.confirm); path == "" {
				emitError(ctx, out, "session jsonl did not appear after prompt")
				return
			}
		}

		for ev := range tailTurn(ctx, path, offset, s.logger, s.tail) {
			if !sendEvent(ctx, out, ev) {
				return
			}
		}
	}()

	return out, nil
}

// Markers for matching TUI states in a plain pane capture: the resume chooser's
// two option lines, the idle input box's footer, and the chooser's arrow.
const (
	resumeChooserMarker = "Resume full session as-is" // the option usher wants
	resumeSummaryMarker = "Resume from summary"       // the highlighted default
	inputReadyMarker    = "? for shortcuts"
	chooserArrow        = "❯"
)

// chooserArrowOn reports whether the chooser's selection arrow sits on `option`
// — arrow and option text on the SAME line. A loose Contains(option) also
// matches that string replayed in a transcript, which fired keys into the
// prompt box during the boot frames before the input footer rendered.
func chooserArrowOn(text, option string) bool {
	for _, ln := range strings.Split(text, "\n") {
		if strings.Contains(ln, chooserArrow) && strings.Contains(ln, option) {
			return true
		}
	}
	return false
}

// locate finds the session log for sessionID via the active backend.
func (s *Sender) locate(sessionID string) string {
	return s.backend.locate(sessionID)
}

// locateWait polls locate until the file appears or timeout/ctx fires.
func (s *Sender) locateWait(ctx context.Context, sessionID string, timeout time.Duration) string {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(s.t.poll)
	defer ticker.Stop()
	for {
		if p := s.locate(sessionID); p != "" {
			return p
		}
		select {
		case <-ctx.Done():
			return ""
		case <-deadline.C:
			return ""
		case <-ticker.C:
		}
	}
}

// sendEvent delivers ev unless ctx is cancelled. Returns true if delivered.
func sendEvent(ctx context.Context, ch chan<- StreamEvent, ev StreamEvent) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func emitError(ctx context.Context, out chan<- StreamEvent, msg string) {
	raw, _ := json.Marshal(map[string]string{"message": msg})
	sendEvent(ctx, out, StreamEvent{Type: "error", Raw: raw})
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
