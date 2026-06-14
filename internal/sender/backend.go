package sender

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"usher/internal/codexrollout"
)

// backend abstracts the per-CLI differences in driving an interactive coding
// agent inside a tmux window. The pool (window lifecycle, inject, capture) and
// the Sender (busy-tracking, streaming) are otherwise backend-agnostic; a
// backend answers only: how to spawn/resume the process, where its session log
// lives, whether usher chooses the new-session id or discovers it, how a turn
// ends, and how to get the freshly-spawned TUI ready for a prompt.
//
// claudeBackend (the existing behavior) is introduced when the Sender is wired
// to delegate; codexBackend below is the first concrete implementation.
type backend interface {
	// spawnCommand is the shell command to run in the tmux window for a new or
	// resumed session (env-unset prefix included).
	spawnCommand(sessionID, cwd, model string, resume bool) string
	// preAssignsID reports whether usher picks the new session's id up front
	// (Claude `--session-id`) or the backend generates it, to be discovered
	// after spawn via discoverNewID (Codex).
	preAssignsID() bool
	// locate finds the on-disk session log for sessionID, or "".
	locate(sessionID string) string
	// discoverNewID returns the id of a session just spawned in cwd — the newest
	// log under the backend's root whose cwd matches and whose id is not in
	// known. Only meaningful when preAssignsID is false. "" if none yet.
	discoverNewID(cwd string, known map[string]bool) string
	// knownSessionIDs snapshots the ids of all existing session logs, taken just
	// before a !preAssignsID spawn so discoverNewID can tell the new one apart.
	knownSessionIDs() map[string]bool
	// turnComplete is the tailer's end-of-turn predicate for this backend's log.
	turnComplete(line []byte) bool
	// waitReady prepares the freshly-spawned/resumed TUI to accept a pasted
	// prompt. Returns false only on ctx cancellation.
	waitReady(ctx context.Context, sessionID, cwd string, fresh, resume bool) bool
}

// --- Claude --------------------------------------------------------------

var _ backend = claudeBackend{}

// claudeBackend drives interactive `claude`, the original behavior, now behind
// the backend interface. usher pre-assigns the session id (--session-id), the
// log is a flat <projectsDir>/<cwd>/<id>.jsonl, the turn ends on
// system/turn_duration, and the TUI may show a "trust this folder" prompt and a
// long-resume chooser that must be answered toward "full session as-is".
type claudeBackend struct {
	p           *pool
	t           timing
	projectsDir string
	claudeCmd   string
	extraArgs   []string
}

func (b claudeBackend) preAssignsID() bool            { return true }
func (b claudeBackend) turnComplete(line []byte) bool { return isTurnComplete(line) }

// discoverNewID / knownSessionIDs are unused for Claude (usher assigns the id
// up front via --session-id).
func (b claudeBackend) discoverNewID(cwd string, known map[string]bool) string { return "" }
func (b claudeBackend) knownSessionIDs() map[string]bool                       { return nil }

func (b claudeBackend) spawnCommand(sessionID, cwd, model string, resume bool) string {
	return claudeSpawnCommand(b.claudeCmd, b.extraArgs, sessionID, cwd, model, resume)
}

