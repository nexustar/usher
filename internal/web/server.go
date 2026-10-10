// Package web serves the usher HTTP API and embedded static UI.
//
// Two listeners run side by side:
//
//   - the **web** listener (TCP at s.addr) serves the SPA, JSON API, SSE
//     stream, and /login. Every non-exempt request is gated by auth
//     middleware when a password is configured.
//   - the **hook** listener (Unix socket at s.hookSockPath, mode 0600)
//     serves only /hook/{event}. Hook traffic from the local `usher hook`
//     subprocess never touches the public TCP port, so it doesn't need
//     auth and a leaky-netns container can't reach it (fs-isolated).
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/agent/usheragent"
	"github.com/nexustar/usher/internal/agentprofile"
	"github.com/nexustar/usher/internal/api"
	"github.com/nexustar/usher/internal/auth"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/mainchat"
	"github.com/nexustar/usher/internal/pathutil"
	"github.com/nexustar/usher/internal/pluginapi"
	"github.com/nexustar/usher/internal/push"
	"github.com/nexustar/usher/internal/router"
	"github.com/nexustar/usher/internal/schedule"
	"github.com/nexustar/usher/internal/terminal"
)

//go:embed static
var staticFS embed.FS

//go:embed login.tmpl.html
var loginTemplateRaw string

var loginTmpl = template.Must(template.New("login").Parse(loginTemplateRaw))

func init() {
	// Go's default MIME table has no .webmanifest entry; without this the
	// FileServer would serve the PWA manifest as application/octet-stream and
	// browsers would reject it.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Verify Router satisfies AgentAPI at compile time.
var _ usheragent.AgentAPI = (*router.Router)(nil)

type Server struct {
	addr           string
	hookSockPath   string
	attachmentsDir string
	auth           *auth.Store
	router         *router.Router
	main           *mainchat.Store
	agent          usheragent.Agent
	push           *push.Manager
	logger         *slog.Logger
	// editorURL is the --editor-url template for the "Open in editor" entry
	// in the session actions menu; "" hides the entry.
	editorURL string
	uiDir     string
	themePath string
	agents    *agentprofile.Store
	schedules *schedule.Runner
	// readOnly rejects every state-changing web request (see
	// readOnlyMiddleware); reads, SSE, and static files stay available.
	readOnly bool

	// Main-chat delivery. The user message is persisted in the POST handler
	// (202 means durable); the agent turn then runs on the chat's single
	// worker goroutine, fed by a bounded FIFO queue. Everything the chat
	// displays reaches clients through the per-chat SSE stream; chatSubs
	// maps each subscriber channel to its cancel so a slow subscriber is
	// force-closed rather than silently dropped (reconnect + refetch is the
	// only gap-healing path).
	chatMu      sync.Mutex
	chatSubs    map[string]map[chan chatFrame]func()
	chatQueues  map[string]chan mainchat.Message
	chatPending map[string]int // reserved turn-queue slots (see tryReserveTurn)
}

func NewServer(
	addr string,
	hookSockPath string,
	attachmentsDir string,
	authStore *auth.Store,
	r *router.Router,
	main *mainchat.Store,
	agent usheragent.Agent,
	pushMgr *push.Manager,
	editorURL string,
	uiDir string,
	themePath string,
	agents *agentprofile.Store,
	schedules *schedule.Runner,
	readOnly bool,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		addr:           addr,
		hookSockPath:   hookSockPath,
		attachmentsDir: attachmentsDir,
		auth:           authStore,
		router:         r,
		main:           main,
		agent:          agent,
		push:           pushMgr,
		logger:         logger,
		editorURL:      editorURL,
		uiDir:          uiDir,
		themePath:      themePath,
		agents:         agents,
		schedules:      schedules,
		readOnly:       readOnly,
		chatSubs:       map[string]map[chan chatFrame]func(){},
		chatQueues:     map[string]chan mainchat.Message{},
		chatPending:    map[string]int{},
	}
}

// handleModels feeds the new-session picker from each backend's model catalog.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	backends := s.router.Backends()
	models := make(map[string][]backend.Model, len(backends))
	for _, name := range backends {
		catalog, err := s.router.Models(r.Context(), name)
		if err == nil {
			models[name] = catalog
		}
	}
	api.WriteJSON(w, http.StatusOK, struct {
		Backends []string                   `json:"backends"`
		Models   map[string][]backend.Model `json:"models"`
	}{
		Backends: backends,
		Models:   models,
	})
}

