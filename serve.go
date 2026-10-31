package usher

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexustar/usher/pkg/agentprofile"
	"github.com/nexustar/usher/pkg/api"
	"github.com/nexustar/usher/pkg/auth"
	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/broker"
	"github.com/nexustar/usher/pkg/cli"
	"github.com/nexustar/usher/pkg/discovery"
	"github.com/nexustar/usher/pkg/interaction"
	"github.com/nexustar/usher/pkg/modelcatalog"
	piagent "github.com/nexustar/usher/pkg/pi"
	"github.com/nexustar/usher/pkg/pluginapi"
	"github.com/nexustar/usher/pkg/push"
	"github.com/nexustar/usher/pkg/router"
	"github.com/nexustar/usher/pkg/schedule"
	"github.com/nexustar/usher/pkg/sender"
	"github.com/nexustar/usher/pkg/sessionmeta"
	"github.com/nexustar/usher/pkg/settings"
	"github.com/nexustar/usher/pkg/telegram"
	"github.com/nexustar/usher/pkg/terminal"
	"github.com/nexustar/usher/pkg/transcript"
	"github.com/nexustar/usher/pkg/web"
)

var _ telegram.RouterAPI = (*router.Router)(nil)
var _ pluginapi.RouterAPI = (*router.Router)(nil)

// Version is what `usher version` prints and the API reports; the Makefile
// sets it at build time.
var Version = "dev"

// telegramTokenEnv holds the bot token; setting it enables the Telegram
// integration (the token is a secret, so it's never a flag).
const telegramTokenEnv = "TELEGRAM_BOT_TOKEN"

// Main is the usher command line: args follow the program name; the result
// is the exit status.
func Main(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "serve":
		err = serve(rest)
	case "hook":
		err = cli.Hook(rest)
	case "setup":
		err = cli.Setup(rest)
	case "set-password":
		err = cli.SetPassword(rest)
	case "mcp-stdio":
		err = cli.MCPStdio(rest, Version)
	case "version", "-v", "--version":
		fmt.Println(Version)
		return 0
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "usher %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: usher <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  serve              start the web server")
	fmt.Fprintln(os.Stderr, "  setup --remove     remove interactions installed by older usher releases")
	fmt.Fprintln(os.Stderr, "  set-password       set/change the web UI password (required for non-loopback bind)")
	fmt.Fprintln(os.Stderr, "  hook <event-name>  invoked by Claude Code; not for direct use")
	fmt.Fprintln(os.Stderr, "  mcp-stdio         serve usher MCP tools over stdio; --router-tools opts into router controls")
	fmt.Fprintln(os.Stderr, "  version            print version")
}

// serve parses the serve flags and runs until SIGINT or SIGTERM.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	o := DefaultOptions()
	o.Flags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger, err := o.logger()
	if err != nil {
		return err
	}
	o.Logger = logger
	fs.Visit(func(f *flag.Flag) {
		switch {
		case f.Name == "agent-mode" || strings.HasPrefix(f.Name, "llm-"):
			logger.Warn("flag ignored: the main-chat agent was replaced by the router session", "flag", "--"+f.Name)
		case f.Name == "permission-mode":
			logger.Warn("flag ignored: approvals are per session, ask unless set", "flag", "--permission-mode")
		}
	})
	o.TelegramBotToken = os.Getenv(telegramTokenEnv)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return Run(ctx, o)
}

