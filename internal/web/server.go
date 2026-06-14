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
	"sort"
	"strconv"
	"strings"
	"time"

	"usher/internal/agent/usheragent"
	"usher/internal/auth"
	"usher/internal/core"
	"usher/internal/hook"
	"usher/internal/jsonl"
	"usher/internal/mainchat"
	"usher/internal/router"
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
	addr         string
	hookSockPath string
	auth         *auth.Store
	router       *router.Router
	main         *mainchat.Store
	agent        usheragent.Agent
	logger       *slog.Logger
	// codexModelsPath is ~/.codex/models_cache.json (codex's per-account model
	// catalog). "" when codex isn't enabled. Read per request so a plan change
	// (cache refetch) shows up without restarting usher.
	codexModelsPath string
}

func NewServer(
	addr string,
	hookSockPath string,
	authStore *auth.Store,
	r *router.Router,
	main *mainchat.Store,
	agent usheragent.Agent,
	codexModelsPath string,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		addr:            addr,
		hookSockPath:    hookSockPath,
		auth:            authStore,
		router:          r,
		main:            main,
		agent:           agent,
		logger:          logger,
		codexModelsPath: codexModelsPath,
	}
}

// modelOption is one entry for the new-session model picker.
type modelOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// codexModels returns the user-selectable Codex models from codex's own
// per-account catalog (models_cache.json) — so the picker matches whatever the
// account's plan (free/Plus/Pro) actually offers, with no hardcoded list.
// Empty when codex isn't enabled or the cache is unreadable. Ordered as codex's
// own picker (lowest priority number first).
func (s *Server) codexModels() []modelOption {
	if s.codexModelsPath == "" {
		return nil // codex backend not enabled
	}
	out := s.codexCatalog()
	if len(out) == 0 {
		// Codex is enabled but its catalog is missing/unreadable (cache not yet
		// written, deleted, …). Fall back to the current named models so a session
		// can still be created; codex retires old models slowly, so these stay
		// valid well past any new release, and a present cache supersedes them.
		return []modelOption{
			{Value: "gpt-5.5", Label: "GPT-5.5"},
			{Value: "gpt-5.4-mini", Label: "GPT-5.4 Mini"},
		}
	}
	return out
}