func (s *Server) Run(ctx context.Context) error {
	webMux := http.NewServeMux()

	webMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	webMux.HandleFunc("GET /login", s.handleLogin)
	webMux.HandleFunc("POST /login", s.handleLogin)
	webMux.HandleFunc("GET /logout", s.handleLogout)
	webMux.HandleFunc("POST /logout", s.handleLogout)

	api.New(s.router, s.agents, s.attachmentsDir).Register(webMux, "/api")
	webMux.HandleFunc("POST /api/uploads", s.handleDraftUpload)
	webMux.HandleFunc("GET /api/models", s.handleModels)
	webMux.HandleFunc("GET /api/agents", s.handleAgents)
	webMux.HandleFunc("POST /api/agents", s.handleCreateAgent)
	webMux.HandleFunc("PUT /api/agents/{name}", s.handleUpdateAgent)
	webMux.HandleFunc("DELETE /api/agents/{name}", s.handleDeleteAgent)
	webMux.HandleFunc("GET /api/sessions/{id}/commands", s.handleSessionCommands)
	webMux.HandleFunc("POST /api/sessions/{id}/pause", s.handlePauseSession)
	webMux.HandleFunc("POST /api/sessions/{id}/resume", s.handleResumeSession)
	webMux.HandleFunc("POST /api/sessions/{id}/terminal", s.handleOpenTerminal)
	webMux.HandleFunc("DELETE /api/sessions/{id}/terminal", s.handleCloseTerminal)
	webMux.HandleFunc("POST /api/sessions/{id}/terminal/input", s.handleTerminalInput)
	webMux.HandleFunc("GET /api/sessions/{id}/terminal/screen", s.handleTerminalScreen)
	webMux.HandleFunc("GET /api/sessions/{id}/image", s.handleSessionImage)
	webMux.HandleFunc("POST /api/sessions/{id}/terminal/control", s.handleTerminalControl)

	webMux.HandleFunc("GET /api/mainchats", s.handleListMainChats)
	webMux.HandleFunc("GET /api/mainchats/{id}", s.handleGetMainChat)
	webMux.HandleFunc("GET /api/mainchats/{id}/messages", s.handleListMainChatMessages)
	webMux.HandleFunc("GET /api/mainchats/{id}/events", s.handleMainChatEvents)
	webMux.HandleFunc("POST /api/mainchats/{id}/send", s.handleMainChatSend)

	webMux.HandleFunc("GET /api/schedules", s.handleSchedules)
	webMux.HandleFunc("POST /api/schedules", s.handleCreateSchedule)
	webMux.HandleFunc("PUT /api/schedules/{id}", s.handleUpdateSchedule)
	webMux.HandleFunc("DELETE /api/schedules/{id}", s.handleDeleteSchedule)
	webMux.HandleFunc("POST /api/schedules/{id}/run", s.handleRunSchedule)

	webMux.HandleFunc("GET /api/config", s.handleConfig)
	webMux.HandleFunc("GET /api/sessions/{id}/routed", s.handleRouted)
	webMux.HandleFunc("POST /api/router", s.handleStartRouter)
	webMux.HandleFunc("PUT /api/router", s.handleSetRouterAgent)
	webMux.HandleFunc("GET /theme.css", s.handleTheme)

	webMux.HandleFunc("GET /api/push/vapid-key", s.handlePushVAPIDKey)
	webMux.HandleFunc("POST /api/push/subscribe", s.handlePushSubscribe)
	webMux.HandleFunc("POST /api/push/unsubscribe", s.handlePushUnsubscribe)

	var staticRoot fs.FS
	if s.uiDir != "" {
		staticRoot = os.DirFS(s.uiDir)
		s.logger.Info("serving UI from disk", "dir", s.uiDir)
	} else {
		var err error
		staticRoot, err = fs.Sub(staticFS, "static")
		if err != nil {
			return err
		}
	}
	webMux.Handle("GET /", http.FileServer(http.FS(staticRoot)))

	var apiHandler http.Handler = webMux
	if s.readOnly {
		apiHandler = readOnlyMiddleware(webMux)
	}
	webSrv := &http.Server{
		Addr:              s.addr,
		Handler:           gzipMiddleware(s.authMiddleware(apiHandler)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	webListener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("web listen %s: %w", s.addr, err)
	}

	errCh := make(chan error, 2)

	// Read-only never binds the hook socket: it spawns no sessions so no
	// hook can arrive, and on a data-dir shared with a live instance binding
	// would steal that instance's socket (ListenUnixSocket unlinks it).
	var hookSrv *http.Server
	if !s.readOnly {
		hookMux := http.NewServeMux()
		hookMux.HandleFunc("POST /hook/{event}", s.handleHook)
		sockListener, err := pluginapi.ListenUnixSocket(s.hookSockPath)
		if err != nil {
			_ = webListener.Close()
			return fmt.Errorf("hook socket: %w", err)
		}
		hookSrv = &http.Server{
			Handler:           hookMux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			s.logger.Info("usher hook listening", "socket", s.hookSockPath)
			errCh <- hookSrv.Serve(sockListener)
		}()
	}

	go func() {
		s.logger.Info("usher web listening", "addr", s.addr)
		errCh <- webSrv.Serve(webListener)
	}()

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = webSrv.Shutdown(shutdownCtx)
		if hookSrv != nil {
			_ = hookSrv.Shutdown(shutdownCtx)
			_ = os.Remove(s.hookSockPath)
		}
	}

	select {
	case <-ctx.Done():
		shutdown()
		return nil
	case err := <-errCh:
		shutdown()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handleTheme serves the operator-supplied CSS outside the embedded static
// filesystem. Opening it per request lets edits take effect on page refresh.
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	f, err := os.Open(s.themePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || s.themePath == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		s.logger.Error("open theme", "path", s.themePath, "err", err)
		http.Error(w, "theme unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.logger.Error("stat theme", "path", s.themePath, "err", err)
		http.Error(w, "theme unavailable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, "theme.css", info.ModTime(), f)
}

// readOnlyMiddleware (installed only under --read-only) rejects state-changing
// requests. Enforcement is by method — every mutating route is POST/PUT/DELETE,
// so new GET routes stay readable without touching this; keep that discipline
// when adding routes. Login/logout stay allowed so a password-protected
// read-only instance remains reachable.
func readOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
		case r.URL.Path == "/login" || r.URL.Path == "/logout":
		default:
			api.WriteError(w, http.StatusForbidden, "this usher instance is read-only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- auth middleware + handlers -----------------------------------------

// authMiddleware gates every web route on a valid cookie when a password
// is configured. When auth is not configured (loopback-only test mode) it
// is a no-op pass-through. /healthz, /login, and /logout are always
// exempt.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil || !s.auth.IsConfigured() {
			next.ServeHTTP(w, r)
			return
		}
		if isAuthExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(auth.CookieName)
		if err == nil && s.auth.VerifyCookie(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		// API/XHR clients get 401; full-page navigations get a redirect to
		// /login so the user lands on the form instead of seeing JSON.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			api.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		target := "/login"
		if r.URL.Path != "/" && r.URL.Path != "" {
			target = "/login?next=" + safeNextEscape(r.URL.RequestURI())
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	})
}

func isAuthExempt(p string) bool {
	switch p {
	case "/healthz", "/login", "/logout",
		// PWA install assets must be reachable without auth. Browsers fetch
		// the manifest, service worker, and icons to evaluate installability
		// (sometimes before or independent of the page's auth state); a 303
		// to /login here gets followed and cached by the SW as if it were
		// the manifest, permanently breaking install. None of these files
		// contain user data.
		"/manifest.webmanifest", "/sw.js":
		return true
	}
	return strings.HasPrefix(p, "/icons/")
}

// safeNextEscape URL-encodes a path for use in ?next=. We don't accept
// scheme/authority; callers should already have a relative path.
func safeNextEscape(p string) string {
	// http.Redirect and the browser will handle the rest; just keep the
	// payload safe to embed in a query string.
	r := strings.NewReplacer("&", "%26", "?", "%3F", "#", "%23", " ", "%20")
	return r.Replace(p)
}

// validateNext rejects open redirects: anything other than a same-origin
// absolute path collapses to "/".
func validateNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") {
		return "/"
	}
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.IsConfigured() {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		s.renderLogin(w, r, "", http.StatusOK, r.URL.Query().Get("next"))
		return
	}
	// POST
	ip := auth.ClientIP(r)
	if delay, ok := s.auth.Limiter.Acquire(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(delay.Seconds())+1))
		s.renderLogin(w, r,
			fmt.Sprintf("too many attempts; try again in %ds", int(delay.Seconds())+1),
			http.StatusTooManyRequests,
			r.FormValue("next"))
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, "invalid form submission", http.StatusBadRequest, "")
		return
	}
	pw := r.PostForm.Get("password")
	next := r.PostForm.Get("next")
	if !s.auth.Verify(pw) {
		s.auth.Limiter.OnFailure(ip)
		s.logger.Info("login failed", "ip", ip)
		s.renderLogin(w, r, "invalid password", http.StatusUnauthorized, next)
		return
	}
	s.auth.Limiter.OnSuccess(ip)
	cookieVal, err := s.auth.IssueCookie()
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, s.auth.NewSessionCookie(cookieVal))
	http.Redirect(w, r, validateNext(next), http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string, status int, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, struct {
		Error string
		Next  string
	}{
		Error: errMsg,
		Next:  validateNext(next),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		http.SetCookie(w, s.auth.ClearCookie())
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- sessions ------------------------------------------------------------

func (s *Server) handleAgents(w http.ResponseWriter, _ *http.Request) {
	api.WriteJSON(w, http.StatusOK, map[string]any{"agents": s.agents.List()})
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var profile agentprofile.Profile
	if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	created, err := s.agents.Create(profile)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	var profile agentprofile.Profile
	if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	updated, err := s.agents.Update(r.PathValue("name"), profile)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.agents.Delete(r.PathValue("name")); err != nil {
		api.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scheduleDTO adds the next due time, which the server derives and a write
// never carries.
type scheduleDTO struct {
	schedule.Task
	NextRun time.Time `json:"next_run,omitzero"`
}

// validateScheduleTarget rejects a task that could not create a session. The
// agent is re-resolved at fire time, so this is a courtesy, not a guarantee —
// but it catches typos that would otherwise surface at 3am as a failed run.
func (s *Server) validateScheduleTarget(task schedule.Task) error {
	opts, err := s.agents.Resolve(task.Agent, task.Cwd, task.Backend, task.Model)
	if err != nil {
		return err
	}
	if opts.Cwd == "" {
		return errors.New("cwd is required (set one here or on the agent)")
	}
	return nil
}

func (s *Server) handleSchedules(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	tasks := s.schedules.Store().List()
	out := make([]scheduleDTO, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, scheduleDTO{Task: task, NextRun: task.NextRun(now)})
	}
	// The zone rides with the list, not /api/config: it is only meaningful next
	// to these timestamps, and the pane cannot render one before knowing it.
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"schedules": out,
		"timezone":  schedule.LocalZone(now),
	})
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var task schedule.Task
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := s.validateScheduleTarget(task); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.schedules.Store().Create(task)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusCreated, scheduleDTO{Task: created, NextRun: created.NextRun(time.Now())})
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	var task schedule.Task
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := s.validateScheduleTarget(task); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.schedules.Store().Update(r.PathValue("id"), task)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, scheduleDTO{Task: updated, NextRun: updated.NextRun(time.Now())})
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.schedules.Store().Delete(r.PathValue("id")); err != nil {
		api.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRunSchedule starts a task's session now, answering once the id is
// known (like POST /api/sessions) since the caller is testing the task.
func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := s.schedules.RunNow(r.PathValue("id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusAccepted, map[string]string{"session_id": id})
}

// handleConfig exposes the few server-side settings the SPA needs to render
// optional UI.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, struct {
		EditorURL string       `json:"editor_url"`
		ReadOnly  bool         `json:"read_only"`
		Router    routerConfig `json:"router"`
	}{s.editorURL, s.readOnly, routerConfig{s.router.RouterAgent(), s.router.RouterSession()}})
}

