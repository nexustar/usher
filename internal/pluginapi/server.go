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
	"github.com/nexustar/usher/internal/api"
	"github.com/nexustar/usher/internal/core"
)

// RouterAPI is what the control socket serves: the session handlers shared
// with the web server, plus the dispatch and search operations only the
// router session's tools use.
type RouterAPI interface {
	api.Router
	// The Route variants queue the finished turn as a note for usher's router
	// session. The bool reports whether there is one; without it they are the
	// plain form.
	RouteToSession(id, text string) (relayed bool, err error)
	RouteNewSession(o core.CreateOptions) (id string, relayed bool, err error)
	SearchSessionTranscript(id, query string, maxHits, contextChars int) ([]core.TranscriptSearchHit, bool, error)
	SearchAllSessions(query string, maxSessions, contextChars int) ([]core.SessionSearchResult, bool, error)
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
	api.New(s.router, s.agents, s.attachmentsDir).Register(mux, "/v1")
	mux.HandleFunc("POST /v1/sessions/{id}/route", s.handleRoute)
	mux.HandleFunc("POST /v1/route", s.handleRouteNew)
	mux.HandleFunc("GET /v1/search", s.handleSearch)
	return mux
}

// Delivery is the ack for routed work: accepted, not waited on. Relayed
// reports whether its reply will come back to the router session by itself.
type Delivery struct {
	SessionID string `json:"id"`
	Relayed   bool   `json:"relayed,omitempty"`
}

// handleRoute sends a prompt to a session on the router session's behalf.
// With no router session the prompt is still delivered, and the ack says
// nothing will come back.
func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	var req api.SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		api.WriteError(w, http.StatusBadRequest, "text is required")
		return
	}
	id := r.PathValue("id")
	relayed, err := s.router.RouteToSession(id, req.Text)
	if err != nil {
		api.WriteError(w, api.ErrStatus(err), err.Error())
		return
	}
	api.WriteJSON(w, http.StatusAccepted, Delivery{SessionID: id, Relayed: relayed})
}

// handleRouteNew is handleRoute for a session that does not exist yet.
func (s *Server) handleRouteNew(w http.ResponseWriter, r *http.Request) {
	var req api.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	opts, err := req.Options(s.agents)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, relayed, err := s.router.RouteNewSession(opts)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusAccepted, Delivery{SessionID: id, Relayed: relayed})
}

// SearchResult carries both search shapes: Hits when a session id scoped the
// search to one transcript, Results when it ranged over every session.
type SearchResult struct {
	Scope     string                     `json:"scope"`
	SessionID string                     `json:"session_id,omitempty"`
	Hits      []core.TranscriptSearchHit `json:"hits,omitempty"`
	Results   []core.SessionSearchResult `json:"results,omitempty"`
	Truncated bool                       `json:"truncated"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		api.WriteError(w, http.StatusBadRequest, "query is required")
		return
	}
	contextChars := queryInt(r, "context_chars", 120, 500)
	if id := r.URL.Query().Get("session_id"); id != "" {
		hits, truncated, err := s.router.SearchSessionTranscript(id, query, queryInt(r, "limit", 20, 100), contextChars)
		if err != nil {
			api.WriteError(w, api.ErrStatus(err), err.Error())
			return
		}
		api.WriteJSON(w, http.StatusOK, SearchResult{
			Scope: "session", SessionID: id, Hits: hits, Truncated: truncated,
		})
		return
	}
	results, truncated, err := s.router.SearchAllSessions(query, queryInt(r, "limit", 20, 50), contextChars)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, SearchResult{Scope: "all", Results: results, Truncated: truncated})
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
