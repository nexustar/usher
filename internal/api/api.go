// Package api holds the session handlers the web server and the control
// socket both serve: one implementation, so a request means the same thing on
// either listener. Each listener mounts them under its own prefix and adds its
// own authentication and private routes.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/agentprofile"
	"github.com/nexustar/usher/internal/attachment"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/window"
)

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
	ReadTurns(id, before string, limit int) ([]core.Turn, bool, error)
	ReadTurnsFrom(id, from string) ([]core.Turn, error)
	SubscribeSession(id string) (<-chan broker.Event, func())
	SubscribeAllSessions() (<-chan broker.Event, func())
	Rename(ctx context.Context, id, title string) error
	Archive(id string)
	Unarchive(id string)
	IsArchived(id string) bool
	Pin(id string)
	Unpin(id string)
	IsPinned(id string) bool
	SetAutoApprove(id string, enabled bool)
	IsAutoApprove(id string) bool
	HasTerminal(id string) bool
	TerminalAvailable() bool
	ListPendingInteractions() []interaction.Pending
	SubscribePendingInteractions() (<-chan interaction.Pending, func())
	RespondInteraction(id string, resp interaction.Response) error
}

type Server struct {
	router         Router
	agents         *agentprofile.Store
	attachmentsDir string
}

func New(r Router, agents *agentprofile.Store, attachmentsDir string) *Server {
	return &Server{router: r, agents: agents, attachmentsDir: attachmentsDir}
}

// Register mounts the handlers on mux under prefix ("/api", "/v1").
func (s *Server) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/sessions", s.handleListSessions)
	mux.HandleFunc("POST "+prefix+"/sessions", s.handleCreateSession)
	mux.HandleFunc("GET "+prefix+"/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE "+prefix+"/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET "+prefix+"/sessions/{id}/transcript", s.handleTranscript)
	mux.HandleFunc("POST "+prefix+"/sessions/{id}/fork", s.handleFork)
	mux.HandleFunc("POST "+prefix+"/sessions/{id}/send", s.handleSend)
	mux.HandleFunc("DELETE "+prefix+"/sessions/{id}/send", s.handleCancelSend)
	mux.HandleFunc("GET "+prefix+"/sessions/{id}/events", s.handleSessionEvents)
	mux.HandleFunc("POST "+prefix+"/sessions/{id}/upload", s.handleUpload)
	mux.HandleFunc("POST "+prefix+"/sessions/{id}/auto-approve", s.handleAutoApprove)
	mux.HandleFunc("POST "+prefix+"/sessions/{id}/rename", s.handleRename)
	mux.HandleFunc("POST "+prefix+"/sessions/{id}/archive", s.handleArchive)
	mux.HandleFunc("DELETE "+prefix+"/sessions/{id}/archive", s.handleUnarchive)
	mux.HandleFunc("POST "+prefix+"/sessions/{id}/pin", s.handlePin)
	mux.HandleFunc("DELETE "+prefix+"/sessions/{id}/pin", s.handleUnpin)
	mux.HandleFunc("GET "+prefix+"/events", s.handleEvents)
	mux.HandleFunc("GET "+prefix+"/interactions", s.handleListInteractions)
	mux.HandleFunc("POST "+prefix+"/interactions/{id}/respond", s.handleRespondInteraction)
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, msg string) {
	if status >= 500 {
		slog.Warn("request failed", "status", status, "err", msg)
	}
	WriteJSON(w, status, map[string]string{"error": msg})
}

