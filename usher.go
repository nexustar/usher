// Package usher assembles a usher binary: the web router over local
// coding-agent sessions. cmd/usher calls Main; a custom binary calls Main or
// Run with Extensions that add backends, HTTP routes and in-process frontends
// without forking, keeping their own dependencies in their own module.
//
// Stability: this package, pkg/backend, pkg/core, pkg/interaction and pkg/api
// are the extension contract, and changes to them are called out in release
// notes. Every other package under pkg/ is importable but may change in any
// release.
package usher

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nexustar/usher/pkg/agentprofile"
	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/broker"
	"github.com/nexustar/usher/pkg/interaction"
	"github.com/nexustar/usher/pkg/pluginapi"
	"github.com/nexustar/usher/pkg/router"
	"github.com/nexustar/usher/pkg/web"
)

// Extension adds to a usher binary at build time. Every field is optional.
// Main calls Flags before parsing the serve command line; Run calls Backends
// before the router exists and Start once it does, each in the order the
// extensions were given.
type Extension struct {
	// Name labels the extension in logs and errors.
	Name string
	// Flags registers serve flags the extension reads back in Backends or
	// Start.
	Flags func(fs *flag.FlagSet)
	// Backends returns the backends the extension contributes, keyed by the
	// name sessions carry. A name a built-in or earlier extension already
	// uses is an error.
	Backends func(env Env) (map[string]backend.Backend, error)
	// Start runs after the router and web server are built and before
	// serving begins. Long-running work belongs in a goroutine bound to ctx.
	Start func(ctx context.Context, app *App) error
}

// Env is what Run constructs the built-in backends with.
type Env struct {
	Options Options
	// HookSocket is the Unix socket spawned sessions report tool calls to;
	// backends pass it to their workers as USHER_HOOK_SOCK.
	HookSocket string
	// Interactions is where a backend submits permission requests and
	// questions, and reads a session's approval at spawn.
	Interactions *interaction.Manager
	Logger       *slog.Logger
}

// App is the assembled server an Extension's Start receives.
type App struct {
	Options      Options
	Router       *router.Router
	Broker       *broker.Broker
	Interactions *interaction.Manager
	Agents       *agentprofile.Store
	Logger       *slog.Logger
	web          *web.Server
}

// Handle serves h at pattern, an http.ServeMux pattern, behind the web
// server's authentication. Only valid during Start.
func (a *App) Handle(pattern string, h http.Handler) { a.web.Handle(pattern, h) }

// Options configures Run. DefaultOptions fills in what `usher serve` uses
// without flags; Flags binds the serve flags on top.
type Options struct {
	// Addr is the web listen address.
	Addr string
	// DataDir holds usher's own state: auth, settings, sockets, attachments.
	DataDir string

	// Each backend is enabled when its sessions directory exists or its CLI
	// is installed; an empty directory disables it.
	ClaudeCmd         string
	ClaudeSessionsDir string
	CodexCmd          string
	CodexSessionsDir  string
	// CodexArgs are extra flags for spawned codex. Empty uses codex's own
	// approval policy.
	CodexArgs     []string
	PiCmd         string
	PiSessionsDir string
	// PiArgs are extra flags for spawned pi RPC workers.
	PiArgs []string

	// TmuxSocket is the tmux socket prefix for session terminals.
	TmuxSocket string
	// TerminalShell is the shell executable for session terminals.
	TerminalShell string

	// AutoPause stops a session's backend process once it has been idle this
	// long; 0 keeps processes until evicted.
	AutoPause time.Duration
	// MaxLiveSessions caps concurrent live backend processes; least-recently
	// used sessions are evicted beyond it.
	MaxLiveSessions int
	// AutoArchiveDays hides sessions whose log is older than this from the
	// sidebar's default view; 0 disables auto-archive.
	AutoArchiveDays int

	// UIDir serves the web UI from disk instead of the embedded copy.
	UIDir string
	// EditorURL is the "Open in editor" template; {cwd} is replaced with the
	// session's working directory. Empty hides the entry.
	EditorURL string
	// DisablePush turns off Web Push notifications.
	DisablePush bool
	// DisableUsherTools leaves usher's own MCP tools off spawned sessions.
	DisableUsherTools bool

	// TelegramBotToken enables the Telegram mirror; `usher serve` reads it
	// from $TELEGRAM_BOT_TOKEN, never a flag.
	TelegramBotToken       string
	TelegramGroupID        int64
	TelegramAllowedUserIDs []int64

	// Insecure allows binding a non-loopback address without a password.
	Insecure bool
	// ReadOnly rejects state-changing web requests and runs nothing that
	// drives sessions or writes shared state.
	ReadOnly bool

	LogLevel  string
	LogFormat string
	// Logger replaces the one built from LogLevel and LogFormat. When nil,
	// Run builds one and installs it as slog's default, as the usher binary
	// does.
	Logger *slog.Logger
}

