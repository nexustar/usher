// Package sender drives headless Claude and Codex sessions and reports each
// turn's output by tailing the backend's session log.
package sender

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/appserver"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/claudestream"
	"github.com/nexustar/usher/internal/codexrollout"
	"github.com/nexustar/usher/internal/hook"
)

// StreamEvent is one event for a turn. Type is the jsonl line's "type"
// (e.g. "user", "assistant", "system"), or one of the synthesized values
// "subprocess.started" / "subprocess.exit" / "error". The names are kept from
// the headless era so the broker/web layer needs minimal change; the payloads
// are now whole jsonl lines (message granularity), not stream-json token
// deltas.
type StreamEvent = backend.Event

// timing groups the tunable delays for driving the TUI. Defaults are set in
// New; tests override them for speed.
type timing struct{ confirm, poll time.Duration }

type Sender struct {
	app      *appserver.Manager // non-nil for the headless Codex backend
	claude   *claudestream.Manager
	locateFn func(string) string
	logger   *slog.Logger
	t        timing
	tail     tailConfig
}

// claudeMCPConfigArgs writes an MCP config registering `usher mcp-stdio` next to
// the hook socket and returns the `--mcp-config` flags to load it. Returns nil
// (disabling the feature, not erroring) if the executable can't be resolved, so
// a write hiccup never blocks spawns.
func claudeMCPConfigArgs(hookSock string, logger *slog.Logger) []string {
	if hookSock == "" {
		return nil
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		logger.Warn("mcp config: cannot resolve usher executable; show_image disabled", "err", err)
		return nil
	}
	// alwaysLoad exempts the server from Claude Code's Tool Search deferral so the
	// tool is always loaded, not hidden behind a ToolSearch step.
	cfg := map[string]any{"mcpServers": map[string]any{
		"usher": map[string]any{"command": exe, "args": []string{"mcp-stdio"}, "alwaysLoad": true},
	}}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil
	}
	path := filepath.Join(filepath.Dir(hookSock), "mcp.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		logger.Warn("mcp config: write failed; show_image disabled", "path", path, "err", err)
		return nil
	}
	return []string{"--mcp-config", path}
}

// New builds a Sender. claudeCmd is the claude binary; permissionMode (if
// non-empty) is passed through as --permission-mode; projectsDir is Claude
// Code's projects root (used to locate session jsonl files by their globally
// unique id); socket is retained for configuration compatibility; hookSock
// routes AskUserQuestion hooks back to this instance; maxLive caps Claude
// workers. Tool permissions use claudestream's stdio control protocol.
func New(claudeCmd, permissionMode, projectsDir, socket, hookSock string, maxLive int, injectMCPTools bool, hooks *hook.Manager, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	_ = socket // retained for CLI/config compatibility
	var extra []string
	if permissionMode != "" {
		extra = []string{"--permission-mode", permissionMode}
	}
	// Register the show_image MCP server (unless --disable-usher-tools). Additive
	// — no --strict-mcp-config — so the user's own MCP servers are untouched.
	var mcpArgs []string
	if injectMCPTools {
		mcpArgs = claudeMCPConfigArgs(hookSock, logger)
		extra = append(extra, mcpArgs...)
	}
	t := timing{confirm: 8 * time.Second, poll: 150 * time.Millisecond}
	return &Sender{
		claude:   claudestream.New(claudeCmd, claudeHookSettings(hookSock, logger), hookSock, extra, maxLive, hooks, logger),
		locateFn: func(id string) string { return locateClaude(projectsDir, id) },
		logger:   logger,
		t:        t,
		tail: tailConfig{
			poll: 150 * time.Millisecond, appearWait: 20 * time.Second,
			contentOnly: true, turnComplete: isTurnComplete, turnAborted: isClaudeTurnAborted,
		},
	}
}

func claudeHookSettings(hookSock string, logger *slog.Logger) string {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		logger.Warn("claude hook: cannot resolve usher executable", "err", err)
		return ""
	}
	hookCommand := func(event string) string {
		cmd := exe + " hook " + event
		if hookSock != "" {
			cmd = "USHER_HOOK_SOCK=" + hookSock + " " + cmd
		}
		return cmd
	}
	handler := func(event string) []any {
		return []any{map[string]any{
			"type": "command", "command": hookCommand(event), "timeout": 604800,
		}}
	}
	settings := map[string]any{
		"hooks": map[string]any{
			// Permissions use Claude's streaming can_use_tool callback protocol.
			// AskUserQuestion still needs PreToolUse so the web UI can collect an
			// answer and return it as updatedInput under -p.
			"PreToolUse": []any{map[string]any{"matcher": "AskUserQuestion", "hooks": handler("PreToolUse")}},
		},
	}
	b, _ := json.Marshal(settings)
	return string(b)
}