// routerConfig is the router agent setting and the current router session;
// session_id is empty until a first message creates one.
type routerConfig struct {
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
}

type startRouterRequest struct {
	InitialMessage string `json:"initial_message"`
}

// handleStartRouter creates a session from the router agent and makes it the
// router, replacing the current one.
func (s *Server) handleStartRouter(w http.ResponseWriter, r *http.Request) {
	var req startRouterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	opts, err := s.agents.ResolveRouter(s.router.RouterAgent(), s.router.DefaultBackend())
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts.InitialMessage = req.InitialMessage
	id, err := s.router.StartRouter(opts)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

// handleRouted resolves the replies to the work a session routed elsewhere, for
// the turns from ?from= on (the newest page without it).
func (s *Server) handleRouted(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var turns []core.Turn
	var err error
	if from := r.URL.Query().Get("from"); from != "" {
		turns, err = s.router.ReadTurnsFrom(id, from)
	} else {
		turns, _, err = s.router.ReadTurns(id, "", 50)
	}
	switch {
	case errors.Is(err, router.ErrSessionNotFound):
		api.WriteError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, s.router.RoutedReplies(turns))
}

type routerAgentRequest struct {
	Agent string `json:"agent"`
}

// handleSetRouterAgent picks the agent the next router session is created
// from; the current router is left as it is.
func (s *Server) handleSetRouterAgent(w http.ResponseWriter, r *http.Request) {
	var req routerAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if _, err := s.agents.ResolveRouter(req.Agent, ""); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.router.SetRouterAgent(req.Agent)
	api.WriteJSON(w, http.StatusOK, routerConfig{req.Agent, s.router.RouterSession()})
}

func (s *Server) handleSessionCommands(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.router.GetSession(id)
	if !ok {
		api.WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	catalog, err := s.router.ComposerItems(r.Context(), id, sess.Cwd)
	if err != nil {
		api.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	if catalog.Items == nil {
		catalog.Items = []backend.ComposerItem{}
	}
	api.WriteJSON(w, http.StatusOK, struct {
		Source    string                 `json:"source"`
		Available bool                   `json:"available"`
		Commands  []backend.ComposerItem `json:"commands"`
	}{Source: sess.Backend, Available: catalog.Available, Commands: catalog.Items})
}

// handlePauseSession tears down the session's live worker without deleting
// anything — the conversation stays on disk and resumes on the next send.
// Non-destructive and distinct from DELETE (which removes the jsonl). Returns
// 404 for an unknown id.
func (s *Server) handlePauseSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.router.PauseSession(id); err != nil {
		api.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]bool{"paused": true})
}

// handleResumeSession starts an idle backend worker without adding a user turn.
func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.router.ResumeSession(r.Context(), id); err != nil {
		if errors.Is(err, router.ErrSessionNotFound) {
			api.WriteError(w, http.StatusNotFound, err.Error())
			return
		}
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]bool{"resumed": true})
}