// errStatus maps a router error onto the status a caller can branch on: the
// session is not there, it cannot take the request now, or it never can.
func errStatus(err error) int {
	switch {
	case errors.Is(err, core.ErrSessionNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrSendQueueFull):
		return http.StatusConflict
	case errors.Is(err, core.ErrReadOnlySession):
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}

// --- sessions ------------------------------------------------------------

// Session is a core.Session plus the flags usher keeps outside the log.
type Session struct {
	core.Session
	AutoApprove bool `json:"auto_approve"`
	Archived    bool `json:"archived"`
	Pinned      bool `json:"pinned"`
	// Router marks usher's router session — the one holding the routing tools.
	Router            bool `json:"router,omitempty"`
	TerminalOpen      bool `json:"terminal_open,omitempty"`
	TerminalAvailable bool `json:"terminal_available,omitempty"`
}

// handleListSessions returns root sessions by default; include_archived=1 and
// include_subagents=1 add the archived roots and the read-only subagents.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("include_archived") == "1"
	includeSubagents := r.URL.Query().Get("include_subagents") == "1"
	sessions := s.router.ListSessions()
	if includeSubagents {
		sessions = s.router.ListSessionsWithSubagents()
	}
	routerID := s.router.RouterSession()
	out := make([]Session, 0, len(sessions))
	for _, sess := range sessions {
		if sess.IsSubagent {
			out = append(out, Session{Session: sess})
			continue
		}
		archived := s.router.IsArchived(sess.ID)
		if archived && !includeArchived {
			continue
		}
		out = append(out, Session{
			Session:     sess,
			AutoApprove: s.router.IsAutoApprove(sess.ID),
			Archived:    archived,
			Pinned:      s.router.IsPinned(sess.ID),
			Router:      sess.ID == routerID,
		})
	}
	WriteJSON(w, http.StatusOK, out)
}