func codexMCPConfig(logger *slog.Logger) map[string]any {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		logger.Warn("codex mcp: cannot resolve usher executable; show_image disabled", "err", err)
		return nil
	}
	return map[string]any{
		"mcp_servers.usher.command":                     exe,
		"mcp_servers.usher.args":                        []string{"mcp-stdio"},
		"mcp_servers.usher.env_vars":                    []string{"USHER_HOOK_SOCK"},
		"mcp_servers.usher.default_tools_approval_mode": "approve",
		// Codex's default callable MCP namespace keeps the legacy mcp__ prefix;
		// the unprefixed form covers installations with that feature enabled.
		"features.code_mode.direct_only_tool_namespaces": []string{"mcp__usher", "usher"},
	}
}

// NewCodex builds a Sender that drives Codex through per-session app-server
// workers.
// codexCmd is the codex binary; sessionsDir is ~/.codex/sessions (the rollout
// root, used to locate logs); sandboxArgs are extra codex flags (e.g.
// --sandbox workspace-write); hookSock, if set, routes the codex permission hook
// back to this instance. maxLive caps Codex workers; idle workers are shut down
// and cold-resumed on the next send. Codex assigns ids for new threads.
func NewCodex(codexCmd, sessionsDir, socket, hookSock string, sandboxArgs []string, maxLive int, injectMCPTools bool, hooks *hook.Manager, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	_ = socket // retained for CLI/config compatibility
	var env []string
	if hookSock != "" {
		env = append(env, "USHER_HOOK_SOCK="+hookSock)
	}
	t := timing{confirm: 8 * time.Second, poll: 150 * time.Millisecond}
	appConfig := map[string]any{}
	if injectMCPTools {
		appConfig = codexMCPConfig(logger)
	}
	sandbox, config := codexHeadlessParams(sandboxArgs, logger)
	for k, v := range appConfig {
		config[k] = v
	}
	return &Sender{
		app:      appserver.NewManager(codexCmd, hooks, sandbox, config, env, maxLive, logger),
		locateFn: func(id string) string { return locateCodex(sessionsDir, id) },
		logger:   logger,
		t:        t,
		tail: tailConfig{
			poll: 150 * time.Millisecond, appearWait: 20 * time.Second,
			contentOnly: true, turnComplete: codexrollout.IsTurnComplete, turnAborted: codexrollout.IsTurnAborted,
		},
	}
}

func codexHeadlessParams(args []string, logger *slog.Logger) (map[string]any, map[string]any) {
	p, cfg := map[string]any{}, map[string]any{}
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--sandbox" || args[i] == "-s") && i+1 < len(args):
			p["sandbox"] = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--sandbox="):
			p["sandbox"] = strings.TrimPrefix(args[i], "--sandbox=")
		case args[i] == "-c" && i+1 < len(args):
			kv := strings.SplitN(args[i+1], "=", 2)
			if len(kv) == 2 {
				cfg[kv[0]] = codexConfigValue(kv[1])
			} else {
				logger.Warn("headless codex: invalid -c override", "value", args[i+1])
			}
			i++
		default:
			logger.Warn("headless codex: unsupported --codex-args option", "option", args[i])
		}
	}
	return p, cfg
}

// Codex's common TOML literals (strings, booleans, numbers and arrays) are
// also valid JSON. Preserve bare TOML words as strings.
func codexConfigValue(raw string) any {
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return v
	}
	return raw
}

// Send injects prompt into the session's live interactive claude (resuming /
// spawning it as needed) and streams the resulting turn's events. The channel
// closes when the turn ends or ctx is cancelled.
func (s *Sender) Send(ctx context.Context, sessionID, prompt, cwd string) (<-chan StreamEvent, error) {
	if s.app != nil {
		return s.codexPrompt(ctx, sessionID, prompt, cwd, false)
	}
	if s.claude != nil {
		return s.claudeTurn(ctx, sessionID, prompt, cwd, "", true)
	}
	return nil, errors.New("sender has no headless backend")
}