// imageContentTypes is the /image allowlist (also the forced Content-Type).
// Raster only — SVG is excluded because directly navigating to a same-origin SVG
// would execute its scripts in usher's authenticated origin (an <img> load is
// inert, but the endpoint must be safe under direct navigation too).
var imageContentTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// handleSessionImage serves an image file from a session's working directory or
// /tmp. Auth is the surrounding cookie middleware.
func (s *Server) handleSessionImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.router.GetSession(id)
	if !ok {
		api.WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		api.WriteError(w, http.StatusBadRequest, "missing path")
		return
	}
	ctype, ok := imageContentTypes[strings.ToLower(filepath.Ext(rel))]
	if !ok {
		api.WriteError(w, http.StatusForbidden, "not a supported image type")
		return
	}
	// Beyond the cwd: this session's uploads, the pre-session uploads its
	// first message points at, Codex's generated images.
	full, ok := pathutil.ResolveImagePath(sess.Cwd, rel,
		filepath.Join(s.attachmentsDir, id),
		filepath.Join(s.attachmentsDir, draftsDir),
		pathutil.CodexGeneratedImagesDir(id))
	if !ok {
		api.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		api.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=60")
	http.ServeContent(w, r, filepath.Base(full), info.ModTime(), f)
}

// draftsDir holds uploads made before a session exists, one subdirectory per
// upload. Session ids are UUIDs, so it can't collide with a session's own.
const draftsDir = "_drafts"