// locate finds the session jsonl by its globally unique id, sidestepping the
// ambiguous cwd<->dir mapping (a cwd may legitimately contain '-'). "" if absent.
func (b claudeBackend) locate(sessionID string) string {
	matches, err := filepath.Glob(filepath.Join(b.projectsDir, "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// waitReady prepares the TUI to receive a pasted prompt: a fresh resume answers
// the long-session chooser, a fresh new window dismisses the trust prompt, a
// warm window just needs a beat. cwd is unused (the markers are global). Returns
// false on ctx cancel.
func (b claudeBackend) waitReady(ctx context.Context, sessionID, cwd string, fresh, resume bool) bool {
	switch {
	case fresh && resume:
		return b.waitResumeReady(ctx, sessionID)
	case fresh:
		if !sleepCtx(ctx, b.t.spawnSettle) {
			return false
		}
		_ = b.p.acceptTrust(sessionID)
		return sleepCtx(ctx, b.t.trustToInject)
	default:
		return sleepCtx(ctx, b.t.warmSettle)
	}
}

// waitResumeReady answers the long-resume chooser ("full session") and waits for
// the input box, tracking the selection arrow each tick: claude swallows keys
// aimed at the select before it mounts, so a swallowed Down must self-retry (the
// arrow hasn't moved). Bounded by t.resumeReady; false only on ctx cancel.
func (b claudeBackend) waitResumeReady(ctx context.Context, sessionID string) bool {
	deadline := time.NewTimer(b.t.resumeReady)
	defer deadline.Stop()
	ticker := time.NewTicker(b.t.poll)
	defer ticker.Stop()
	for {
		text, _ := b.p.paneText(sessionID)
		switch {
		case strings.Contains(text, inputReadyMarker):
			// Box ready. Settle first or the Enter after inject's paste races the
			// still-settling TUI and is dropped (the "lost Enter" on resume).
			return sleepCtx(ctx, b.t.trustToInject)
		case chooserArrowOn(text, resumeChooserMarker):
			_ = b.p.sendKeys(sessionID, "Enter")
		case chooserArrowOn(text, resumeSummaryMarker):
			// Arrow on the summary default: step down (a leaked Down is harmless,
			// unlike a digit or Enter); re-read next tick.
			_ = b.p.sendKeys(sessionID, "Down")
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return true
		case <-ticker.C:
		}
	}
}

// --- Codex ---------------------------------------------------------------

// nestedCodexEnv lists the per-session markers Codex exports into processes it
// spawns. The critical one is CODEX_THREAD_ID — the analog of Claude's
// CLAUDE_CODE_SESSION_ID: a codex that inherits it behaves as a continuation of
// the parent thread, which would blind usher's per-session tailer (cf. the
// nestedClaudeEnv trap). CODEX_HOME is deliberate user config and is NOT
// scrubbed. The sandbox/CI markers are unset defensively. Exact list pending
// the empirical check (launch usher from inside a codex session, confirm a
// spawned session still persists its own rollout).
var nestedCodexEnv = []string{
	"CODEX_THREAD_ID",
	"CODEX_CI",
	"CODEX_SANDBOX",
	"CODEX_SANDBOX_NETWORK_DISABLED",
}

// Markers for matching Codex TUI states in a plain pane capture (validated on
// codex 0.139.0). Codex's resume has no chooser, so the only gate is the
// one-time trust prompt; readiness is the bottom footer.
const (
	codexTrustMarker  = "Do you trust the contents"
	codexBannerMarker = "OpenAI Codex (v"
)

var _ backend = codexBackend{}

type codexBackend struct {
	p           *pool
	t           timing
	codexCmd    string   // path to the codex binary
	sessionsDir string   // ~/.codex/sessions
	extraArgs   []string // e.g. ["--sandbox","workspace-write"]
}

func (b codexBackend) preAssignsID() bool            { return false }
func (b codexBackend) turnComplete(line []byte) bool { return codexrollout.IsTurnComplete(line) }

// spawnCommand builds `env -u CODEX_* codex [resume <id>] [-c model=…] [args]`.
// New sessions pass no id — Codex has no --session-id flag and generates its own
// UUIDv7, discovered after spawn. Resume goes straight in (no chooser). Model is
// set only on a new session via the universal `-c model=` override; a resumed
// session keeps the model it was created with (matching the Claude path).
func (b codexBackend) spawnCommand(sessionID, cwd, model string, resume bool) string {
	parts := []string{"env"}
	for _, v := range nestedCodexEnv {
		parts = append(parts, "-u", v)
	}
	parts = append(parts, shellQuote(b.codexCmd))
	// Codex won't run a config-declared hook until it's "trusted". usher persists
	// that trust at `usher setup` time (writes the hook's trusted_hash into
	// ~/.codex/config.toml's [hooks.state]; see setupCodexHook), so no
	// --dangerously-bypass-hook-trust is needed here — the hook is trusted by id.
	if resume {
		parts = append(parts, "resume", shellQuote(sessionID))
	} else if model != "" {
		parts = append(parts, "-c", shellQuote("model="+model))
	}
	for _, a := range b.extraArgs {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// locate globs the date-partitioned tree for the rollout whose filename ends in
// the session id: <sessionsDir>/YYYY/MM/DD/rollout-<ts>-<id>.jsonl.
func (b codexBackend) locate(sessionID string) string {
	matches, err := filepath.Glob(
		filepath.Join(b.sessionsDir, "*", "*", "*", "rollout-*-"+sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// discoverNewID finds the newest rollout under sessionsDir whose cwd matches and
// whose id is not already known — used after spawning a new Codex session to
// learn the id Codex assigned itself.
func (b codexBackend) discoverNewID(cwd string, known map[string]bool) string {
	matches, err := filepath.Glob(
		filepath.Join(b.sessionsDir, "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		return ""
	}
	var bestID string
	var bestMod time.Time
	for _, path := range matches {
		id := codexrollout.SessionIDFromPath(path)
		if id == "" || known[id] {
			continue
		}
		if rolloutCwd(path) != cwd {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if fi.ModTime().After(bestMod) {
			bestMod, bestID = fi.ModTime(), id
		}
	}
	return bestID
}

// knownSessionIDs globs every rollout under the sessions tree for its embedded
// id — the pre-spawn snapshot discoverNewID diffs against.
func (b codexBackend) knownSessionIDs() map[string]bool {
	out := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(b.sessionsDir, "*", "*", "*", "rollout-*.jsonl"))
	for _, p := range matches {
		if id := codexrollout.SessionIDFromPath(p); id != "" {
			out[id] = true
		}
	}
	return out
}

// waitReady accepts the one-time trust prompt (default option is "Yes,
// continue" → Enter) if it appears, then waits for the input-ready footer.
// Codex resume has no chooser, so unlike the Claude path there is no arrow-row
// tracking — just trust-then-footer. Bounded by t.resumeReady; false on cancel.
func (b codexBackend) waitReady(ctx context.Context, sessionID, cwd string, fresh, resume bool) bool {
	if !fresh {
		return sleepCtx(ctx, b.t.warmSettle)
	}
	deadline := time.NewTimer(b.t.resumeReady)
	defer deadline.Stop()
	ticker := time.NewTicker(b.t.poll)
	defer ticker.Stop()
	trusted := false
	for {
		text, _ := b.p.paneText(sessionID)
		switch {
		case codexInputReady(text, cwd):
			// Settle before the paste so the Enter after it isn't dropped into a
			// still-rendering composer.
			return sleepCtx(ctx, b.t.trustToInject)
		case !trusted && strings.Contains(text, codexTrustMarker):
			_ = b.p.sendKeys(sessionID, "Enter")
			trusted = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return true
		case <-ticker.C:
		}
	}
}

// codexInputReady reports whether the composer is ready: the bottom footer
// carries "· <cwd>" (always visible when ready, unlike the top banner which can
// scroll off a long resumed session); the banner is a fallback for short ones.
func codexInputReady(text, cwd string) bool {
	return strings.Contains(text, "· "+cwd) || strings.Contains(text, codexBannerMarker)
}

// rolloutCwd reads the cwd from a rollout's first line (the session_meta header)
// without scanning the whole file.
func rolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !sc.Scan() {
		return ""
	}
	var l struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(sc.Bytes(), &l); err != nil || l.Type != "session_meta" {
		return ""
	}
	return l.Payload.Cwd
}