func (s *Sender) codexPrompt(ctx context.Context, sessionID, prompt, cwd string, fresh bool) (<-chan StreamEvent, error) {
	command, args, isCommand := backend.ParseSlashCommand(prompt)
	if !isCommand {
		return s.appTurn(ctx, sessionID, prompt, cwd, fresh)
	}
	switch command {
	case "/compact":
		if args != "" {
			return nil, errors.New("usage: /compact")
		}
		return s.appOperation(ctx, sessionID, cwd, func() (<-chan appserver.TurnResult, <-chan appserver.Delta, error) {
			return s.app.Compact(ctx, sessionID, cwd)
		})
	case "/review":
		return s.appOperation(ctx, sessionID, cwd, func() (<-chan appserver.TurnResult, <-chan appserver.Delta, error) {
			return s.app.Review(ctx, sessionID, cwd, args)
		})
	default:
		skills, err := s.app.Skills(ctx, sessionID, cwd)
		if err != nil {
			return nil, fmt.Errorf("resolve command %s: %w", command, err)
		}
		if skillPrompt, ok := codexSlashSkillPrompt(command, args, skills); ok {
			return s.appTurn(ctx, sessionID, skillPrompt, cwd, fresh)
		}
		return nil, fmt.Errorf("unknown command: %s", command)
	}
}

// codexSlashSkillPrompt rewrites the /skill compatibility form into Codex's
// native $skill mention. Names match exactly because Codex resolves them
// exactly (core-skills injection matches the mention against skill.name as a
// plain string), and its slash commands are case-sensitive too — accepting
// /ImageGen here would invent a spelling that works in usher and nowhere else.
func codexSlashSkillPrompt(command, args string, skills []appserver.Skill) (string, bool) {
	name := strings.TrimPrefix(command, "/")
	for _, skill := range skills {
		if !skill.Enabled || skill.Name != name {
			continue
		}
		prompt := "$" + skill.Name
		if args != "" {
			prompt += " " + args
		}
		return prompt, true
	}
	return "", false
}

// Start creates a new backend session and starts its first turn. The concrete
// backend owns ID assignment: Claude accepts a caller-generated UUID while
// Codex returns the thread ID assigned by app-server.
func (s *Sender) Start(ctx context.Context, req backend.StartRequest) (string, <-chan StreamEvent, error) {
	if s.claude != nil {
		id := newSessionID()
		ch, err := s.claudeTurn(ctx, id, req.Prompt, req.Cwd, req.Model, false)
		return id, ch, err
	}
	if s.app != nil {
		id, err := s.app.StartThread(ctx, req.Cwd, req.Model)
		if err != nil {
			return "", nil, err
		}
		ch, err := s.codexPrompt(ctx, id, req.Prompt, req.Cwd, true)
		return id, ch, err
	}
	return "", nil, errors.New("sender has no headless backend")
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Has reports whether usher currently holds a live interactive process for
// sessionID.
func (s *Sender) Has(sessionID string) bool {
	if s.app != nil {
		return s.app.Has(sessionID)
	}
	if s.claude != nil {
		return s.claude.Has(sessionID)
	}
	return false
}

// Resume brings an existing session's backend worker online without sending a
// prompt. It is the inverse of Kill for UI lifecycle controls.
func (s *Sender) Resume(ctx context.Context, sessionID, cwd string) error {
	if s.app != nil {
		return s.app.Resume(ctx, sessionID, cwd)
	}
	if s.claude != nil {
		return s.claude.Resume(ctx, sessionID, cwd)
	}
	return errors.New("sender has no headless backend")
}

// LiveSessions returns the ids of sessions with a live backend worker.
func (s *Sender) LiveSessions() []string {
	if s.app != nil {
		return s.app.LiveSessions()
	}
	if s.claude != nil {
		return s.claude.LiveSessions()
	}
	return nil
}

// ComposerItems exposes backend-native composer completions through one web
// shape. Claude advertises slash commands at init; Codex supplies a small
// supported command set plus cwd-scoped skills from app-server. Both sides
// answer only from a live backend, so a cold session lists no skills until it
// has run a turn — completion is never worth starting a process for.
func (s *Sender) ComposerItems(ctx context.Context, sessionID, cwd string) (backend.ComposerCatalog, error) {
	if s.claude != nil {
		commands, available := s.claude.CommandsIfLive(sessionID)
		out := make([]backend.ComposerItem, 0, len(commands))
		for _, command := range commands {
			out = append(out, backend.ComposerItem{
				Name: command.Name, Kind: command.Kind,
			})
		}
		return backend.ComposerCatalog{Items: out, Available: available}, nil
	}
	if s.app == nil {
		return backend.ComposerCatalog{}, nil
	}
	out := []backend.ComposerItem{
		{Name: "compact", Kind: "command", Description: "Compact conversation context"},
		{Name: "review", Kind: "command", Description: "Review current changes"},
	}
	skills, available, err := s.app.SkillsIfLive(ctx, sessionID, cwd)
	if err != nil {
		s.logger.Warn("codex skill discovery failed", "session", sessionID, "err", err)
		return backend.ComposerCatalog{}, err
	}
	for _, skill := range skills {
		if !skill.Enabled {
			continue
		}
		out = append(out, backend.ComposerItem{
			Name:        skill.Name,
			Kind:        "skill",
			Description: skill.Description,
		})
	}
	return backend.ComposerCatalog{Items: out, Available: available}, nil
}

// Interrupt stops the in-flight turn for sessionID without killing its worker.
func (s *Sender) Interrupt(sessionID string) error {
	if s.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.app.Interrupt(ctx, sessionID)
	}
	if s.claude != nil {
		return s.claude.Interrupt(sessionID)
	}
	return nil
}

