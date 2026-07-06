// usher is the entrypoint binary. It dispatches subcommands.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nexustar/usher/internal/agent/usheragent"
	"github.com/nexustar/usher/internal/auth"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/mainchat"
	"github.com/nexustar/usher/internal/pluginapi"
	"github.com/nexustar/usher/internal/push"
	"github.com/nexustar/usher/internal/router"
	"github.com/nexustar/usher/internal/sender"
	"github.com/nexustar/usher/internal/sessionmeta"
	"github.com/nexustar/usher/internal/telegram"
	"github.com/nexustar/usher/internal/web"
)

var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		if err := serve(args); err != nil {
			fmt.Fprintln(os.Stderr, "usher serve:", err)
			os.Exit(1)
		}
	case "hook":
		if err := runHook(args); err != nil {
			fmt.Fprintln(os.Stderr, "usher hook:", err)
			os.Exit(1)
		}
	case "setup":
		if err := runSetup(args); err != nil {
			fmt.Fprintln(os.Stderr, "usher setup:", err)
			os.Exit(1)
		}
	case "set-password":
		if err := runSetPassword(args); err != nil {
			fmt.Fprintln(os.Stderr, "usher set-password:", err)
			os.Exit(1)
		}
	case "mcp-stdio":
		if err := runMCPStdio(args); err != nil {
			fmt.Fprintln(os.Stderr, "usher mcp-stdio:", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Println(Version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: usher <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  serve              start the web server")
	fmt.Fprintln(os.Stderr, "  setup              register usher's permission hook with installed backends (Claude/Codex)")
	fmt.Fprintln(os.Stderr, "  set-password       set/change the web UI password (required for non-loopback bind)")
	fmt.Fprintln(os.Stderr, "  hook <event-name>  invoked by Claude Code; not for direct use")
	fmt.Fprintln(os.Stderr, "  version            print version")
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7777", "listen address")
	projectsDir := fs.String("projects-dir", defaultProjectsDir(), "Claude Code projects directory")
	dataDir := fs.String("data-dir", defaultDataDir(), "usher data directory (XDG_DATA_HOME/usher)")
	claudeCmd := fs.String("claude", "claude", "path to the claude binary")
	codexCmd := fs.String("codex", "codex", "path to the codex binary (Codex backend)")
	codexSessionsDir := fs.String("codex-sessions-dir", defaultCodexSessionsDir(),
		"Codex rollout sessions directory; the Codex backend auto-enables when it exists")
	codexArgs := fs.String("codex-args", "",
		"extra flags for spawned codex (space-separated). Empty uses codex's own approval "+
			"policy (usher gates whatever codex natively escalates); e.g. \"-c approval_policy=untrusted\" "+
			"to route most commands through usher like Claude.")
	permissionMode := fs.String("permission-mode", "default",
		"--permission-mode passed to claude (default|acceptEdits|bypassPermissions|plan)")
	tmuxSocket := fs.String("tmux-socket", "usher",
		"prefix for usher's dedicated tmux server sockets (tmux -L <prefix>-claude / <prefix>-codex)")
	maxLiveSessions := fs.Int("max-live-sessions", 8,
		"max concurrent live interactive claude processes; least-recently-used sessions are evicted beyond this")
	agentMode := fs.String("agent-mode", "rule",
		"main-chat agent backend: rule | llm")
	llmBaseURL := fs.String("llm-base-url", "https://api.openai.com/v1",
		"OpenAI-compatible chat completions base URL (e.g. https://api.openai.com/v1, http://localhost:11434/v1)")
	llmModel := fs.String("llm-model", "",
		"model identifier when --agent-mode=llm (e.g. gpt-4o-mini, claude-haiku-4-5, qwen2.5:14b)")
	llmAPIKeyEnv := fs.String("llm-api-key-env", "OPENAI_API_KEY",
		"env var holding the API key (use empty value for backends without auth, e.g. local Ollama)")
	llmStrict := fs.Bool("llm-strict", false,
		"append a small-model enforcement block to the system prompt (recommended for haiku / mini / flash / 7B-class models)")
	autoArchiveDays := fs.Int("auto-archive-days", 7,
		"sessions whose jsonl mtime is older than this fall out of the sidebar's default view; 0 disables auto-archive (manual archive still works)")
	uiDir := fs.String("ui-dir", "",
		"serve the web UI from this directory instead of the embedded copy")
	editorURL := fs.String("editor-url", "",
		"URL template for an \"Open in editor\" entry in the session actions menu; {cwd} is replaced "+
			"verbatim with the session's working directory (e.g. \"https://code.example.com/?folder={cwd}\" "+
			"for code-server, \"vscode://file{cwd}\" for local VS Code). Empty hides the entry.")
	disablePush := fs.Bool("disable-push", false,
		"turn off Web Push browser notifications (turn-done + permission prompts). On by default, but "+
			"inert until a browser opts in (which needs the user's notification-permission grant) — nothing "+
			"is sent, and no push service is contacted, until then.")
	disableUsherTools := fs.Bool("disable-usher-tools", false,
		"do not register usher's own MCP tools (currently just show_image, which renders an image inline "+
			"in the web UI) on spawned claude sessions. On by default; the tools self-gate to usher-managed "+
			"sessions and don't touch the user's own MCP servers.")
	tgGroupID := fs.Int64("telegram-group-id", 0,
		"forum supergroup chat id to mirror sessions into; set the bot token in $TELEGRAM_BOT_TOKEN to enable")
	tgAllowedUsers := fs.String("telegram-allowed-user-ids", "",
		"comma-separated Telegram user ids allowed to drive sessions; empty = any member of the group")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *autoArchiveDays < 0 {
		return fmt.Errorf("--auto-archive-days must be ≥ 0 (got %d)", *autoArchiveDays)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *dataDir == "" {
		return fmt.Errorf("could not resolve data dir; pass --data-dir")
	}

	authStore, err := auth.Load(*dataDir)
	if err != nil {
		return fmt.Errorf("load auth: %w", err)
	}
	if !authStore.IsConfigured() && !addrIsLoopback(*addr) {
		return fmt.Errorf(
			"refusing to bind non-loopback %q without a password.\n"+
				"  run `usher set-password` first, or bind to 127.0.0.1 / localhost for local-only access.",
			*addr,
		)
	}

	// Each backend is enabled only when its session dir exists (created once that
	// CLI has run). usher works with either or both; at least one is required. A
	// separate tmux socket per backend keeps their process pools from adopting
	// each other's windows. defaultBackend (new-session/fallback) prefers Claude.
	sources := []discovery.Source{}
	senders := map[string]*sender.Sender{}
	defaultBackend := ""
	var codexModelsPath string

	if dir := *projectsDir; dir != "" && isDir(dir) {
		sources = append(sources, discovery.NewClaudeSource(dir))
		senders["claude"] = sender.New(*claudeCmd, *permissionMode, dir, *tmuxSocket+"-claude", hookSockPath(*dataDir), *maxLiveSessions, !*disableUsherTools, logger)
		defaultBackend = "claude"
		logger.Info("claude backend enabled", "projects_dir", dir)
	}
	if dir := *codexSessionsDir; dir != "" && isDir(dir) {
		sources = append(sources, discovery.NewCodexSource(dir))
		senders["codex"] = sender.NewCodex(*codexCmd, dir, *tmuxSocket+"-codex", hookSockPath(*dataDir), strings.Fields(*codexArgs), *maxLiveSessions, !*disableUsherTools, logger)
		// codex's per-account model catalog sits next to the sessions dir.
		codexModelsPath = filepath.Join(filepath.Dir(dir), "models_cache.json")
		if defaultBackend == "" {
			defaultBackend = "codex"
		}
		logger.Info("codex backend enabled", "sessions_dir", dir)
	}

	if len(senders) == 0 {
		return fmt.Errorf("no backend found: neither %q (Claude Code) nor %q (Codex) exists.\n"+
			"  run claude or codex once first, or pass --projects-dir / --codex-sessions-dir.",
			*projectsDir, *codexSessionsDir)
	}

	d, err := discovery.NewMulti(logger, sources...)
	if err != nil {
		return err
	}
	b := broker.New()
	h := hook.New(filepath.Join(*dataDir, "auto-approve.json"))
	meta := sessionmeta.New(
		filepath.Join(*dataDir, "sessions.json"),
		time.Duration(*autoArchiveDays)*24*time.Hour,
	)
	r := router.New(d, senders, defaultBackend, b, h, meta)

	mainStore, err := mainchat.NewStore(filepath.Join(*dataDir, "mainchats"))
	if err != nil {
		return fmt.Errorf("init main chat store: %w", err)
	}

	agent, err := buildAgent(r, *agentMode, *llmBaseURL, *llmModel, *llmAPIKeyEnv, *llmStrict)
	if err != nil {
		return err
	}

	// Web push: a second consumer of the broker/hook event seams, delivering
	// turn-done and permission notifications to subscribed browsers. On by
	// default but inert until a browser subscribes — which needs the user's
	// explicit notification-permission grant, and until then nothing is sent and
	// no push service is contacted. --disable-push skips it entirely (no VAPID
	// key, routes 404). A keypair failure disables push but doesn't stop serving.
	var pushMgr *push.Manager
	if !*disablePush {
		pushMgr, err = push.New(push.Config{
			KeyPath:   filepath.Join(*dataDir, "vapid.json"),
			StorePath: filepath.Join(*dataDir, "push-subscriptions.json"),
			Lookup: func(id string) (push.SessionInfo, bool) {
				sess, ok := r.GetSession(id)
				return push.SessionInfo{Title: sess.Title, Cwd: sess.Cwd}, ok
			},
			Events:  b,
			Pending: h,
			Logger:  logger,
		})
		if err != nil {
			logger.Warn("web push disabled", "err", err)
			pushMgr = nil
		}
	}
	logger.Info("web push", "enabled", pushMgr != nil)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		return err
	}

	if pushMgr != nil {
		go pushMgr.Run(ctx)
	}

	logger.Info("agent backend",
		"mode", *agentMode,
		"model", *llmModel,
		"base_url", *llmBaseURL,
		"strict", *llmStrict,
	)
	logger.Info("auth",
		"configured", authStore.IsConfigured(),
		"loopback_bind", addrIsLoopback(*addr),
	)

	if err := startTelegramHub(ctx, r, *tgGroupID, *tgAllowedUsers, *dataDir, logger); err != nil {
		return err
	}

	// Plugin API: out-of-process IM frontends (e.g. usher-lark) consume the
	// same Router seam the Telegram hub uses, over a 0600 Unix socket. Always
	// on — it is inert until a plugin connects.
	go func() {
		if err := pluginapi.NewServer(r, logger).Run(ctx, pluginapi.SocketPath(*dataDir)); err != nil && ctx.Err() == nil {
			logger.Warn("plugin api stopped", "err", err)
		}
	}()

	srv := web.NewServer(*addr, hookSockPath(*dataDir), authStore, r, mainStore, agent, pushMgr, codexModelsPath, *editorURL, *uiDir, logger)

	// Foreign-turn watcher: turns usher didn't start (background workflow
	// continuations, pane-typed prompts) get relayed to the chats that
	// routed to their session.
	r.SetForeignTurnHandler(srv.RelayForeignTurn)
	go r.RunForeignWatch(ctx, 0)

	return srv.Run(ctx)
}

// telegramTokenEnv is the env var holding the bot token; setting it enables the
// Telegram integration (the token is a secret, so it's never a flag).
const telegramTokenEnv = "TELEGRAM_BOT_TOKEN"

// startTelegramHub launches the Telegram forum mirror in a background goroutine
// when $TELEGRAM_BOT_TOKEN is set. No token disables it silently; a token
// without --telegram-group-id is a misconfiguration and errors out.
func startTelegramHub(ctx context.Context, r *router.Router, groupID int64, allowedUsers, dataDir string, logger *slog.Logger) error {
	token := os.Getenv(telegramTokenEnv)
	if token == "" {
		return nil // integration disabled
	}
	if groupID == 0 {
		return fmt.Errorf("--telegram-group-id is required when %s is set", telegramTokenEnv)
	}
	allowed, err := parseUserIDs(allowedUsers)
	if err != nil {
		return fmt.Errorf("--telegram-allowed-user-ids: %w", err)
	}
	hub, err := telegram.NewHub(
		telegram.NewClient(token, ""),
		r,
		telegram.Config{
			GroupID:        groupID,
			StatePath:      filepath.Join(dataDir, "telegram-topics.json"),
			AllowedUserIDs: allowed,
		},
		logger,
	)
	if err != nil {
		return fmt.Errorf("init telegram hub: %w", err)
	}
	go func() {
		if err := hub.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("telegram hub stopped", "err", err)
		}
	}()
	return nil
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

var _ telegram.RouterAPI = (*router.Router)(nil)
var _ pluginapi.RouterAPI = (*router.Router)(nil)

// addrIsLoopback reports whether the host part of addr binds only on loopback
// interfaces. Empty host (e.g. ":7777") means all interfaces ⇒ not loopback.
func addrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hookSockPath returns the Unix socket path for the hook listener.
func hookSockPath(dataDir string) string {
	return filepath.Join(dataDir, "hook.sock")
}

func buildAgent(r *router.Router, mode, baseURL, model, apiKeyEnv string, strict bool) (usheragent.Agent, error) {
	switch mode {
	case "", "rule":
		return usheragent.NewRule(r), nil
	case "llm":
		if model == "" {
			return nil, fmt.Errorf("--agent-mode=llm requires --llm-model (e.g. --llm-model gpt-4o-mini)")
		}
		var apiKey string
		if apiKeyEnv != "" {
			apiKey = os.Getenv(apiKeyEnv)
		}
		client := usheragent.NewChatClient(baseURL, apiKey)
		return usheragent.NewLLM(r, usheragent.LLMConfig{Client: client, Model: model, Strict: strict})
	default:
		return nil, fmt.Errorf("unknown --agent-mode: %q (want rule | llm)", mode)
	}
}

func defaultProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

func defaultCodexSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func defaultDataDir() string { return pluginapi.DefaultDataDir() }