// handleDraftUpload does the same for the new-session composer, which has no
// id to file the upload under yet. Nothing relocates it afterwards: the id is
// minted in the same call that sends the initial message, which already
// carries the path. One directory per upload keeps the original filename.
func (s *Server) handleDraftUpload(w http.ResponseWriter, r *http.Request) {
	var b [8]byte
	_, _ = rand.Read(b[:]) // documented never to fail
	api.SaveUpload(w, r, filepath.Join(s.attachmentsDir, draftsDir), fmt.Sprintf("%x", b[:]))
}

// --- session terminal ----------------------------------------------------

const (
	terminalPollInterval = 500 * time.Millisecond
	terminalHeartbeat    = 15 * time.Second
)

// terminalControls is the fixed set of keys accepted by the HTTP API.
var terminalControls = map[string]string{
	"up":     "Up",
	"down":   "Down",
	"left":   "Left",
	"right":  "Right",
	"enter":  "Enter",
	"escape": "Escape",
	"tab":    "Tab",
	"ctrl-c": "C-c",
	"ctrl-z": "C-z",
	"ctrl-d": "C-d",
	"ctrl-x": "C-x",
	"ctrl-o": "C-o",
	"ctrl-w": "C-w",
	"ctrl-k": "C-k",
	"ctrl-u": "C-u",
}

