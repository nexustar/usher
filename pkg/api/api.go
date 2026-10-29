// Package api holds the /api/v1 handlers the web server and the control
// socket both serve. Each listener adds its own authentication and its own
// routes under /internal.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nexustar/usher/pkg/agentprofile"
	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/broker"
	"github.com/nexustar/usher/pkg/core"
	"github.com/nexustar/usher/pkg/interaction"
	"github.com/nexustar/usher/pkg/schedule"
)

// Prefix is where every listener mounts these handlers.
const Prefix = "/api/v1"

// Router is the subset of usher's router these handlers drive.
type Router interface {
	ListSessions() []core.Session
	ListSessionsWithSubagents() []core.Session
	GetSession(id string) (core.Session, bool)
	OpenSession(id string, wait time.Duration) (core.Session, bool)
	DefaultEffort(ctx context.Context, backendName, model string) string
	RouterSession() string
	StartSession(o core.CreateOptions) (string, error)
	DeleteSession(id string) error
	ForkSession(srcID, afterUUID string) (string, error)
	SendToSession(id, text string) error
	CancelSend(id string) error
	PauseSession(id string) error
	ResumeSession(ctx context.Context, id string) error
	ReadTurns(id, before string, limit int) ([]core.Turn, bool, error)
	ReadTurnsFrom(id, from string) ([]core.Turn, error)
	SubscribeSession(id string) (<-chan broker.Event, func())
	SubscribeAllSessions() (<-chan broker.Event, func())
	Rename(ctx context.Context, id, title string) error
	SetRuntime(ctx context.Context, id, model, effort string) error
	LaunchOptions(id string) (prompt string, args []string)
	SetExtraArgs(id string, args []string) error
	Archive(id string)
	Unarchive(id string)
	IsArchived(id string) bool
	Pin(id string)
	Unpin(id string)
	IsPinned(id string) bool
	Approval(id string) string
	SetApproval(ctx context.Context, id, approval string) error
	Approvals(backendName string) []string
	HasTerminal(id string) bool
	TerminalAvailable() bool
	ComposerItems(ctx context.Context, id, cwd string) (backend.ComposerCatalog, error)
	Backends() []string
	Models(ctx context.Context, backendName string) ([]backend.Model, error)
	Usage(ctx context.Context, backendName string, refresh bool) (backend.Usage, bool, error)
	SearchSessionTranscript(id, query string, maxHits, contextChars int) ([]core.TranscriptSearchHit, bool, error)
	SearchAllSessions(query string, maxSessions, contextChars int) ([]core.SessionSearchResult, bool, error)
	ListPendingInteractions() []interaction.Pending
	SubscribePendingInteractions() (<-chan interaction.Pending, func())
	RespondInteraction(id string, resp interaction.Response) error
}

type Config struct {
	Router Router
	Agents *agentprofile.Store
	// Schedules nil leaves out the /schedules routes.
	Schedules      *schedule.Runner
	AttachmentsDir string
	Version        string
	ReadOnly       bool
}

type Server struct {
	Config
}

func New(c Config) *Server { return &Server{Config: c} }

// Register mounts the handlers on mux under Prefix.
func (s *Server) Register(mux *http.ServeMux) {
	handle := func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+Prefix+path, h)
	}
	handle("GET /info", s.handleInfo)
	handle("GET /backends", s.handleBackends)
	handle("GET /usage", s.handleUsage)

	handle("GET /sessions", s.handleListSessions)
	handle("POST /sessions", s.handleCreateSession)
	handle("GET /sessions/{id}", s.handleGetSession)
	handle("PATCH /sessions/{id}", s.handlePatchSession)
	handle("DELETE /sessions/{id}", s.handleDeleteSession)
	handle("POST /sessions/{id}/fork", s.handleFork)
	handle("POST /sessions/{id}/messages", s.handleMessage)
	handle("POST /sessions/{id}/interrupt", s.handleInterrupt)
	handle("POST /sessions/{id}/pause", s.handlePause)
	handle("POST /sessions/{id}/resume", s.handleResume)
	handle("GET /sessions/{id}/transcript", s.handleTranscript)
	handle("GET /sessions/{id}/events", s.handleSessionEvents)
	handle("GET /sessions/{id}/commands", s.handleCommands)
	handle("GET /sessions/{id}/image", s.handleImage)
	handle("POST /uploads", s.handleUpload)
	handle("GET /search", s.handleSearch)

	handle("GET /events", s.handleEvents)
	handle("GET /interactions", s.handleListInteractions)
	handle("POST /interactions/{id}/respond", s.handleRespondInteraction)

	handle("GET /agents", s.handleAgents)
	handle("POST /agents", s.handleCreateAgent)
	handle("PUT /agents/{name}", s.handleUpdateAgent)
	handle("DELETE /agents/{name}", s.handleDeleteAgent)

	if s.Schedules != nil {
		handle("GET /schedules", s.handleSchedules)
		handle("POST /schedules", s.handleCreateSchedule)
		handle("PUT /schedules/{id}", s.handleUpdateSchedule)
		handle("DELETE /schedules/{id}", s.handleDeleteSchedule)
		handle("POST /schedules/{id}/run", s.handleRunSchedule)
	}
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, struct {
		Version  string `json:"version"`
		ReadOnly bool   `json:"read_only"`
	}{s.Version, s.ReadOnly})
}

// --- responses -----------------------------------------------------------

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error is the body of every failed request.
type Error struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail's Code is derived from the status.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var errorCodes = map[int]string{
	http.StatusBadRequest:            "invalid_request",
	http.StatusUnauthorized:          "unauthorized",
	http.StatusForbidden:             "forbidden",
	http.StatusNotFound:              "not_found",
	http.StatusConflict:              "conflict",
	http.StatusRequestEntityTooLarge: "too_large",
	http.StatusTooManyRequests:       "rate_limited",
	http.StatusBadGateway:            "unavailable",
	http.StatusServiceUnavailable:    "unavailable",
}

// ErrorCode is the code WriteError reports for status.
func ErrorCode(status int) string {
	if code, ok := errorCodes[status]; ok {
		return code
	}
	if status >= 500 {
		return "internal"
	}
	return "invalid_request"
}

func WriteError(w http.ResponseWriter, status int, msg string) {
	if status >= 500 {
		slog.Warn("request failed", "status", status, "err", msg)
	}
	WriteJSON(w, status, Error{ErrorDetail{Code: ErrorCode(status), Message: msg}})
}

// ErrStatus maps a router error onto an HTTP status.
func ErrStatus(err error) int {
	switch {
	case errors.Is(err, core.ErrSessionNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrSendQueueFull), errors.Is(err, core.ErrRouterSession):
		return http.StatusConflict
	case errors.Is(err, core.ErrReadOnlySession):
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}

// Decode reads a JSON body into v, answering 400 itself on failure.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return false
	}
	return true
}

// known answers 404 for a session that does not exist.
func (s *Server) known(w http.ResponseWriter, id string) bool {
	if _, ok := s.Router.GetSession(id); !ok {
		WriteError(w, http.StatusNotFound, "session not found")
		return false
	}
	return true
}