// DefaultOptions is `usher serve` without flags.
func DefaultOptions() Options {
	return Options{
		Addr:              "127.0.0.1:7777",
		DataDir:           pluginapi.DefaultDataDir(),
		ClaudeCmd:         "claude",
		ClaudeSessionsDir: defaultProjectsDir(),
		CodexCmd:          "codex",
		CodexSessionsDir:  defaultCodexSessionsDir(),
		PiCmd:             defaultPiCmd(),
		PiSessionsDir:     defaultPiSessionsDir(),
		TmuxSocket:        "usher",
		TerminalShell:     defaultTerminalShell(),
		MaxLiveSessions:   8,
		AutoArchiveDays:   7,
		LogLevel:          "info",
		LogFormat:         "text",
	}
}

// Flags binds the serve flags to o, with o's current values as defaults.
func (o *Options) Flags(fs *flag.FlagSet) {
	fs.StringVar(&o.Addr, "addr", o.Addr, "listen address")
	fs.StringVar(&o.ClaudeSessionsDir, "claude-sessions-dir", o.ClaudeSessionsDir,
		"Claude Code projects directory; the Claude backend auto-enables when it exists or claude is installed")
	fs.StringVar(&o.ClaudeSessionsDir, "projects-dir", o.ClaudeSessionsDir, "alias for --claude-sessions-dir")
	fs.StringVar(&o.DataDir, "data-dir", o.DataDir, "usher data directory (XDG_DATA_HOME/usher)")
	fs.StringVar(&o.ClaudeCmd, "claude", o.ClaudeCmd, "path to the claude binary")
	fs.StringVar(&o.CodexCmd, "codex", o.CodexCmd, "path to the codex binary (Codex backend)")
	fs.StringVar(&o.CodexSessionsDir, "codex-sessions-dir", o.CodexSessionsDir,
		"Codex rollout sessions directory; the Codex backend auto-enables when it exists or codex is installed")
	fs.Func("codex-args",
		"extra flags for spawned codex (space-separated). Empty uses codex's own approval "+
			"policy (usher gates whatever codex natively escalates); e.g. \"-c approval_policy=untrusted\" "+
			"to route most commands through usher like Claude.",
		func(s string) error { o.CodexArgs = strings.Fields(s); return nil })
	fs.StringVar(&o.PiCmd, "pi", o.PiCmd, "path to the pi binary (pi backend)")
	fs.StringVar(&o.PiSessionsDir, "pi-sessions-dir", o.PiSessionsDir,
		"pi coding-agent sessions directory; the pi backend auto-enables when it exists or pi is installed")
	fs.Func("pi-args", "extra flags for spawned pi RPC workers (space-separated)",
		func(s string) error { o.PiArgs = strings.Fields(s); return nil })
	fs.StringVar(&o.TmuxSocket, "tmux-socket", o.TmuxSocket, "tmux socket prefix for session terminals")
	fs.StringVar(&o.TerminalShell, "terminal-shell", o.TerminalShell, "shell executable for session terminals")
	fs.DurationVar(&o.AutoPause, "auto-pause", o.AutoPause,
		"stop a session's backend process once it has been idle this long (e.g. 10m); it resumes on the next message. "+
			"A warm Codex process keeps the session from being resumed in another Codex client. 0 keeps processes until evicted")
	fs.IntVar(&o.MaxLiveSessions, "max-live-sessions", o.MaxLiveSessions,
		"max concurrent live Claude stream-json processes; least-recently-used sessions are evicted beyond this")
	fs.IntVar(&o.AutoArchiveDays, "auto-archive-days", o.AutoArchiveDays,
		"sessions whose jsonl mtime is older than this fall out of the sidebar's default view; 0 disables auto-archive (manual archive still works)")
	fs.StringVar(&o.UIDir, "ui-dir", o.UIDir, "serve the web UI from this directory instead of the embedded copy")
	fs.StringVar(&o.EditorURL, "editor-url", o.EditorURL,
		"URL template for an \"Open in editor\" entry in the session actions menu; {cwd} is replaced "+
			"verbatim with the session's working directory (e.g. \"https://code.example.com/?folder={cwd}\" "+
			"for code-server, \"vscode://file{cwd}\" for local VS Code). Empty hides the entry.")
	fs.BoolVar(&o.DisablePush, "disable-push", o.DisablePush,
		"turn off Web Push browser notifications (turn-done + permission prompts). On by default, but "+
			"inert until a browser opts in (which needs the user's notification-permission grant) — nothing "+
			"is sent, and no push service is contacted, until then.")
	fs.BoolVar(&o.DisableUsherTools, "disable-usher-tools", o.DisableUsherTools,
		"do not register usher's own MCP tools (currently just show_image, which renders an image inline "+
			"in the web UI) on spawned claude sessions. On by default; the tools self-gate to usher-managed "+
			"sessions and don't touch the user's own MCP servers.")
	fs.Int64Var(&o.TelegramGroupID, "telegram-group-id", o.TelegramGroupID,
		"forum supergroup chat id to mirror sessions into; set the bot token in $TELEGRAM_BOT_TOKEN to enable")
	fs.Func("telegram-allowed-user-ids",
		"comma-separated Telegram user ids allowed to drive sessions; empty = any member of the group",
		func(s string) error {
			ids, err := parseUserIDs(s)
			o.TelegramAllowedUserIDs = ids
			return err
		})
	fs.BoolVar(&o.Insecure, "insecure", o.Insecure, "allow binding a non-loopback address without a password")
	fs.BoolVar(&o.ReadOnly, "read-only", o.ReadOnly,
		"serve this instance read-only: state-changing web requests are rejected, and nothing "+
			"that drives sessions or writes shared state runs (schedule runner, Telegram mirror, "+
			"plugin/hook sockets, web push). Can run alongside a live instance sharing the same "+
			"--data-dir and sessions directories.")
	fs.StringVar(&o.LogLevel, "log-level", o.LogLevel, "log verbosity (debug|info|warn|error)")
	fs.StringVar(&o.LogFormat, "log-format", o.LogFormat, "log output format (text|json)")

	// Still accepted so a service unit written for an older usher keeps
	// starting; Main warns when one is used.
	fs.String("permission-mode", "", "ignored: set a session's approval in the web UI, or a default on the default agent")
	for _, name := range []string{"agent-mode", "llm-base-url", "llm-model", "llm-api-key-env"} {
		fs.String(name, "", "ignored: the main-chat agent was replaced by the router session")
	}
	fs.Bool("llm-strict", false, "ignored: the main-chat agent was replaced by the router session")
}

