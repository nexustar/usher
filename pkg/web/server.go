// Package web serves the usher HTTP API and embedded static UI.
//
// Two kinds of listener run side by side:
//
//   - **web** listeners, any the caller passes to Serve, serve Handler: the
//     SPA, JSON API, SSE stream, and /login, behind auth middleware when a
//     password is configured or a proxy header is trusted.
//   - the **hook** listener (Unix socket at s.hookSockPath, mode 0600)
//     serves only /hook/{event}. Hook traffic from the local `usher hook`
//     subprocess never touches the public TCP port, so it doesn't need
//     auth and a leaky-netns container can't reach it (fs-isolated).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/pkg/api"
	"github.com/nexustar/usher/pkg/auth"
	"github.com/nexustar/usher/pkg/interaction"
	"github.com/nexustar/usher/pkg/pluginapi"
	"github.com/nexustar/usher/pkg/push"
	"github.com/nexustar/usher/pkg/router"
	"github.com/nexustar/usher/pkg/terminal"
)

//go:embed static
var staticFS embed.FS

//go:embed login.tmpl.html
var loginTemplateRaw string

var loginTmpl = template.Must(template.New("login").Parse(loginTemplateRaw))

// ProxyAuth signs a request in by the identity header an authenticating
// proxy sets, in place of the password. Nothing but the proxy may be able to
// reach the listener.
type ProxyAuth struct {
	// Header carries the signed-in user, e.g. X-Consumer-Username; empty
	// disables proxy auth.
	Header string
	// LogoutURL is where Sign out leads; empty hides the entry.
	LogoutURL string
}

type Server struct {
	hookSockPath string
	auth         *auth.Store
	proxy        ProxyAuth
	router       *router.Router
	api          *api.Server
	push         *push.Manager
	logger       *slog.Logger
	// editorURL is the --editor-url template for the "Open in editor" entry
	// in the session actions menu; "" hides the entry.
	editorURL string
	uiDir     string
	themePath string
	appName   string
	// readOnly rejects every state-changing web request (see
	// readOnlyMiddleware); reads, SSE, and static files stay available.
	readOnly bool

	build   sync.Once
	mux     *http.ServeMux
	handler http.Handler

	mu      sync.Mutex
	closed  bool
	servers []*http.Server // one per Serve
	hookSrv *http.Server
}

// Handle adds h at pattern to Handler. A duplicate pattern panics, as in
// ServeMux.
func (s *Server) Handle(pattern string, h http.Handler) {
	s.Handler()
	s.mux.Handle(pattern, h)
}

func NewServer(
	hookSockPath string,
	authStore *auth.Store,
	proxy ProxyAuth,
	r *router.Router,
	pushMgr *push.Manager,
	editorURL string,
	uiDir string,
	themePath string,
	appName string,
	c api.Config,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if appName == "" {
		appName = "usher"
	}
	c.Router = r
	return &Server{
		hookSockPath: hookSockPath,
		auth:         authStore,
		proxy:        proxy,
		router:       r,
		api:          api.New(c),
		push:         pushMgr,
		logger:       logger,
		editorURL:    editorURL,
		uiDir:        uiDir,
		themePath:    themePath,
		appName:      appName,
		readOnly:     c.ReadOnly,
	}
}

// Handler is the web surface behind its authentication.
func (s *Server) Handler() http.Handler {
	s.build.Do(s.buildHandler)
	return s.handler
}