// CreateRequest spawns a session. When Agent names a configured agent,
// non-empty Cwd/Backend/Model override that agent's defaults; without it they
// are the request in full. AutoApprove is a pointer: omitted keeps the agent's
// default, an explicit false turns it off for this session only.
type CreateRequest struct {
	Agent          string `json:"agent,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	InitialMessage string `json:"initial_message"`
	Backend        string `json:"backend,omitempty"`
	Model          string `json:"model,omitempty"`
	AutoApprove    *bool  `json:"auto_approve,omitempty"`
}

// Options resolves the request against the configured agents.
func (req CreateRequest) Options(agents *agentprofile.Store) (core.CreateOptions, error) {
	opts, err := agents.Resolve(req.Agent, req.Cwd, req.Backend, req.Model)
	if err != nil {
		return opts, err
	}
	opts.InitialMessage = req.InitialMessage
	if req.AutoApprove != nil {
		opts.AutoApprove = *req.AutoApprove
	}
	return opts, nil
}

// handleCreateSession spawns a session and returns its id as soon as the
// process is up; the first reply arrives on the session's events.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	opts, err := req.Options(s.agents)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.router.StartSession(opts)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.router.OpenSession(id, 300*time.Millisecond)
	if !ok {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	if sess.Runtime.Effort == "" {
		sess.Runtime.Effort = s.router.DefaultEffort(r.Context(), sess.Backend, sess.Runtime.Model)
	}
	WriteJSON(w, http.StatusOK, Session{
		Session:           sess,
		AutoApprove:       s.router.IsAutoApprove(id),
		Archived:          s.router.IsArchived(id),
		Pinned:            s.router.IsPinned(id),
		Router:            id == s.router.RouterSession(),
		TerminalOpen:      s.router.HasTerminal(id),
		TerminalAvailable: s.router.TerminalAvailable(),
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.router.DeleteSession(r.PathValue("id")); err != nil {
		WriteError(w, errStatus(err), err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// known answers 404 for a session that does not exist.
func (s *Server) known(w http.ResponseWriter, id string) bool {
	if _, ok := s.router.GetSession(id); !ok {
		WriteError(w, http.StatusNotFound, "session not found")
		return false
	}
	return true
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid body")
		return
	}
	title := strings.TrimSpace(req.Title)
	if err := s.router.Rename(r.Context(), id, title); err != nil {
		if errors.Is(err, core.ErrInvalidTitle) {
			WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"title": title})
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	s.router.Archive(id)
	WriteJSON(w, http.StatusOK, map[string]bool{"archived": true})
}

func (s *Server) handleUnarchive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	s.router.Unarchive(id)
	WriteJSON(w, http.StatusOK, map[string]bool{"archived": false})
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	s.router.Pin(id)
	WriteJSON(w, http.StatusOK, map[string]bool{"pinned": true})
}

func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	s.router.Unpin(id)
	WriteJSON(w, http.StatusOK, map[string]bool{"pinned": false})
}

func (s *Server) handleAutoApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	s.router.SetAutoApprove(id, req.Enabled)
	WriteJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
}

// handleFork branches a session at a past turn into a new session. Responds
// with the new session id.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AfterUUID string `json:"after_uuid"` // fork point: the uuid a transcript turn carries
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.AfterUUID == "" {
		WriteError(w, http.StatusBadRequest, "after_uuid is required")
		return
	}
	newID, err := s.router.ForkSession(r.PathValue("id"), req.AfterUUID)
	if err != nil {
		WriteError(w, errStatus(err), err.Error())
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]string{"id": newID})
}

// SendRequest is a prompt for a session.
type SendRequest struct {
	Text string `json:"text"`
}

// handleSend routes a prompt to a session and returns as soon as it is
// accepted — never waiting for the turn.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		WriteError(w, http.StatusBadRequest, "text is required")
		return
	}
	if err := s.router.SendToSession(r.PathValue("id"), req.Text); err != nil {
		WriteError(w, errStatus(err), err.Error())
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) handleCancelSend(w http.ResponseWriter, r *http.Request) {
	if err := s.router.CancelSend(r.PathValue("id")); err != nil {
		WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// TranscriptPage is a run of consecutive turns, oldest first. More reports
// older turns before the first one; pass its cursor as ?before= to get them.
type TranscriptPage struct {
	Turns []core.Turn `json:"turns"`
	More  bool        `json:"more,omitempty"`
}

// handleTranscript pages a session's turns by cursor: the newest page, the page
// just before ?before=, or everything from ?from= on.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	id, q := r.PathValue("id"), r.URL.Query()
	limit := 0
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = n
	}
	var page TranscriptPage
	var err error
	if from := q.Get("from"); from != "" {
		page.Turns, err = s.router.ReadTurnsFrom(id, from)
	} else {
		page.Turns, page.More, err = s.router.ReadTurns(id, q.Get("before"), limit)
	}
	switch {
	case errors.Is(err, core.ErrSessionNotFound):
		// A session that exists but has written no log yet reads as empty.
		if !s.known(w, id) {
			return
		}
	case errors.Is(err, window.ErrBadCursor):
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if page.Turns == nil {
		page.Turns = []core.Turn{}
	}
	WriteJSON(w, http.StatusOK, page)
}

// --- uploads -------------------------------------------------------------

const maxUploadSize = 100 << 20

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	SaveUpload(w, r, s.attachmentsDir, id)
}

// SaveUpload stores the request body as the file named by ?filename= under
// root/dir and answers with its path.
func SaveUpload(w http.ResponseWriter, r *http.Request, root, dir string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	path, err := attachment.Save(root, dir, r.URL.Query().Get("filename"), r.Body, maxUploadSize)
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, attachment.ErrInvalidName):
		WriteError(w, http.StatusBadRequest, "invalid filename")
	case errors.Is(err, attachment.ErrTooLarge) || errors.As(err, &maxErr):
		WriteError(w, http.StatusRequestEntityTooLarge, "file too large")
	case err != nil:
		WriteError(w, http.StatusInternalServerError, "failed to write file")
	default:
		WriteJSON(w, http.StatusOK, map[string]string{"path": path})
	}
}

// --- events --------------------------------------------------------------

// EventInteractionPending is the event for a permission request waiting on an
// answer; its raw is the interaction.Pending.
const EventInteractionPending = "interaction.pending"

// sessionEvents are the broker events a single session's stream carries.
var sessionEvents = map[string]bool{
	backend.EventProcessStarted: true,
	backend.EventProcessExit:    true,
	backend.EventError:          true,
	backend.EventPart:           true,
	backend.EventPartDelta:      true,
	backend.EventTurnStatus:     true,
	backend.EventTurnUser:       true,
	backend.EventRuntime:        true,
	core.EventRelayLanded:       true,
}

// SSEStart asserts streaming support and writes the SSE preamble (headers +
// 200 + first flush). Returns false after answering 500 itself. The single
// home for proxy-facing details like X-Accel-Buffering — fix once, fix every
// stream.
func SSEStart(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

// writeEvent writes one frame: the type as the SSE event name, the whole event
// as its data, so one parser reads either stream.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, ev broker.Event) bool {
	data, err := json.Marshal(ev)
	if err != nil {
		return true // skip the event, keep the stream
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func running(sess core.Session) bool {
	return sess.Status == core.StatusRunning || sess.Status == core.StatusAwaitingPermission
}

// stream forwards events until the client goes away, with a heartbeat so idle
// proxies keep the connection.
func stream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	events <-chan broker.Event, pending <-chan interaction.Pending, keep func(string) bool) {
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		var ev broker.Event
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		case e, ok := <-events:
			if !ok {
				return
			}
			ev = e
		case p, ok := <-pending:
			if !ok {
				return
			}
			ev = pendingEvent(p)
		}
		if keep(ev.Type) && !writeEvent(w, flusher, ev) {
			return
		}
	}
}

func pendingEvent(p interaction.Pending) broker.Event {
	raw, _ := json.Marshal(p)
	return broker.Event{SessionID: p.SessionID, Type: EventInteractionPending, Raw: raw}
}

// handleSessionEvents streams one session's events.
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	// Register before flushing the 200 response: once EventSource observes the
	// connection as open, every later broker event must have a subscriber.
	ch, cancel := s.router.SubscribeSession(id)
	defer cancel()
	flusher, ok := SSEStart(w)
	if !ok {
		return
	}
	// Snapshot-on-connect: the broker has no replay, so a turn whose start (or
	// end) happened before this subscribe is invisible to the client. Emit the
	// current turn state on every connect so a refresh/reconnect can reconcile
	// either way: turn.active stands the bubble back up; turn.idle lets a client
	// that still thinks it's mid-turn (its subprocess.exit was dropped on a
	// dropped connection) finalize. The client handles both idempotently.
	state := "turn.idle"
	if sess, ok := s.router.GetSession(id); ok && running(sess) {
		state = "turn.active"
	}
	if !writeEvent(w, flusher, broker.Event{SessionID: id, Type: state}) {
		return
	}
	stream(r.Context(), w, flusher, ch, nil, func(t string) bool { return sessionEvents[t] })
}

// handleEvents streams every session's events and the pending interactions,
// optionally only the ?types= listed. On connect it reports what is already
// true: a turn.active per running session and each pending interaction.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, cancel := s.router.SubscribeAllSessions()
	defer cancel()
	pending, cancelPending := s.router.SubscribePendingInteractions()
	defer cancelPending()
	flusher, ok := SSEStart(w)
	if !ok {
		return
	}
	keep := func(string) bool { return true }
	if q := r.URL.Query().Get("types"); q != "" {
		types := map[string]bool{}
		for _, t := range strings.Split(q, ",") {
			types[t] = true
		}
		keep = func(t string) bool { return types[t] }
	}
	if keep("turn.active") {
		for _, sess := range s.router.ListSessions() {
			if running(sess) && !writeEvent(w, flusher, broker.Event{SessionID: sess.ID, Type: "turn.active"}) {
				return
			}
		}
	}
	if keep(EventInteractionPending) {
		for _, p := range s.router.ListPendingInteractions() {
			if !writeEvent(w, flusher, pendingEvent(p)) {
				return
			}
		}
	}
	stream(r.Context(), w, flusher, events, pending, keep)
}

// --- interactions --------------------------------------------------------

func (s *Server) handleListInteractions(w http.ResponseWriter, r *http.Request) {
	list := s.router.ListPendingInteractions()
	if list == nil {
		list = []interaction.Pending{}
	}
	WriteJSON(w, http.StatusOK, list)
}

func (s *Server) handleRespondInteraction(w http.ResponseWriter, r *http.Request) {
	var resp interaction.Response
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if resp.Behavior != "allow" && resp.Behavior != "deny" {
		WriteError(w, http.StatusBadRequest, "behavior must be allow|deny")
		return
	}
	if resp.Scope != "" && resp.Scope != "once" && resp.Scope != "session" {
		WriteError(w, http.StatusBadRequest, "scope must be once|session")
		return
	}
	if err := s.router.RespondInteraction(r.PathValue("id"), resp); err != nil {
		WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