// logger returns o.Logger, or builds one from LogLevel and LogFormat and
// makes it slog's default so packages without an injected logger share it.
func (o Options) logger() (*slog.Logger, error) {
	if o.Logger != nil {
		return o.Logger, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(o.LogLevel)); err != nil {
		return nil, fmt.Errorf("--log-level: %w", err)
	}
	opts := &slog.HandlerOptions{Level: l}
	var logger *slog.Logger
	switch o.LogFormat {
	case "text":
		logger = slog.New(slog.NewTextHandler(os.Stderr, opts))
	case "json":
		logger = slog.New(slog.NewJSONHandler(os.Stderr, opts))
	default:
		return nil, fmt.Errorf("--log-format must be text or json (got %q)", o.LogFormat)
	}
	slog.SetDefault(logger)
	return logger, nil
}

// parseUserIDs parses a comma-separated list of Telegram user ids. Empty
// entries are skipped; an empty string yields a nil slice (no whitelist).
func parseUserIDs(s string) ([]int64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var ids []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid user id %q", part)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func defaultProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

func defaultCodexSessionsDir() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

func defaultPiSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if dir := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

func defaultPiCmd() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "pi"
	}
	agentDir := os.Getenv("PI_CODING_AGENT_DIR")
	if agentDir == "" {
		agentDir = filepath.Join(home, ".pi", "agent")
	}
	// Neither official install lands on a service's PATH: pi 1.0's managed
	// installer keeps its launcher in the agent dir, the older npm-based one
	// beside its own Node runtime.
	for _, installed := range []string{
		filepath.Join(agentDir, "bin", "pi"),
		filepath.Join(home, ".local", "share", "pi-node", "current", "bin", "pi"),
	} {
		if info, statErr := os.Stat(installed); statErr == nil && !info.IsDir() {
			return installed
		}
	}
	return "pi"
}

func defaultTerminalShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "bash"
}
