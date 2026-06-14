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
	"strings"
	"syscall"
	"time"

	"usher/internal/agent/usheragent"
	"usher/internal/archive"
	"usher/internal/auth"
	"usher/internal/broker"
	"usher/internal/discovery"
	"usher/internal/hook"
	"usher/internal/mainchat"
	"usher/internal/router"
	"usher/internal/sender"
	"usher/internal/web"
)

const Version = "0.1.0-dev"

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
		senders["claude"] = sender.New(*claudeCmd, *permissionMode, dir, *tmuxSocket+"-claude", hookSockPath(*dataDir), *maxLiveSessions, logger)
		defaultBackend = "claude"
		logger.Info("claude backend enabled", "projects_dir", dir)
	}
	if dir := *codexSessionsDir; dir != "" && isDir(dir) {
		sources = append(sources, discovery.NewCodexSource(dir))
		senders["codex"] = sender.NewCodex(*codexCmd, dir, *tmuxSocket+"-codex", hookSockPath(*dataDir), strings.Fields(*codexArgs), *maxLiveSessions, logger)
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
	archiveStore := archive.New(
		filepath.Join(*dataDir, "archived.json"),
		time.Duration(*autoArchiveDays)*24*time.Hour,
	)
	r := router.New(d, senders, defaultBackend, b, h, archiveStore)

	mainStore, err := mainchat.NewStore(filepath.Join(*dataDir, "mainchats"))
	if err != nil {
		return fmt.Errorf("init main chat store: %w", err)
	}

	agent, err := buildAgent(r, *agentMode, *llmBaseURL, *llmModel, *llmAPIKeyEnv, *llmStrict)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		return err
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

	srv := web.NewServer(*addr, hookSockPath(*dataDir), authStore, r, mainStore, agent, codexModelsPath, logger)
	return srv.Run(ctx)
}

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

func defaultDataDir() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "usher")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "usher")
}
