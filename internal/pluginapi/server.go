// Package pluginapi exposes controlled Router surfaces to out-of-process local
// clients over a Unix socket, and provides the matching client.
//
// Heavy-SDK integrations (e.g. Lark, whose event long-connection needs
// websocket + protobuf libraries) live in their own Go module and run as a
// sidecar process. The opt-in router MCP stdio adapter also uses this control
// plane. The socket sits in the data dir with mode 0600 — the same
// fs-permission trust boundary as the hook socket.
package pluginapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexustar/usher/internal/agentprofile"
	"github.com/nexustar/usher/internal/attachment"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
)

// RouterAPI is the subset of router.Router served on the local control socket.
// It contains the event-driven IM surface plus the query/control operations
// needed by the opt-in router MCP.
type RouterAPI interface {
	GetSession(id string) (core.Session, bool)
	ListSessions() []core.Session
	StartSession(o core.CreateOptions) (string, error)
	SubscribeAllSessions() (<-chan broker.Event, func())
	SendToSession(id, text string) error
	// The Route variants resolve the inbox — usher's own router session —
	// themselves and queue the finished turn there as a note; the caller only
	// asks. The returned bool reports whether a relay was attached: with no
	// router session designated they degrade to the plain form.
	RouteToSession(id, text string) (relayed bool, err error)
	RouteNewSession(o core.CreateOptions) (id string, relayed bool, err error)
	ReadSessionTranscriptPage(id string, offset, limit int) (core.TranscriptPage, error)
	SearchSessionTranscript(id, query string, maxHits, contextChars int) ([]core.TranscriptSearchHit, bool, error)
	SearchAllSessions(query string, maxSessions, contextChars int) ([]core.SessionSearchResult, bool, error)
	SetArchived(id string, archived bool)
	IsArchived(id string) bool
	SetAutoApprove(id string, enabled bool)
	IsAutoApprove(id string) bool
	ListPendingInteractions() []interaction.Pending
	SubscribePendingInteractions() (<-chan interaction.Pending, func())
	RespondInteraction(id string, resp interaction.Response) error
}

// Server serves the plugin API on a Unix socket.
type Server struct {
	router         RouterAPI
	attachmentsDir string
	agents         *agentprofile.Store
	logger         *slog.Logger
}

func NewServer(router RouterAPI, attachmentsDir string, agents *agentprofile.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{router: router, attachmentsDir: attachmentsDir, agents: agents, logger: logger}
}

// Run listens on the Unix socket at path and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context, path string) error {
	ln, err := ListenUnixSocket(path)
	if err != nil {
		return fmt.Errorf("plugin socket: %w", err)
	}
	srv := &http.Server{
		Handler:           s.mux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("usher plugin api listening", "socket", path)
		errCh <- srv.Serve(ln)
	}()
	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = os.Remove(path)
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

func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("POST /v1/sessions/{id}/send", s.handleSend)
	mux.HandleFunc("GET /v1/sessions/{id}/transcript", s.handleTranscript)
	mux.HandleFunc("POST /v1/sessions/{id}/archive", s.handleArchive)
	mux.HandleFunc("POST /v1/sessions/{id}/auto-approve", s.handleAutoApprove)
	mux.HandleFunc("POST /v1/sessions/{id}/attachments", s.handleAttachment)
	mux.HandleFunc("GET /v1/search", s.handleSearch)
	mux.HandleFunc("GET /v1/interactions/pending", s.handlePendingInteractions)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/interactions", s.handleInteractions)
	mux.HandleFunc("POST /v1/interactions/{id}/respond", s.handleRespond)
	return mux
}