// Kill tears down usher's live worker for sessionID, if any.
func (s *Sender) Kill(sessionID string) error {
	if s.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.app.Kill(ctx, sessionID)
	}
	if s.claude != nil {
		return s.claude.Kill(sessionID)
	}
	return nil
}

// Shutdown tears down all backend workers.
func (s *Sender) Shutdown() {
	if s.app != nil {
		s.app.Shutdown()
		return
	}
	if s.claude != nil {
		s.claude.Shutdown()
		return
	}
}

func (s *Sender) claudeTurn(ctx context.Context, id, prompt, cwd, model string, resume bool) (<-chan StreamEvent, error) {
	path := s.locate(id)
	var offset int64
	if path != "" {
		if fi, e := os.Stat(path); e == nil {
			offset = fi.Size()
		}
	}
	done, deltas, fresh, queuedAhead, err := s.claude.Send(ctx, id, prompt, cwd, model, resume)
	if err != nil {
		return nil, err
	}
	tail := s.tail
	tail.skipCompletions = queuedAhead
	return mergeLoggedTurn(ctx, loggedTurnConfig[claudestream.Result, claudestream.Delta]{
		backend: "claude", idKey: "session_id", id: id, cwd: cwd, fresh: fresh,
		path: path, offset: offset, locate: func() string { return s.locateWait(ctx, id, s.t.confirm) },
		tail: tail, done: done, deltas: deltas, logger: s.logger,
		delta: func(d claudestream.Delta) (string, string, bool) { return "text", d.Text, true },
		result: func(ctx context.Context, out chan<- StreamEvent, result claudestream.Result) {
			// User-requested cancellation is not a turn failure.
			if result.IsError && result.Subtype != "error_during_execution" && result.Subtype != "cancelled" {
				emitError(ctx, out, "claude turn failed: "+result.Subtype)
			}
			emitClaudeRuntime(ctx, out, result)
		},
	}), nil
}

func emitClaudeRuntime(ctx context.Context, out chan<- StreamEvent, result claudestream.Result) bool {
	if result.ContextWindow <= 0 {
		return true
	}
	raw, _ := json.Marshal(map[string]any{
		"model":          result.Model,
		"context_window": result.ContextWindow,
	})
	return sendEvent(ctx, out, StreamEvent{Type: backend.EventRuntime, Raw: raw})
}

// appTurn keeps rollout jsonl as the content plane while app-server supplies
// the driving and terminal lifecycle signal.
func (s *Sender) appTurn(ctx context.Context, id, prompt, cwd string, fresh bool) (<-chan StreamEvent, error) {
	return s.appLoggedTurn(ctx, id, cwd, fresh, func() (<-chan appserver.TurnResult, <-chan appserver.Delta, error) {
		return s.app.StartTurn(ctx, id, prompt, cwd)
	})
}