// codexCatalog reads codex's per-account model catalog (models_cache.json),
// returning the user-listable models ordered as codex's own picker. Empty when
// the cache is unreadable or has no listable models.
func (s *Server) codexCatalog() []modelOption {
	raw, err := os.ReadFile(s.codexModelsPath)
	if err != nil {
		return nil
	}
	var doc struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
			Priority    int    `json:"priority"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	picks := doc.Models[:0:0]
	for _, m := range doc.Models {
		if m.Visibility == "list" && m.Slug != "" { // "hide" = internal (e.g. auto-review)
			picks = append(picks, m)
		}
	}
	// Lower priority number = higher up in codex's own picker.
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].Priority < picks[j].Priority })
	out := make([]modelOption, 0, len(picks))
	for _, m := range picks {
		label := m.DisplayName
		if label == "" {
			label = m.Slug
		}
		out = append(out, modelOption{Value: m.Slug, Label: label})
	}
	return out
}

// handleModels feeds the new-session model picker: which backends are installed
// (so it drops an unavailable one) and Codex's per-account model catalog (Claude's
// list is static in the page markup).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Backends []string      `json:"backends"`
		Codex    []modelOption `json:"codex"`
	}{
		Backends: s.router.Backends(),
		Codex:    s.codexModels(),
	})
}

// codexModelAllowed reports whether slug is in the account's Codex catalog.
func (s *Server) codexModelAllowed(slug string) bool {
	for _, m := range s.codexModels() {
		if m.Value == slug {
			return true
		}
	}
	return false
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

	webMux.HandleFunc("GET /api/sessions", s.handleListSessions)
	webMux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	webMux.HandleFunc("GET /api/models", s.handleModels)
	webMux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	webMux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	webMux.HandleFunc("GET /api/sessions/{id}/transcript", s.handleTranscript)
	webMux.HandleFunc("POST /api/sessions/{id}/fork", s.handleFork)
	webMux.HandleFunc("POST /api/sessions/{id}/send", s.handleSend)
	webMux.HandleFunc("DELETE /api/sessions/{id}/send", s.handleCancelSend)
	webMux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	webMux.HandleFunc("GET /api/sessions/{id}/screen", s.handleScreen)
	webMux.HandleFunc("POST /api/sessions/{id}/keys", s.handleKeys)
	webMux.HandleFunc("POST /api/sessions/{id}/auto-approve", s.handleAutoApprove)
	webMux.HandleFunc("POST /api/sessions/{id}/archive", s.handleArchive)
	webMux.HandleFunc("DELETE /api/sessions/{id}/archive", s.handleUnarchive)

	webMux.HandleFunc("GET /api/mainchats", s.handleListMainChats)
	webMux.HandleFunc("GET /api/mainchats/{id}", s.handleGetMainChat)
	webMux.HandleFunc("GET /api/mainchats/{id}/messages", s.handleListMainChatMessages)
	webMux.HandleFunc("POST /api/mainchats/{id}/send", s.handleMainChatSend)

	webMux.HandleFunc("GET /api/interactions", s.handleListInteractions)
	webMux.HandleFunc("POST /api/interactions/{id}/respond", s.handleRespondInteraction)

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	webMux.Handle("GET /", http.FileServer(http.FS(sub)))

	hookMux := http.NewServeMux()
	hookMux.HandleFunc("POST /hook/{event}", s.handleHook)

	webSrv := &http.Server{
		Addr:              s.addr,
		Handler:           gzipMiddleware(s.authMiddleware(webMux)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	hookSrv := &http.Server{
		Handler:           hookMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	sockListener, err := listenUnixSocket(s.hookSockPath)
	if err != nil {
		return fmt.Errorf("hook socket: %w", err)
	}

	errCh := make(chan error, 2)
	go func() {
		s.logger.Info("usher web listening", "addr", s.addr)
		errCh <- webSrv.ListenAndServe()
	}()
	go func() {
		s.logger.Info("usher hook listening", "socket", s.hookSockPath)
		errCh <- hookSrv.Serve(sockListener)
	}()

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = webSrv.Shutdown(shutdownCtx)
		_ = hookSrv.Shutdown(shutdownCtx)
		_ = os.Remove(s.hookSockPath)
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

// listenUnixSocket binds a Unix domain socket at path with mode 0600. A
// stale socket file from a previous unclean shutdown is removed first.
func listenUnixSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Remove stale socket from a prior unclean shutdown. We intentionally
	// don't check whether an instance is currently bound — net.Listen will
	// fail loudly below if another process holds the address.
	if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return ln, nil
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
			writeErr(w, http.StatusUnauthorized, "unauthorized")
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
		writeErr(w, http.StatusInternalServerError, err.Error())
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- sessions ------------------------------------------------------------

// sessionDTO wraps core.Session with web-only fields (auto-approve flag
// and archive visibility). We don't put these on core.Session because
// the state is process-local and shouldn't leak into the discovery model.
type sessionDTO struct {
	core.Session
	AutoApprove bool `json:"auto_approve"`
	Archived    bool `json:"archived"`
}

// handleListSessions returns sessions visible by default, or the full
// set when ?include_archived=1 is passed (used by the sidebar's per-cwd
// "show archived" disclosure).
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("include_archived") == "1"
	sessions := s.router.ListSessions()
	out := make([]sessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		archived := s.router.IsArchived(sess.ID)
		if archived && !includeArchived {
			continue
		}
		out = append(out, sessionDTO{
			Session:     sess,
			AutoApprove: s.router.IsAutoApprove(sess.ID),
			Archived:    archived,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createSessionRequest struct {
	Cwd            string `json:"cwd"`
	InitialMessage string `json:"initial_message"`
	Model          string `json:"model"`
}

// allowedModels gates the --model the create form may request, keeping
// arbitrary values out of the spawned claude command. The keys mirror the
// dropdown in app.js; "" and "default" both mean "no --model flag" (claude's
// own default). Resumes are unaffected — they keep their original model.
var allowedModels = map[string]bool{
	"": true, "default": true,
	"opus": true, "sonnet": true, "haiku": true,
	"opusplan": true,
	// sonnet[1m] is meaningful: plain sonnet defaults to 200K, the suffix opts
	// into Sonnet's 1M context. No opus[1m] — Opus is already natively 1M, so
	// the suffix is a no-op there.
	"sonnet[1m]": true,
	"fable":      true,
	// Version-pinned full ID (no short alias; plain "opus" resolves to 4.8).
	"claude-opus-4-6": true,
	// Codex models are NOT listed here — they're validated per request against
	// the account's live catalog (see codexModelAllowed), so the picker supports
	// whatever the plan offers without a hardcoded list.
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// Codex models are validated against the account's live catalog (no hardcoded
	// list — supports whatever the plan offers); Claude models against the static
	// allowlist.
	if router.BackendForModel(req.Model) == "codex" {
		if !s.codexModelAllowed(req.Model) {
			writeErr(w, http.StatusBadRequest, "invalid model: "+req.Model)
			return
		}
	} else if !allowedModels[req.Model] {
		writeErr(w, http.StatusBadRequest, "invalid model: "+req.Model)
		return
	}
	model := req.Model
	if model == "default" {
		model = ""
	}
	id, err := s.router.StartSession(req.Cwd, req.InitialMessage, model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.router.GetSession(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sessionDTO{
		Session:     sess,
		AutoApprove: s.router.IsAutoApprove(id),
		Archived:    s.router.IsArchived(id),
	})
}

// handleDeleteSession permanently deletes a session and its jsonl. Destructive
// and irreversible — distinct from archive (DELETE .../archive), which only
// un-hides. Returns 404 for an unknown id.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.router.DeleteSession(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	s.router.Archive(id)
	writeJSON(w, http.StatusOK, map[string]bool{"archived": true})
}

func (s *Server) handleUnarchive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	s.router.Unarchive(id)
	writeJSON(w, http.StatusOK, map[string]bool{"archived": false})
}

type autoApproveRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleAutoApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	var req autoApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	s.router.SetAutoApprove(id, req.Enabled)
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
}

type forkRequest struct {
	AfterUUID string `json:"after_uuid"` // fork point: the uuid a transcript turn carries
}

// handleFork branches a session at a past turn into a new session (a prefix
// copy of its jsonl — conversation only). Responds with the new session id.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req forkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.AfterUUID == "" {
		writeErr(w, http.StatusBadRequest, "after_uuid is required")
		return
	}
	newID, err := s.router.ForkSession(id, req.AfterUUID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": newID})
}

type sendRequest struct {
	Text string `json:"text"`
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	if err := s.router.SendToSession(id, req.Text); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) handleCancelSend(w http.ResponseWriter, r *http.Request) {
	if err := s.router.CancelSend(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	turns, total, err := s.router.ReadTurns(id, limit)
	if errors.Is(err, router.ErrSessionNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if turns == nil {
		turns = []jsonl.Turn{}
	}
	// Total turn count before the limit trim, so the client knows whether
	// older turns exist beyond the window (to offer "load earlier").
	w.Header().Set("X-Transcript-Total", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, turns)
}

// sseForward is the event vocabulary the web client consumes. Raw jsonl
// lines ("user", "assistant", "system", bookkeeping types) are deliberately
// NOT forwarded: the client renders live turns from the derived "part" /
// "turn.user" events (server-side grouped, see router.publishStream), and
// the raw lines — thinking blocks, usage stats, file-history snapshots —
// would only burn mobile bandwidth.
var sseForward = map[string]bool{
	"subprocess.started": true,
	"subprocess.exit":    true,
	"error":              true,
	"part":               true,
	"turn.user":          true,
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.router.SubscribeSession(id)
	defer cancel()

	// Snapshot-on-connect: the broker has no replay, so a turn whose start (or
	// end) happened before this subscribe is invisible to the client. Emit the
	// current turn state on every connect so a refresh/reconnect can reconcile
	// either way: turn.active stands the bubble back up; turn.idle lets a client
	// that still thinks it's mid-turn (its subprocess.exit was dropped on a
	// dropped connection) finalize. The client handles both idempotently.
	turnEvent := "turn.idle"
	if sess, ok := s.router.GetSession(id); ok &&
		(sess.Status == core.StatusRunning || sess.Status == core.StatusAwaitingPermission) {
		turnEvent = "turn.active"
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: {}\n\n", turnEvent); err == nil {
		flusher.Flush()
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !sseForward[ev.Type] {
				continue
			}
			payload := ev.Raw
			if len(payload) == 0 {
				payload = []byte("{}")
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// --- terminal mirror -----------------------------------------------------

const (
	screenPollInterval = 500 * time.Millisecond
	screenHeartbeat    = 15 * time.Second
)

// softKeys maps the terminal mirror's allow-listed key names to tmux send-keys
// arguments. The mirror deliberately exposes only navigation/control keys, not
// arbitrary typing — the chat send path already covers free text, and an
// allow-list stops a client from injecting unexpected key sequences into the
// pane. send-keys forwards what it's given verbatim, so the gate lives here.
var softKeys = map[string]string{
	"up":     "Up",
	"down":   "Down",
	"left":   "Left",
	"right":  "Right",
	"enter":  "Enter",
	"escape": "Escape",
	"tab":    "Tab",
}

// handleScreen streams a session's live tmux pane as a periodically
// re-captured snapshot — the raw TUI mirror behind the read-only terminal
// view, distinct from /events (the jsonl-tailed message stream). Every
// screenPollInterval we capture-pane and, when the frame changed, push it as a
// `screen` event. When usher holds no live window we emit a single `nopane`
// event (not repeated) so the client can prompt "start the session from chat"
// without the stream spamming identical frames.
func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Size the pane to the viewer (cols + rows measured/derived client-side) so
	// the mirror fills the panel without scroll; this also repairs any
	// manual-attach drift. The client owns the policy (auto vs on, the fraction
	// of the viewport); the server just clamps to wide defensive bounds so a bad
	// client can't ask for an absurd size. Best-effort: an unowned session just
	// errors and the first capture emits `nopane`.
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
	_ = s.router.ResizeCanvas(id, cols, rows)

	ticker := time.NewTicker(screenPollInterval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(screenHeartbeat)
	defer heartbeat.Stop()

	lastFrame := ""
	lastErr := false
	// emit captures one frame and writes it if it changed. Returns false only
	// when the connection is gone (a write failed), to end the stream.
	emit := func() bool {
		screen, err := s.router.CaptureScreen(id)
		if err != nil {
			if lastErr {
				return true // already told the client; don't repeat
			}
			lastErr, lastFrame = true, ""
			if _, werr := fmt.Fprint(w, "event: nopane\ndata: {}\n\n"); werr != nil {
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

type keyRequest struct {
	Key string `json:"key"`
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req keyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	tmuxKey, ok := softKeys[req.Key]
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown key")
		return
	}
	if err := s.router.SendKeys(id, tmuxKey); err != nil {
		// No live window to receive the key (or the session is the user's own,
		// unowned). 409: the client shows the pane is not live.
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- main chat -----------------------------------------------------------

func (s *Server) handleListMainChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.main.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if chats == nil {
		chats = []mainchat.Chat{}
	}
	writeJSON(w, http.StatusOK, chats)
}

func (s *Server) handleListMainChatMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	msgs, err := s.main.Read(id, limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if msgs == nil {
		msgs = []mainchat.Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

type mainChatSendResponse struct {
	Messages []mainchat.Message `json:"messages"`
	Focus    *focusDetail       `json:"focus,omitempty"`
}

type focusDetail struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
}

type mainChatInfo struct {
	ID    string       `json:"id"`
	Focus *focusDetail `json:"focus,omitempty"`
}

const (
	mainChatHistoryCap = 40 // 20 turns of user+agent
	stateBlockMaxRows  = 30 // session rows in the rendered <current_state> preamble
)

// renderStateBlock produces a compact ground-truth dump of the router's
// current view of sessions + the active focus. We append it to the user's
// message every turn so trivia questions ("how many sessions?", "what's
// the focused cwd?", "is X running?") can be answered straight from
// context instead of hallucinated. Patterned after Hermes-Agent's
// per-turn state injection — kept off the system prompt so cache hits
// still happen on the static prefix.
func (s *Server) renderStateBlock(focusID string) string {
	sessions := s.router.ListSessions()
	pending := s.router.ListPendingInteractions()

	var b strings.Builder
	b.WriteString("<current_state>\n")
	fmt.Fprintf(&b, "session_count: %d\n", len(sessions))
	fmt.Fprintf(&b, "pending_permission_requests: %d\n", len(pending))
	if focusID != "" {
		if sess, ok := s.router.GetSession(focusID); ok {
			fmt.Fprintf(&b, "focus: %s (cwd %s, title %q)\n",
				focusID, sess.Cwd, truncateRunes(sess.Title, 60))
		} else {
			fmt.Fprintf(&b, "focus: %s (no longer in discovery)\n", focusID)
		}
	} else {
		b.WriteString("focus: (none yet)\n")
	}
	b.WriteString("sessions:\n")
	rows := sessions
	if len(rows) > stateBlockMaxRows {
		rows = rows[:stateBlockMaxRows]
	}
	for _, sess := range rows {
		mark := ""
		if sess.ID == focusID {
			mark = "  [FOCUS]"
		}
		fmt.Fprintf(&b, "  %s  %-30s  %-7s  %s%s\n",
			sess.ID,
			truncateRunes(sess.Cwd, 30),
			string(sess.Status),
			truncateRunes(sess.Title, 50),
			mark)
	}
	if len(sessions) > stateBlockMaxRows {
		fmt.Fprintf(&b, "  … %d more (truncated)\n", len(sessions)-stateBlockMaxRows)
	}
	b.WriteString("</current_state>")
	return b.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// focusSwitchBanner returns a one-line clickable banner when `touched` (the
// session this turn acted on) differs from prevFocus, or "" if focus didn't
// change. The link is the SPA's session route, rendered in the chat bubble.
func focusSwitchBanner(prevFocus, touched, title string) string {
	if touched == "" || touched == prevFocus {
		return ""
	}
	verb := "Switching to"
	if prevFocus == "" {
		verb = "Routing to"
	}
	label := title
	if label == "" {
		label = shortID(touched)
	}
	return fmt.Sprintf("↪ %s [%s](#/s/%s)\n\n", verb, label, touched)
}

func (s *Server) handleGetMainChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgs, err := s.main.Read(id, 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	info := mainChatInfo{ID: id, Focus: s.lastFocus(msgs)}
	writeJSON(w, http.StatusOK, info)
}

// lastFocus walks msgs newest-first and returns the most recent non-empty
// FocusSession decorated with current session metadata (cwd, title) if
// the session is still discoverable.
func (s *Server) lastFocus(msgs []mainchat.Message) *focusDetail {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].FocusSession == "" {
			continue
		}
		fd := &focusDetail{SessionID: msgs[i].FocusSession}
		if sess, ok := s.router.GetSession(msgs[i].FocusSession); ok {
			fd.Cwd = sess.Cwd
			fd.Title = sess.Title
		}
		return fd
	}
	return nil
}

func (s *Server) handleMainChatSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}

	// Read history (and prior focus) BEFORE persisting the new user message.
	prior, err := s.main.Read(id, mainChatHistoryCap)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	history := make([]usheragent.HistoryMessage, 0, len(prior))
	for _, m := range prior {
		history = append(history, usheragent.HistoryMessage{Role: m.Role, Content: m.Content})
	}
	prevFocus := ""
	if fd := s.lastFocus(prior); fd != nil {
		prevFocus = fd.SessionID
	}

	userMsg := mainchat.Message{Role: "user", Content: req.Text, Time: time.Now().UTC()}
	if err := s.main.Append(id, userMsg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Append a compact ground-truth block to the user message. The agent
	// (especially small models like Haiku / Flash / mini) uses it to answer
	// metadata trivia without hallucinating, and to verify focus before
	// claiming a switch.
	enrichedUserMsg := req.Text + "\n\n" + s.renderStateBlock(prevFocus)

	res, err := s.agent.Handle(r.Context(), history, prevFocus, enrichedUserMsg)
	if err != nil {
		s.logger.Warn("agent handle", "err", err)
		res = usheragent.AgentResult{Reply: "agent error: " + err.Error()}
	}
	// Carry forward focus when this turn didn't touch any session.
	newFocus := res.FocusSession
	if newFocus == "" {
		newFocus = prevFocus
	}
	// Announce a focus change server-side (the model relays verbatim and can't
	// reliably detect a switch itself): prepend a linked banner when this turn
	// routed to a session different from the prior focus.
	title := ""
	if sess, ok := s.router.GetSession(res.FocusSession); ok {
		title = sess.Title
	}
	agentMsg := mainchat.Message{
		Role:         "agent",
		Content:      focusSwitchBanner(prevFocus, res.FocusSession, title) + res.Reply,
		Time:         time.Now().UTC(),
		FocusSession: newFocus,
	}
	if err := s.main.Append(id, agentMsg); err != nil {
		s.logger.Warn("main chat append agent", "err", err)
	}

	resp := mainChatSendResponse{Messages: []mainchat.Message{userMsg, agentMsg}}
	if newFocus != "" {
		fd := &focusDetail{SessionID: newFocus}
		if sess, ok := s.router.GetSession(newFocus); ok {
			fd.Cwd = sess.Cwd
			fd.Title = sess.Title
		}
		resp.Focus = fd
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- hooks ---------------------------------------------------------------

type hookPayload struct {
	SessionID      string          `json:"session_id"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	Cwd            string          `json:"cwd"`
	TranscriptPath string          `json:"transcript_path"`
}