func terminalErrStatus(err error) int {
	switch {
	case errors.Is(err, router.ErrSessionNotFound):
		return http.StatusNotFound
	case errors.Is(err, terminal.ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusConflict
	}
}

type terminalOpenRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

func (s *Server) handleOpenTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req terminalOpenRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	if req.Cols == 0 {
		req.Cols = 80
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	req.Cols = clampInt(req.Cols, 60, 240)
	req.Rows = clampInt(req.Rows, 4, 200)
	if err := s.router.OpenTerminal(id, req.Cols, req.Rows); err != nil {
		api.WriteError(w, terminalErrStatus(err), err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]bool{"open": true})
}

func (s *Server) handleCloseTerminal(w http.ResponseWriter, r *http.Request) {
	if err := s.router.CloseTerminal(r.PathValue("id")); err != nil {
		api.WriteError(w, terminalErrStatus(err), err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]bool{"open": false})
}

type terminalInputRequest struct {
	Text      string `json:"text"`
	RequestID string `json:"request_id"`
}

func (s *Server) handleTerminalInput(w http.ResponseWriter, r *http.Request) {
	const maxTerminalInput = 64 << 10
	r.Body = http.MaxBytesReader(w, r.Body, maxTerminalInput)
	var req terminalInputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		api.WriteError(w, http.StatusBadRequest, "terminal input is empty")
		return
	}
	if req.RequestID == "" || len(req.RequestID) > 128 {
		api.WriteError(w, http.StatusBadRequest, "invalid terminal request_id")
		return
	}
	if strings.IndexByte(req.Text, 0) >= 0 {
		api.WriteError(w, http.StatusBadRequest, "terminal input contains NUL")
		return
	}
	if err := s.router.SubmitTerminal(r.PathValue("id"), req.RequestID, req.Text); err != nil {
		api.WriteError(w, terminalErrStatus(err), err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleTerminalScreen streams changed tmux snapshots over SSE.
func (s *Server) handleTerminalScreen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		api.WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	if !s.router.TerminalAvailable() {
		api.WriteError(w, http.StatusServiceUnavailable, "terminal unavailable: tmux is not installed")
		return
	}
	if !s.router.HasTerminal(id) {
		api.WriteError(w, http.StatusConflict, "terminal is not open")
		return
	}
	flusher, ok := api.SSEStart(w)
	if !ok {
		return
	}

	// Size the pane to the viewer (cols + rows measured/derived client-side) so
	// the screen fills the panel without wrapping. The server clamps to wide
	// defensive bounds so a bad client cannot request an absurd canvas.
	cols := 80
	if v := r.URL.Query().Get("cols"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cols = n
		}
	}
	rows := 24
	if v := r.URL.Query().Get("rows"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rows = n
		}
	}
	cols = clampInt(cols, 60, 240)
	rows = clampInt(rows, 4, 200)
	_ = s.router.ResizeTerminal(id, cols, rows)

	ticker := time.NewTicker(terminalPollInterval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(terminalHeartbeat)
	defer heartbeat.Stop()

	lastFrame := ""
	lastErr := false
	// emit captures one frame and writes it if it changed. Returns false only
	// when the connection is gone (a write failed), to end the stream.
	emit := func() bool {
		screen, err := s.router.CaptureTerminal(id)
		if err != nil {
			// Only a confirmed-gone window means "closed"; a transient tmux
			// failure just skips this frame so one hiccup doesn't flip the
			// client's terminal off while the shell is still alive.
			if !errors.Is(err, terminal.ErrNotOpen) {
				return true
			}
			if lastErr {
				return true // already told the client; don't repeat
			}
			lastErr, lastFrame = true, ""
			if _, werr := fmt.Fprint(w, "event: closed\ndata: {}\n\n"); werr != nil {
				return false
			}
			flusher.Flush()
			return true
		}
		lastErr = false
		if screen == lastFrame {
			return true
		}
		lastFrame = screen
		payload, _ := json.Marshal(screen) // JSON-encode: escapes the ESC + newlines for a single SSE data line
		if _, werr := fmt.Fprintf(w, "event: screen\ndata: %s\n\n", payload); werr != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !emit() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if !emit() {
				return
			}
		}
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type terminalControlRequest struct {
	Control string `json:"control"`
}

func (s *Server) handleTerminalControl(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req terminalControlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	tmuxKey, ok := terminalControls[req.Control]
	if !ok {
		api.WriteError(w, http.StatusBadRequest, "unknown terminal control")
		return
	}
	if err := s.router.SendTerminalControl(id, tmuxKey); err != nil {
		api.WriteError(w, terminalErrStatus(err), err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- interactions ---------------------------------------------------------------

type hookPayload struct {
	SessionID      string          `json:"session_id"`
	ToolUseID      string          `json:"tool_use_id"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	Cwd            string          `json:"cwd"`
	TranscriptPath string          `json:"transcript_path"`
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	eventName := r.PathValue("event")
	// Upgrade bridge: an old hook binary may POST here after the server has
	// switched to native backend approval protocols. Return no decision so
	// Claude falls through to stdio can_use_tool and Codex to app-server. Keep
	// this endpoint for one rolling-upgrade window; it never creates a pending
	// interaction.
	if eventName == "PermissionRequest" {
		api.WriteJSON(w, http.StatusOK, map[string]any{"continue": true})
		return
	}

	body, _ := io.ReadAll(r.Body)
	var ev hookPayload
	if len(body) > 0 {
		if err := json.Unmarshal(body, &ev); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid hook payload: "+err.Error())
			return
		}
	}
	if ev.HookEventName == "" {
		ev.HookEventName = eventName
	}
	// Managed Claude permissions use the stream-json control protocol. The only
	// command hook still routed over HTTP is AskUserQuestion's PreToolUse.
	if eventName != "PreToolUse" {
		api.WriteJSON(w, http.StatusOK, map[string]any{})
		return
	}

	resp, err := s.router.HandleHook(r.Context(), interaction.Request{
		SessionID:   ev.SessionID,
		ToolUseID:   ev.ToolUseID,
		ToolName:    ev.ToolName,
		ToolInput:   ev.ToolInput,
		Cwd:         ev.Cwd,
		AllowAlways: false,
	})
	if err != nil {
		s.logger.Warn("hook submit cancelled", "session", ev.SessionID, "err", err)
		api.WriteJSON(w, http.StatusOK, map[string]any{})
		return
	}

	decision := resp.Behavior
	if decision == "" {
		decision = "allow"
	}

	hookOut := map[string]any{
		"hookEventName":            eventName,
		"permissionDecision":       decision,
		"permissionDecisionReason": resp.Reason,
	}
	// An AskUserQuestion answer comes back as Answers (question → chosen
	// label). Claude resolves the tool from the hook's updatedInput, so we
	// echo the original tool input with the answers merged in — the tool then
	// completes without ever rendering its pane TUI selector.
	if len(resp.Answers) > 0 {
		var ti map[string]any
		if err := json.Unmarshal(ev.ToolInput, &ti); err != nil || ti == nil {
			ti = map[string]any{}
		}
		ti["answers"] = resp.Answers
		hookOut["updatedInput"] = ti
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"hookSpecificOutput": hookOut})
}

// --- web push ------------------------------------------------------------

// handlePushVAPIDKey returns the applicationServerKey the browser passes to
// pushManager.subscribe(). 404 when push isn't available so the client can hide
// the notifications toggle.
func (s *Server) handlePushVAPIDKey(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		api.WriteError(w, http.StatusNotFound, "push not available")
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]string{"key": s.push.VAPIDPublicKey()})
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		api.WriteError(w, http.StatusNotFound, "push not available")
		return
	}
	var sub push.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := s.push.Subscribe(sub); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "subscribed"})
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		api.WriteError(w, http.StatusNotFound, "push not available")
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	s.push.Unsubscribe(req.Endpoint)
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "unsubscribed"})
}