func (s *Server) buildHandler() {
	webMux := http.NewServeMux()

	webMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	webMux.HandleFunc("GET /login", s.handleLogin)
	webMux.HandleFunc("POST /login", s.handleLogin)
	webMux.HandleFunc("GET /logout", s.handleLogout)
	webMux.HandleFunc("POST /logout", s.handleLogout)

	s.api.Register(webMux)
	webMux.HandleFunc("GET /internal/config", s.handleConfig)
	webMux.HandleFunc("POST /internal/router", s.handleStartRouter)
	webMux.HandleFunc("GET /internal/sessions/{id}/routed", s.handleRouted)
	webMux.HandleFunc("POST /internal/sessions/{id}/terminal", s.handleOpenTerminal)
	webMux.HandleFunc("DELETE /internal/sessions/{id}/terminal", s.handleCloseTerminal)
	webMux.HandleFunc("POST /internal/sessions/{id}/terminal/input", s.handleTerminalInput)
	webMux.HandleFunc("GET /internal/sessions/{id}/terminal/screen", s.handleTerminalScreen)
	webMux.HandleFunc("POST /internal/sessions/{id}/terminal/control", s.handleTerminalControl)
	webMux.HandleFunc("GET /internal/push/vapid-key", s.handlePushVAPIDKey)
	webMux.HandleFunc("POST /internal/push/subscribe", s.handlePushSubscribe)
	webMux.HandleFunc("POST /internal/push/unsubscribe", s.handlePushUnsubscribe)
	webMux.HandleFunc("GET /theme.css", s.handleTheme)

	var staticRoot fs.FS
	if s.uiDir != "" {
		staticRoot = os.DirFS(s.uiDir)
		s.logger.Info("serving UI from disk", "dir", s.uiDir)
	} else {
		staticRoot, _ = fs.Sub(staticFS, "static") // a constant, valid path
	}
	webMux.HandleFunc("GET /{$}", s.serveTemplated(staticRoot, "index.html", "text/html; charset=utf-8"))
	webMux.HandleFunc("GET /manifest.webmanifest", s.serveTemplated(staticRoot, "manifest.webmanifest", "application/manifest+json"))
	webMux.Handle("GET /", http.FileServer(http.FS(staticRoot)))

	var apiHandler http.Handler = webMux
	if s.readOnly {
		apiHandler = readOnlyMiddleware(webMux)
	}
	s.mux = webMux
	s.handler = s.hostGuard(contentSecurityPolicy(gzipMiddleware(crossOriginGuard(s.authMiddleware(apiHandler)))))
}

// Start binds the hook socket, except read-only: it spawns no sessions, and
// the socket belongs to the live instance sharing its data dir.
func (s *Server) Start() error {
	if s.readOnly {
		return nil
	}
	hookMux := http.NewServeMux()
	hookMux.HandleFunc("POST /hook/{event}", s.handleHook)
	ln, err := pluginapi.ListenUnixSocket(s.hookSockPath)
	if err != nil {
		return fmt.Errorf("hook socket: %w", err)
	}
	srv := &http.Server{Handler: hookMux, ReadHeaderTimeout: 5 * time.Second}
	s.mu.Lock()
	s.hookSrv = srv
	s.mu.Unlock()
	go func() {
		s.logger.Info("usher hook listening", "socket", s.hookSockPath)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Warn("hook socket stopped", "err", err)
		}
	}()
	return nil
}

// Serve serves Handler on ln until Close; after Close it returns
// http.ErrServerClosed at once.
func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return http.ErrServerClosed
	}
	s.servers = append(s.servers, srv)
	s.mu.Unlock()
	s.logger.Info("usher web listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Close stops every Serve and the hook socket.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.mu.Lock()
	servers, hook := s.servers, s.hookSrv
	s.closed, s.servers, s.hookSrv = true, nil, nil
	s.mu.Unlock()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
	if hook != nil {
		_ = hook.Shutdown(ctx)
		_ = os.Remove(s.hookSockPath)
	}
	return nil
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

// hostGuard refuses host names other than localhost while nothing signs
// requests in. A page whose DNS name is rebound to a loopback listener is
// same-origin with it, so neither the cross-origin guard nor a cookie would
// stop it.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.passwordConfigured() && s.proxy.Header == "" && !localHost(r.Host) {
			api.WriteError(w, http.StatusForbidden, "host "+r.Host+" is refused without a password; open usher by IP or localhost, or set a password")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localHost reports whether a Host header names an IP address or localhost —
// names that DNS rebinding cannot take over.
func localHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" || net.ParseIP(host) != nil {
		return true
	}
	host = strings.ToLower(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// contentSecurityPolicy keeps the page from loading images off-origin, a
// channel a prompt-injected agent could send data out through.
func contentSecurityPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "img-src 'self' data: blob:")
		next.ServeHTTP(w, r)
	})
}