// codexPermissionDecision builds Codex's PermissionRequest hook reply:
// {"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":
// {"behavior":"allow"|"deny","message":"…"}}}. message is included only on a
// deny with a reason. An empty/allow reply lets the tool proceed; deny blocks it
// (Codex's built-in approval flow is skipped because the hook decided).
func codexPermissionDecision(behavior, reason string) map[string]any {
	dec := map[string]any{"behavior": behavior}
	if behavior == "deny" && reason != "" {
		dec["message"] = reason
	}
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision":      dec,
		},
	}
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	eventName := r.PathValue("event")

	body, _ := io.ReadAll(r.Body)
	var ev hookPayload
	if len(body) > 0 {
		if err := json.Unmarshal(body, &ev); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid hook payload: "+err.Error())
			return
		}
	}
	if ev.HookEventName == "" {
		ev.HookEventName = eventName
	}

	// PreToolUse is Claude Code's tool-approval hook; PermissionRequest is
	// Codex's. The request payload (snake_case session_id/tool_name/tool_input/
	// cwd) is identical for both — only the decision the hook must emit differs.
	if eventName != "PreToolUse" && eventName != "PermissionRequest" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	resp, err := s.router.HandleHook(r.Context(), hook.Event{
		SessionID: ev.SessionID,
		Event:     eventName,
		ToolName:  ev.ToolName,
		ToolInput: ev.ToolInput,
		Cwd:       ev.Cwd,
	})
	if err != nil {
		s.logger.Warn("hook submit cancelled", "session", ev.SessionID, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	decision := resp.Behavior
	if decision == "" {
		decision = "allow"
	}

	if eventName == "PermissionRequest" {
		// Codex's shape (see codexPermissionDecision). No updatedInput channel
		// (reserved → fails closed), so the AskUserQuestion answer-merge below is
		// Claude-only.
		writeJSON(w, http.StatusOK, codexPermissionDecision(decision, resp.Reason))
		return
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
	writeJSON(w, http.StatusOK, map[string]any{"hookSpecificOutput": hookOut})
}

func (s *Server) handleListInteractions(w http.ResponseWriter, r *http.Request) {
	list := s.router.ListPendingInteractions()
	if list == nil {
		list = []hook.Pending{}
	}
	writeJSON(w, http.StatusOK, list)
}

type respondReq struct {
	Behavior string `json:"behavior"`
	Reason   string `json:"reason"`
	Scope    string `json:"scope"` // "" or "once" or "session"
	// Answers carries an AskUserQuestion choice (question → chosen label).
	// Sent with Behavior "allow"; the server forwards it into the hook
	// response's updatedInput.
	Answers map[string]string `json:"answers,omitempty"`
}

func (s *Server) handleRespondInteraction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req respondReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Behavior != "allow" && req.Behavior != "deny" {
		writeErr(w, http.StatusBadRequest, "behavior must be allow|deny")
		return
	}
	if req.Scope != "" && req.Scope != "once" && req.Scope != "session" {
		writeErr(w, http.StatusBadRequest, "scope must be once|session")
		return
	}
	if err := s.router.RespondInteraction(id, hook.Response{
		Behavior: req.Behavior,
		Reason:   req.Reason,
		Scope:    req.Scope,
		Answers:  req.Answers,
	}); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