// Run is `usher serve`: New, then Serve on o.Addr until ctx is cancelled.
func Run(ctx context.Context, o Options) error {
	srv, err := New(ctx, o)
	if err != nil {
		return err
	}
	defer srv.Close()
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return fmt.Errorf("web listen %s: %w", o.Addr, err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ErrDataDirInUse is another usher already running on Options.DataDir.
var ErrDataDirInUse = errors.New("another usher is running on this data dir")

// Server is a running usher, from New.
type Server struct {
	app      *App
	web      *web.Server
	auth     *auth.Store
	backends map[string]backend.Backend
	cancel   context.CancelFunc
	close    sync.Once
}

func (s *Server) App() *App { return s.app }

// Handler is the UI and API behind usher's authentication.
func (s *Server) Handler() http.Handler { return s.web.Handler() }

// Handle adds h at pattern, an http.ServeMux pattern, to Handler. Name a
// method in it: not every authenticating proxy covers OPTIONS.
func (s *Server) Handle(pattern string, h http.Handler) { s.web.Handle(pattern, h) }

// Serve serves Handler on ln until Close, then returns
// http.ErrServerClosed. A listener off loopback needs a password, as Addr
// does.
func (s *Server) Serve(ln net.Listener) error {
	if err := exposed(ln.Addr(), s.app.Options, s.auth); err != nil {
		ln.Close()
		return err
	}
	return s.web.Serve(ln)
}

// Close stops serving, the background work and the agent processes.
func (s *Server) Close() error {
	s.close.Do(func() {
		s.cancel()
		_ = s.web.Close()
		for _, b := range s.backends {
			b.Runtime.Shutdown()
		}
	})
	return nil
}

// New assembles usher from o and returns once it is ready to serve; it runs
// until ctx ends or Close.
//
// Spawned sessions call the running executable back as `<exe> hook` and
// `<exe> mcp-stdio`: a program embedding usher must pass those to Main.
func New(ctx context.Context, o Options) (*Server, error) {
	ctx, cancel := context.WithCancel(ctx)
	srv, err := assemble(ctx, o)
	if err != nil {
		cancel()
		return nil, err
	}
	srv.cancel = cancel
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	return srv, nil
}

func assemble(ctx context.Context, o Options) (*Server, error) {
	if o.AutoArchiveDays < 0 {
		return nil, fmt.Errorf("--auto-archive-days must be ≥ 0 (got %d)", o.AutoArchiveDays)
	}
	if o.DataDir == "" {
		return nil, errors.New("could not resolve data dir; pass --data-dir")
	}
	// Before backend setup writes into the data dir. Read-only shares it by
	// design.
	if !o.ReadOnly && socketAnswers(pluginapi.HookSocketPath(o.DataDir)) {
		return nil, fmt.Errorf("%w: %s", ErrDataDirInUse, o.DataDir)
	}
	logger, err := o.logger()
	if err != nil {
		return nil, err
	}
	attachmentsDir, err := filepath.Abs(filepath.Join(o.DataDir, "attachments"))
	if err != nil {
		return nil, fmt.Errorf("resolve attachments dir: %w", err)
	}

	authStore, err := auth.Load(o.DataDir)
	if err != nil {
		return nil, fmt.Errorf("load auth: %w", err)
	}
	cfg := settings.New(filepath.Join(o.DataDir, "settings.json"))
	// A read-only companion leaves the files to the instance that owns them,
	// which may predate settings.json holding them.
	if !o.ReadOnly {
		for _, key := range []string{"agents", "schedules"} {
			if err := cfg.ImportFile(key, filepath.Join(o.DataDir, key+".json")); err != nil {
				return nil, fmt.Errorf("move %s into settings.json: %w", key, err)
			}
		}
	}
	agents, err := agentprofile.Open(cfg.Agents(), func(p []agentprofile.Profile) error { return cfg.SetAgents(p) })
	if err != nil {
		return nil, err
	}
	schedules, err := schedule.Open(cfg.Schedules(), func(t []schedule.Task) error { return cfg.SetSchedules(t) })
	if err != nil {
		return nil, err
	}
	switch {
	case o.AuthHeader != "":
		logger.Info("trusting proxy header as the signed-in user", "header", o.AuthHeader,
			"password_too", authStore.IsConfigured())
	case !authStore.IsConfigured() && o.Addr != "" && !addrIsLoopback(o.Addr):
		if !o.Insecure {
			return nil, refuseExposed(o.Addr)
		}
		logger.Warn("binding non-loopback address without a password (--insecure)", "addr", o.Addr)
	}

	// Each built-in backend is enabled when its session dir exists or its CLI
	// is installed. At least one backend is required.
	// defaultBackend (new-session/fallback) is the first enabled, so Claude
	// when it is.
	hookSock := pluginapi.HookSocketPath(o.DataDir)
	h := interaction.New()
	backends := map[string]backend.Backend{}
	var sources []backend.Source
	defaultBackend := ""
	add := func(name string, b backend.Backend) {
		backends[name] = b
		if b.Source != nil {
			sources = append(sources, b.Source)
		}
		if defaultBackend == "" || (!backends[defaultBackend].Installed() && b.Installed()) {
			defaultBackend = name
		}
	}

	if dir := o.ClaudeSessionsDir; sessionsDir(dir, o.ClaudeCmd, logger) {
		claudeRuntime := sender.New(o.ClaudeCmd, dir, hookSock, o.MaxLiveSessions, !o.DisableUsherTools, h, logger)
		claudeRuntime.SetAutoPause(o.AutoPause)
		// Claude Code's per-account model catalog sits next to the projects dir.
		claudeModels := modelcatalog.Claude{Dir: filepath.Join(filepath.Dir(dir), "cache", "model-catalog")}
		add("claude", backend.Backend{
			CLI: o.ClaudeCmd, Runtime: claudeRuntime, Transcript: transcript.Claude{}, Source: discovery.NewClaudeSource(dir),
			Forker: transcript.ClaudeForker{}, Renamer: claudeRuntime, Models: claudeModels,
		})
		logger.Info("claude backend enabled", "sessions_dir", dir)
		checkCLI("claude", o.ClaudeCmd, o.ReadOnly, logger)
	}
	if dir := o.CodexSessionsDir; sessionsDir(dir, o.CodexCmd, logger) {
		// codex's per-account model catalog sits next to the sessions dir.
		modelsPath := filepath.Join(filepath.Dir(dir), "models_cache.json")
		codexRuntime := sender.NewCodex(o.CodexCmd, dir, hookSock, o.CodexArgs, o.MaxLiveSessions, !o.DisableUsherTools, h, logger)
		codexRuntime.SetAutoPause(o.AutoPause)
		add("codex", backend.Backend{
			CLI: o.CodexCmd, Runtime: codexRuntime, Transcript: transcript.Codex{}, Source: discovery.NewCodexSource(dir),
			Forker: transcript.CodexForker{}, Renamer: codexRuntime, Models: modelcatalog.Codex{Path: modelsPath}, Replier: transcript.Codex{},
		})
		logger.Info("codex backend enabled", "sessions_dir", dir)
		checkCLI("codex", o.CodexCmd, o.ReadOnly, logger)
	}
	if dir := o.PiSessionsDir; sessionsDir(dir, o.PiCmd, logger) {
		extra := append([]string(nil), o.PiArgs...)
		// Read-only never spawns pi, and skipping the writes keeps a shared
		// data-dir's extension files untouched while the live instance spawns.
		routerExtension := ""
		if !o.DisableUsherTools && !o.ReadOnly {
			if extensionPath, extensionErr := piagent.PrepareUsherExtension(o.DataDir); extensionErr != nil {
				logger.Warn("pi extension: write failed; show_image disabled", "err", extensionErr)
			} else {
				extra = append(extra, "--extension", extensionPath)
			}
			if path, routerErr := piagent.PrepareUsherRouterExtension(o.DataDir, hookSock); routerErr != nil {
				logger.Warn("pi router extension: write failed; router sessions get no routing tools", "err", routerErr)
			} else {
				routerExtension = path
			}
		}
		piModels := piagent.Models{Path: filepath.Join(o.DataDir, "pi-models.json")}
		piRuntime := piagent.NewRuntime(o.PiCmd, dir, extra, o.MaxLiveSessions, piModels, h, logger)
		piRuntime.SetAutoPause(o.AutoPause)
		if routerExtension != "" {
			piRuntime.SetRouterExtension(routerExtension)
		}
		add("pi", backend.Backend{
			CLI: o.PiCmd, Runtime: piRuntime, Transcript: piagent.Transcript{}, Source: discovery.NewPiSource(dir),
			Forker: piRuntime, Renamer: piRuntime, Models: piModels,
		})
		logger.Info("pi backend enabled", "sessions_dir", dir)
		checkCLI("pi", o.PiCmd, o.ReadOnly, logger)
	}

	if o.Backends != nil {
		added, err := o.Backends(Env{Options: o, HookSocket: hookSock, Interactions: h, Logger: logger})
		if err != nil {
			return nil, fmt.Errorf("backends: %w", err)
		}
		names := make([]string, 0, len(added))
		for name := range added {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			b := added[name]
			if _, dup := backends[name]; dup {
				return nil, fmt.Errorf("backend %q is already registered", name)
			}
			if b.Runtime == nil || b.Transcript == nil {
				return nil, fmt.Errorf("backend %q needs a Runtime and a Transcript", name)
			}
			add(name, b)
			logger.Info("backend enabled", "backend", name)
		}
	}

	if len(backends) == 0 {
		return nil, fmt.Errorf("no backend found: none of %q, %q, %q is installed, and the sessions directories "+
			"%q, %q, and %q do not exist.\n"+
			"  install a supported agent, or pass its binary or sessions-directory flag.",
			o.ClaudeCmd, o.CodexCmd, o.PiCmd, o.ClaudeSessionsDir, o.CodexSessionsDir, o.PiSessionsDir)
	}

	d, err := discovery.NewMulti(logger, sources...)
	if err != nil {
		return nil, err
	}
	if claudeRuntime, ok := backends["claude"].Runtime.(*sender.Sender); ok {
		claudeRuntime.SetScheduled(func(id string) bool {
			d.Complete(id)
			s, ok := d.Get(id)
			return ok && s.Activity.Pins()
		})
	}
	b := broker.New()
	meta := sessionmeta.New(
		filepath.Join(o.DataDir, "sessions.json"),
		time.Duration(o.AutoArchiveDays)*24*time.Hour,
	)
	if !o.ReadOnly {
		meta.ImportAutoApprove(filepath.Join(o.DataDir, "auto-approve.json"))
	}
	h.SetApprovalStore(meta)
	term := terminal.New("tmux", o.TmuxSocket+"-terminal", o.TerminalShell, logger)
	agents.SetDefaultBackend(defaultBackend)
	r := router.New(d, backends, defaultBackend, b, h, meta, term)
	r.SetSettings(cfg, filepath.Join(o.DataDir, "router"))

	// Web push: a second consumer of the broker/hook event seams, delivering
	// turn-done and permission notifications to subscribed browsers. On by
	// default but inert until a browser subscribes — which needs the user's
	// explicit notification-permission grant, and until then nothing is sent and
	// no push service is contacted. DisablePush skips it entirely (no VAPID
	// key, routes 404). A keypair failure disables push but doesn't stop serving.
	// Read-only skips it too: a companion instance sharing the live one's
	// data-dir would double-send to the same subscriptions.
	var pushMgr *push.Manager
	if !o.DisablePush && !o.ReadOnly {
		pushMgr, err = push.New(push.Config{
			KeyPath:   filepath.Join(o.DataDir, "vapid.json"),
			StorePath: filepath.Join(o.DataDir, "push-subscriptions.json"),
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

	// Scheduled tasks: each due task creates a session, so the runner needs
	// only the two seams the composer uses. Inert until a task is saved.
	// Read-only mode keeps the runner for GET /api/v1/schedules but never fires it.
	scheduleRunner := schedule.NewRunner(schedules, r, agents, logger)
	apiConfig := api.Config{
		Agents:         agents,
		Schedules:      scheduleRunner,
		AttachmentsDir: attachmentsDir,
		Version:        Version,
		ReadOnly:       o.ReadOnly,
	}
	themePath := filepath.Join(o.DataDir, "theme.css")
	proxy := web.ProxyAuth{Header: o.AuthHeader, LogoutURL: o.LogoutURL}
	srv := web.NewServer(hookSock, authStore, proxy, r, pushMgr, o.EditorURL, o.UIDir, themePath, o.AppName, apiConfig, logger)

	// Bind the hook socket before starting anything; it also catches an
	// usher started since the check above.
	if err := srv.Start(); err != nil {
		if errors.Is(err, pluginapi.ErrSocketInUse) {
			return nil, fmt.Errorf("%w: %s", ErrDataDirInUse, o.DataDir)
		}
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			_ = srv.Close()
		}
	}()

	if err := d.Start(ctx); err != nil {
		return nil, err
	}

	if pushMgr != nil {
		go pushMgr.Run(ctx)
	}

	logger.Info("auth",
		"configured", authStore.IsConfigured(),
		"loopback_bind", o.Addr == "" || addrIsLoopback(o.Addr),
	)

	if o.ReadOnly {
		logger.Info("read-only mode: web mutations rejected; schedule runner, telegram, plugin api, hook socket, and web push disabled")
	} else if err := startTelegramHub(ctx, r, o, logger); err != nil {
		return nil, err
	}

	if !o.ReadOnly {
		go scheduleRunner.Run(ctx)
	}

	// Plugin API: out-of-process IM frontends (e.g. usher-lark) consume the
	// same Router seam the Telegram hub uses, over a 0600 Unix socket. Inert
	// until a plugin connects. Read-only skips it: plugins drive sessions,
	// and the socket belongs to the live instance sharing the data dir.
	if !o.ReadOnly {
		go func() {
			if err := pluginapi.NewServer(r, apiConfig, logger).Run(ctx, pluginapi.SocketPath(o.DataDir)); err != nil && ctx.Err() == nil {
				logger.Warn("plugin api stopped", "err", err)
			}
		}()
	}

	// Foreign-turn watcher: turns usher didn't start (background workflow
	// continuations, pane-typed prompts) still have to refresh the transcript
	// views that are open on their session.
	go r.RunForeignWatch(ctx, 0)

	started = true
	app := &App{Options: o, Router: r, Broker: b, Interactions: h, Agents: agents, Logger: logger}
	return &Server{app: app, web: srv, auth: authStore, backends: backends}, nil
}

// startTelegramHub launches the Telegram forum mirror in a background goroutine
// when a bot token is set. No token disables it silently; a token without a
// group id is a misconfiguration and errors out.
func startTelegramHub(ctx context.Context, r *router.Router, o Options, logger *slog.Logger) error {
	if o.TelegramBotToken == "" {
		return nil // integration disabled
	}
	if o.TelegramGroupID == 0 {
		return fmt.Errorf("--telegram-group-id is required when %s is set", telegramTokenEnv)
	}
	hub, err := telegram.NewHub(
		telegram.NewClient(o.TelegramBotToken, ""),
		r,
		telegram.Config{
			GroupID:        o.TelegramGroupID,
			StatePath:      filepath.Join(o.DataDir, "telegram-topics.json"),
			AllowedUserIDs: o.TelegramAllowedUserIDs,
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

// exposed refuses a listener off loopback with nothing to sign a request
// in: no password, no proxy header, no --insecure. Unix sockets are local.
func exposed(addr net.Addr, o Options, a *auth.Store) error {
	if o.Insecure || o.AuthHeader != "" || a.IsConfigured() || addr.Network() == "unix" || addrIsLoopback(addr.String()) {
		return nil
	}
	return refuseExposed(addr.String())
}

func refuseExposed(addr string) error {
	return fmt.Errorf(
		"refusing to bind non-loopback %q without a password.\n"+
			"  run `usher set-password` to set one,\n"+
			"  or bind to 127.0.0.1 / localhost for local-only access,\n"+
			"  or pass --auth-header when an authenticating proxy is the only way in,\n"+
			"  or pass --insecure to skip this check (not recommended unless access is enforced elsewhere).",
		addr,
	)
}

// socketAnswers reports whether something listens on the Unix socket.
func socketAnswers(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
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

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// checkCLI warns at startup when an enabled backend's CLI is not on PATH,
// which would otherwise surface only at the first spawn.
func checkCLI(name, cmd string, readOnly bool, logger *slog.Logger) {
	if readOnly {
		return
	}
	path, err := exec.LookPath(cmd)
	if err != nil {
		logger.Warn("backend CLI not found; its sessions are read-only", "backend", name, "cmd", cmd, "PATH", os.Getenv("PATH"))
		return
	}
	logger.Debug("backend CLI", "backend", name, "path", path)
}

// sessionsDir reports whether dir exists, creating it when cmd is installed.
func sessionsDir(dir, cmd string, logger *slog.Logger) bool {
	if dir == "" {
		return false
	}
	if isDir(dir) {
		return true
	}
	if _, err := exec.LookPath(cmd); err != nil {
		return false
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Warn("sessions directory: create failed", "dir", dir, "err", err)
		return false
	}
	logger.Info("sessions directory created", "dir", dir)
	return true
}