func (s *Server) handlePendingInteractions(w http.ResponseWriter, _ *http.Request) {
	pending := s.router.ListPendingInteractions()
	if pending == nil {
		pending = []interaction.Pending{}
	}
	writeJSON(w, http.StatusOK, pending)
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	sessions := s.router.ListSessions()
	out := make([]SessionView, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, SessionView{
			Session:     sess,
			Archived:    s.router.IsArchived(sess.ID),
			AutoApprove: s.router.IsAutoApprove(sess.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateRequest spawns a session. When Agent names a configured agent,
// non-empty Cwd/Backend/Model override that agent's defaults; without it they
// are the request in full. RelayTo, when set, is the inbox session that
// receives the new session's first reply once it lands.
type CreateRequest struct {
	Agent          string `json:"agent,omitempty"`
	Backend        string `json:"backend,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	InitialMessage string `json:"initial_message"`
	Model          string `json:"model"`
	// Relay asks for the new session's first reply to be delivered into
	// usher's router session once it lands.
	Relay bool `json:"relay,omitempty"`
}

// handleCreateSession spawns a session and returns its id as soon as the
// process is up. With relay, the session's first reply is delivered into
// usher's router session when it lands; the call never waits for it.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	opts, err := s.agents.Resolve(req.Agent, req.Cwd, req.Backend, req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts.InitialMessage = req.InitialMessage
	var id string
	var relayed bool
	if req.Relay {
		id, relayed, err = s.router.RouteNewSession(opts)
	} else {
		id, err = s.router.StartSession(opts)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, Delivery{SessionID: id, Relayed: relayed})
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 20, 200)
	offset := -1
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			offset = n
		}
	}
	page, err := s.router.ReadSessionTranscriptPage(r.PathValue("id"), offset, limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, TranscriptPage{
		TranscriptPage: page,
		HasMore:        page.Offset > 0 || page.Offset+len(page.Turns) < page.Total,
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	contextChars := queryInt(r, "context_chars", 120, 500)
	if id := r.URL.Query().Get("session_id"); id != "" {
		hits, truncated, err := s.router.SearchSessionTranscript(id, query, queryInt(r, "limit", 20, 100), contextChars)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, SearchResult{
			Scope: "session", SessionID: id, Hits: hits, Truncated: truncated,
		})
		return
	}
	results, truncated, err := s.router.SearchAllSessions(query, queryInt(r, "limit", 20, 50), contextChars)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, SearchResult{Scope: "all", Results: results, Truncated: truncated})
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Archived bool `json:"archived"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	s.router.SetArchived(r.PathValue("id"), req.Archived)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAutoApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	s.router.SetAutoApprove(r.PathValue("id"), req.Enabled)
	w.WriteHeader(http.StatusNoContent)
}

func queryInt(r *http.Request, name string, fallback, ceiling int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n <= 0 {
		return fallback
	}
	if n > ceiling {
		return ceiling
	}
	return n
}

const maxAttachmentBytes = 100 << 20

func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.router.GetSession(id); !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes)
	path, err := attachment.Save(s.attachmentsDir, id, r.URL.Query().Get("filename"), r.Body, maxAttachmentBytes)
	if errors.Is(err, attachment.ErrInvalidName) {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	var maxErr *http.MaxBytesError
	if errors.Is(err, attachment.ErrTooLarge) || errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "attachment too large")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save attachment: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.router.GetSession(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

type sendReq struct {
	Text string `json:"text"`
	// Relay asks for this turn's reply to be delivered into usher's router
	// session once it completes. Without it the send is fire-and-forget: the
	// reply lands only in the target session's own transcript.
	Relay bool `json:"relay,omitempty"`
}

// handleSend routes a prompt to a session and returns as soon as it is
// accepted — never waiting for the turn. Turns run for minutes or hours; a
// held request could not outlast them, and the caller's own read timeout
// would fire long before usher's. Requesting a relay when usher has no router
// session is not an error — the prompt is still delivered, and the ack
// reports that nothing will come back.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	id := r.PathValue("id")
	var relayed bool
	var err error
	if req.Relay {
		relayed, err = s.router.RouteToSession(id, req.Text)
	} else {
		err = s.router.SendToSession(id, req.Text)
	}
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if !req.Relay {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusAccepted, Delivery{SessionID: id, Relayed: relayed})
}

func (s *Server) handleRespond(w http.ResponseWriter, r *http.Request) {
	var resp interaction.Response
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	if err := s.router.RespondInteraction(r.PathValue("id"), resp); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents streams every session's broker events as SSE. An optional
// ?types=a,b,c query keeps only those event types — a tool_result "user"
// event drags a whole tool output through the socket, so a plugin that only
// renders a few types should filter server-side.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, cancel := s.router.SubscribeAllSessions()
	defer cancel()
	flush, ok := sseStart(w)
	if !ok {
		return
	}
	keep := map[string]bool{}
	if q := r.URL.Query().Get("types"); q != "" {
		for _, t := range strings.Split(q, ",") {
			keep[t] = true
		}
	}
	forward(r.Context(), w, flush, events, func(ev broker.Event) bool {
		return len(keep) == 0 || keep[ev.Type]
	})
}

// handleInteractions streams pending permission interactions as SSE. The
// currently-pending set is replayed first so a (re)connecting plugin sees
// prompts raised while it was away; consumers dedupe by pending id (a prompt
// can appear both in the snapshot and on the live channel).
func (s *Server) handleInteractions(w http.ResponseWriter, r *http.Request) {
	pending, cancel := s.router.SubscribePendingInteractions()
	defer cancel()
	snapshot := s.router.ListPendingInteractions()
	flush, ok := sseStart(w)
	if !ok {
		return
	}
	for _, p := range snapshot {
		if !writeSSE(w, flush, p) {
			return
		}
	}
	forward(r.Context(), w, flush, pending, nil)
}

// sseStart writes the SSE preamble (same header set as the web package's
// sseStart) and returns the flush func.
func sseStart(w http.ResponseWriter) (func(), bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher.Flush, true
}

// forward pumps channel values to the SSE stream until the client disconnects
// or the channel closes, skipping values keep rejects (nil = keep all). A
// periodic comment line keeps the connection verifiably alive for the
// client's reconnect logic.
func forward[T any](ctx context.Context, w http.ResponseWriter, flush func(), ch <-chan T, keep func(T) bool) {
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flush()
		case v, ok := <-ch:
			if !ok {
				return
			}
			if keep != nil && !keep(v) {
				continue
			}
			if !writeSSE(w, flush, v) {
				return
			}
		}
	}
}

// writeSSE emits one JSON value as an SSE data frame.
func writeSSE(w http.ResponseWriter, flush func(), v any) bool {
	data, err := json.Marshal(v)
	if err != nil {
		return true // skip the value, keep the stream
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	flush()
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// DefaultDataDir resolves usher's data directory ($XDG_DATA_HOME/usher,
// falling back to ~/.local/share/usher) — the same resolution `usher serve`
// uses for its --data-dir default. Exported so out-of-process plugins derive
// the identical socket rendezvous instead of duplicating the logic.
func DefaultDataDir() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "usher")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "usher")
}

// SocketPath returns the plugin API socket path inside dataDir. The one
// definition both `usher serve` and plugins use.
func SocketPath(dataDir string) string {
	return filepath.Join(dataDir, "plugin.sock")
}

// listenMu serializes the umask window in ListenUnixSocket: umask is
// process-wide, so two concurrent listeners (hook + plugin socket) must not
// interleave their set/restore.
var listenMu sync.Mutex

// ListenUnixSocket binds a Unix domain socket at path with mode 0600. A
// stale socket file from a previous unclean shutdown is removed first.
// Shared with the web package's hook listener.
//
// The socket is born 0600, not chmod'd down after bind: a chmod-after-listen
// leaves a window where another local user could connect under the umask
// default and keep the connection across the chmod. The umask flip is
// process-wide for the bind's duration; any file created concurrently only
// gets stricter permissions, never looser.
func ListenUnixSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	listenMu.Lock()
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	listenMu.Unlock()
	if err != nil {
		return nil, err
	}
	return ln, nil
}
