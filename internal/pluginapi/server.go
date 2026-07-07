// Package pluginapi exposes the IM-frontend subset of the Router to
// out-of-process plugins over a Unix socket, and provides the matching client.
//
// Heavy-SDK integrations (e.g. Lark, whose event long-connection needs
// websocket + protobuf libraries) live in their own Go module and run as a
// sidecar process; this socket is their only seam into usher, so their
// dependencies never enter usher's go.mod. The socket sits in the data dir
// with mode 0600 — the same fs-permission trust boundary as the hook socket.
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

// RouterAPI is the strict subset of router.Router served to plugins — the
// same five methods the in-process Telegram hub consumes, plus the pending
// list so a (re)connecting plugin can catch up on prompts it missed.
type RouterAPI interface {
	GetSession(id string) (core.Session, bool)
	SubscribeAllSessions() (<-chan broker.Event, func())
	SendToSession(id, text string) error
	ListPendingInteractions() []hook.Pending
	SubscribePendingInteractions() (<-chan hook.Pending, func())
	RespondInteraction(id string, resp hook.Response) error
}

// Server serves the plugin API on a Unix socket.
type Server struct {
	router RouterAPI
	logger *slog.Logger
}

func NewServer(router RouterAPI, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{router: router, logger: logger}
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
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("POST /v1/sessions/{id}/send", s.handleSend)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/interactions", s.handleInteractions)
	mux.HandleFunc("POST /v1/interactions/{id}/respond", s.handleRespond)
	return mux
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
}

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
	if err := s.router.SendToSession(r.PathValue("id"), req.Text); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRespond(w http.ResponseWriter, r *http.Request) {
	var resp hook.Response
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
