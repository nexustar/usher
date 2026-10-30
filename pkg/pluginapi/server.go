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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/nexustar/usher/pkg/api"
	"github.com/nexustar/usher/pkg/core"
)

// RouterAPI is what the control socket serves: /api/v1 plus the router
// session's dispatch operations.
type RouterAPI interface {
	api.Router
	// The Route variants queue the finished turn as a note for usher's router
	// session. The bool reports whether there is one; without it they are the
	// plain form.
	RouteToSession(id, text string) (relayed bool, err error)
	RouteNewSession(o core.CreateOptions) (id string, relayed bool, err error)
}

// Server serves the plugin API on a Unix socket.
type Server struct {
	router RouterAPI
	api    *api.Server
	logger *slog.Logger
}

func NewServer(router RouterAPI, c api.Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	c.Router = router
	return &Server{router: router, api: api.New(c), logger: logger}
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	s.api.Register(mux)
	mux.HandleFunc("POST /internal/router/sessions", s.handleRouteNew)
	mux.HandleFunc("POST /internal/router/sessions/{id}/messages", s.handleRoute)
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
	var req api.MessageRequest
	if !api.Decode(w, r, &req) {
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
	if !api.Decode(w, r, &req) {
		return
	}
	opts, err := req.Options(s.api.Agents)
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

// HookSocketPath returns the hook socket path inside dataDir: where spawned
// sessions report tool calls, and the directory `usher mcp-stdio` derives the
// plugin socket from.
func HookSocketPath(dataDir string) string {
	return filepath.Join(dataDir, "hook.sock")
}

// listenMu serializes the umask window in ListenUnixSocket: umask is
// process-wide, so two concurrent listeners (hook + plugin socket) must not
// interleave their set/restore.
var listenMu sync.Mutex

// ErrSocketInUse is a socket another process still listens on.
var ErrSocketInUse = errors.New("socket in use")

// ListenUnixSocket binds a Unix domain socket at path with mode 0600,
// replacing a leftover socket file but not a live one (ErrSocketInUse).
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
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: %w", path, ErrSocketInUse)
		}
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