func (s *Sender) appOperation(ctx context.Context, id, cwd string, start func() (<-chan appserver.TurnResult, <-chan appserver.Delta, error)) (<-chan StreamEvent, error) {
	return s.appLoggedTurn(ctx, id, cwd, false, start)
}

func (s *Sender) appLoggedTurn(ctx context.Context, id, cwd string, fresh bool, start func() (<-chan appserver.TurnResult, <-chan appserver.Delta, error)) (<-chan StreamEvent, error) {
	path := s.locate(id)
	var offset int64
	if path != "" {
		if fi, e := os.Stat(path); e == nil {
			offset = fi.Size()
		}
	}
	done, deltas, err := start()
	if err != nil {
		return nil, err
	}
	lastKind := ""
	return mergeLoggedTurn(ctx, loggedTurnConfig[appserver.TurnResult, appserver.Delta]{
		backend: "codex", idKey: "thread_id", id: id, cwd: cwd, fresh: fresh,
		path: path, offset: offset, locate: func() string { return s.locateWait(ctx, id, s.t.confirm) },
		tail: s.tail, done: done, deltas: deltas, logger: s.logger,
		delta: func(d appserver.Delta) (string, string, bool) {
			if d.Kind == "reasoning" && lastKind == "reasoning" {
				return "", "", false
			}
			lastKind = d.Kind
			return d.Kind, d.Text, true
		},
		result: func(ctx context.Context, out chan<- StreamEvent, result appserver.TurnResult) {
			if result.Status == "failed" {
				emitError(ctx, out, "codex turn failed")
			}
		},
	}), nil
}

func emitLiveDelta(ctx context.Context, out chan<- StreamEvent, kind, delta string) bool {
	if delta == "" {
		return true
	}
	typ, payload := backend.EventPartDelta, any(backend.PartDeltaPayload{Delta: delta})
	if kind == "reasoning" {
		typ, payload = backend.EventTurnStatus, backend.TurnStatusPayload{Status: "thinking"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return true
	}
	return sendEvent(ctx, out, StreamEvent{Type: typ, Raw: raw})
}

// drainTail flushes a finished turn's final records, then stops the tailer.
// Protocol completion and file visibility are separate clocks, so it waits for
// the transcript to catch up: until the backend's end-of-turn marker is
// forwarded (alreadyTerminal if the caller already saw it), else a
// finalDrainQuiet silence, else the finalDrainCeiling deadline. Records already
// on disk are never dropped.
func drainTail(ctx context.Context, out chan<- StreamEvent, events <-chan StreamEvent, cancel context.CancelFunc, isTerminal func([]byte) bool, alreadyTerminal bool) bool {
	defer cancel()
	exited := false
	// forceStop cancels the still-live tailer and forwards its final EOF read.
	// Only future growth is dropped, never a record already on disk.
	forceStop := func() bool {
		cancel()
		for ev := range events {
			exited = exited || ev.Type == backend.EventProcessExit
			if !sendEvent(ctx, out, ev) {
				return exited
			}
		}
		return exited
	}
	if alreadyTerminal {
		return forceStop()
	}
	quiet := time.NewTimer(finalDrainQuiet)
	defer quiet.Stop()
	ceiling := time.NewTimer(finalDrainCeiling)
	defer ceiling.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return exited
			}
			exited = exited || ev.Type == backend.EventProcessExit
			if !sendEvent(ctx, out, ev) {
				cancel()
				return exited
			}
			if isTerminal != nil && isTerminal(ev.Raw) {
				return forceStop()
			}
			quiet.Reset(finalDrainQuiet)
		case <-quiet.C:
			return forceStop()
		case <-ceiling.C:
			slog.Warn("final transcript drain hit hard ceiling; stopping tail")
			return forceStop()
		}
	}
}

func locateClaude(root, id string) string {
	matches, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func locateCodex(root, id string) string {
	matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*-"+id+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func (s *Sender) locate(sessionID string) string {
	if s.locateFn == nil {
		return ""
	}
	return s.locateFn(sessionID)
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
	raw, _ := json.Marshal(backend.ErrorPayload{Message: msg})
	sendEvent(ctx, out, StreamEvent{Type: backend.EventError, Raw: raw})
}
