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
	ln, err := listenUnixSocket(path)
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

// handleEvents streams every session's broker events as SSE.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, cancel := s.router.SubscribeAllSessions()
	defer cancel()
	s.serveSSE(w, r, nil, func(w http.ResponseWriter, flush func()) bool {
		return forward(r.Context(), w, flush, events)
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
	s.serveSSE(w, r, func(w http.ResponseWriter, flush func()) bool {
		for _, p := range snapshot {
			if !writeSSE(w, flush, p) {
				return false
			}
		}
		return true
	}, func(w http.ResponseWriter, flush func()) bool {
		return forward(r.Context(), w, flush, pending)
	})
}

// serveSSE writes the SSE preamble, runs the optional prologue (snapshot
// replay), then the stream func until it reports the client is gone.
func (s *Server) serveSSE(
	w http.ResponseWriter,
	r *http.Request,
	prologue func(http.ResponseWriter, func()) bool,
	stream func(http.ResponseWriter, func()) bool,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	if prologue != nil && !prologue(w, flusher.Flush) {
		return
	}
	stream(w, flusher.Flush)
}

// forward pumps channel values to the SSE stream until the client disconnects
// or the channel closes. A periodic comment line keeps the connection
// verifiably alive for the client's reconnect logic.
func forward[T any](ctx context.Context, w http.ResponseWriter, flush func(), ch <-chan T) bool {
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return false
			}
			flush()
		case v, ok := <-ch:
			if !ok {
				return false
			}
			if !writeSSE(w, flush, v) {
				return false
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

// listenUnixSocket binds a Unix domain socket at path with mode 0600. A
// stale socket file from a previous unclean shutdown is removed first.
// (Same idiom as the web package's hook listener.)
func listenUnixSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
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