// crossOriginGuard rejects state-changing requests a browser sent from
// another origin, another port on the same host included.
func crossOriginGuard(next http.Handler) http.Handler {
	p := http.NewCrossOriginProtection()
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.WriteError(w, http.StatusForbidden, "cross-origin request rejected")
	}))
	return p.Handler(next)
}

// --- auth middleware + handlers -----------------------------------------

func (s *Server) passwordConfigured() bool {
	return s.auth != nil && s.auth.IsConfigured()
}

// authMiddleware gates every web route on the proxy header when one is
// trusted, and on a valid cookie when a password is configured: both, with
// both. With neither it passes everything through.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAuthExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.proxy.Header != "" && !s.proxySignedIn(w, r) {
			return
		}
		if !s.passwordConfigured() {
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
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/internal/") {
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

// proxySignedIn reports whether r carries the proxy header, answering 401
// itself when it does not.
func (s *Server) proxySignedIn(w http.ResponseWriter, r *http.Request) bool {
	actor := strings.TrimSpace(r.Header.Get(s.proxy.Header))
	if actor == "" {
		api.WriteError(w, http.StatusUnauthorized, "no signed-in user from the proxy")
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.logger.Info("request", "actor", actor, "method", r.Method, "path", r.URL.Path)
	}
	return true
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
	if !s.passwordConfigured() {
		if s.proxy.Header != "" {
			// The SPA lands here on a 401; going back through the proxy
			// signs the browser in again.
			http.Redirect(w, r, validateNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return
		}
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
		AppName string
		Error   string
		Next    string
	}{
		AppName: s.appName,
		Error:   errMsg,
		Next:    validateNext(next),
	})
}

// handleLogout drops the password's cookie, then signs out of the proxy
// when it has a logout address.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		http.SetCookie(w, s.auth.ClearCookie())
	}
	target := "/login"
	switch {
	case s.proxy.Header != "" && s.proxy.LogoutURL != "":
		target = s.proxy.LogoutURL
	case s.proxy.Header != "" && !s.passwordConfigured():
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// --- sessions ------------------------------------------------------------

// handleConfig exposes the few server-side settings the SPA needs to render
// optional UI.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, struct {
		EditorURL string       `json:"editor_url"`
		LogoutURL string       `json:"logout_url"`
		ReadOnly  bool         `json:"read_only"`
		Router    routerConfig `json:"router"`
	}{s.editorURL, s.logoutURL(), s.readOnly, s.routerConfig()})
}

// logoutURL is where Sign out leads; "" hides the entry.
func (s *Server) logoutURL() string {
	switch {
	case s.passwordConfigured():
		return "/logout" // drops the cookie, then the proxy's logout
	case s.proxy.Header != "":
		return s.proxy.LogoutURL
	}
	return ""
}

// routerConfig is the current router session — session_id is empty until a
// first message creates one — and the cwd router sessions run in.
type routerConfig struct {
	SessionID string `json:"session_id"`
	Dir       string `json:"dir"`
}

func (s *Server) routerConfig() routerConfig {
	return routerConfig{s.router.RouterSession(), s.router.RouterDir()}
}

// startRouterRequest opens a router conversation. An empty Backend or Model
// leaves that choice to usher's defaults.
type startRouterRequest struct {
	InitialMessage string `json:"initial_message"`
	Backend        string `json:"backend"`
	Model          string `json:"model"`
}

// handleStartRouter creates the router session, replacing the current one.
func (s *Server) handleStartRouter(w http.ResponseWriter, r *http.Request) {
	var req startRouterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Model == "default" {
		req.Model = ""
	}
	id, err := s.router.StartRouter(req.InitialMessage, req.Backend, req.Model)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.api.WriteCreated(w, r, id)
}

// handleRouted resolves the replies to the work a session routed elsewhere, for
// the turns from ?from= on (the newest page without it).
func (s *Server) handleRouted(w http.ResponseWriter, r *http.Request) {
	replies, err := s.router.RoutedRepliesFrom(r.PathValue("id"), r.URL.Query().Get("from"))
	switch {
	case errors.Is(err, router.ErrSessionNotFound):
		api.WriteError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, replies)
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
