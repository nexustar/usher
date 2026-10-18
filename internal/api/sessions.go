package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/agentprofile"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/window"
)

// Session is a core.Session plus the flags usher keeps outside the log.
type Session struct {
	core.Session
	AutoApprove       bool `json:"auto_approve"`
	Archived          bool `json:"archived"`
	Pinned            bool `json:"pinned"`
	Router            bool `json:"router,omitempty"`
	TerminalOpen      bool `json:"terminal_open,omitempty"`
	TerminalAvailable bool `json:"terminal_available,omitempty"`
	// AppendSystemPrompt and ExtraArgs are the launch options, on a single
	// session's view only.
	AppendSystemPrompt string   `json:"append_system_prompt,omitempty"`
	ExtraArgs          []string `json:"extra_args,omitempty"`
}

// SessionList is the body of GET /sessions.
type SessionList struct {
	Sessions []Session `json:"sessions"`
}

// handleListSessions returns root sessions by default; include_archived=1 and
// include_subagents=1 add the archived roots and the read-only subagents.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("include_archived") == "1"
	includeSubagents := r.URL.Query().Get("include_subagents") == "1"
	sessions := s.Router.ListSessions()
	if includeSubagents {
		sessions = s.Router.ListSessionsWithSubagents()
	}
	routerID := s.Router.RouterSession()
	out := make([]Session, 0, len(sessions))
	for _, sess := range sessions {
		if sess.IsSubagent {
			out = append(out, Session{Session: sess})
			continue
		}
		archived := s.Router.IsArchived(sess.ID)
		if archived && !includeArchived {
			continue
		}
		out = append(out, Session{
			Session:     sess,
			AutoApprove: s.Router.IsAutoApprove(sess.ID),
			Archived:    archived,
			Pinned:      s.Router.IsPinned(sess.ID),
			Router:      sess.ID == routerID,
		})
	}
	WriteJSON(w, http.StatusOK, SessionList{Sessions: out})
}

// view is the full Session for id; a wait above zero opens it first. A
// session not found reads as just its id.
func (s *Server) view(ctx context.Context, id string, wait time.Duration) (Session, bool) {
	var sess core.Session
	var ok bool
	if wait > 0 {
		sess, ok = s.Router.OpenSession(id, wait)
	} else {
		sess, ok = s.Router.GetSession(id)
	}
	if !ok {
		return Session{Session: core.Session{ID: id}}, false
	}
	if sess.Runtime.Effort == "" {
		sess.Runtime.Effort = s.Router.DefaultEffort(ctx, sess.Backend, sess.Runtime.Model)
	}
	prompt, args := s.Router.LaunchOptions(id)
	return Session{
		Session:            sess,
		AutoApprove:        s.Router.IsAutoApprove(id),
		Archived:           s.Router.IsArchived(id),
		Pinned:             s.Router.IsPinned(id),
		Router:             id == s.Router.RouterSession(),
		TerminalOpen:       s.Router.HasTerminal(id),
		TerminalAvailable:  s.Router.TerminalAvailable(),
		AppendSystemPrompt: prompt,
		ExtraArgs:          args,
	}, true
}

// WriteCreated answers 201 with the new session, as POST /sessions does.
func (s *Server) WriteCreated(w http.ResponseWriter, r *http.Request, id string) {
	sess, _ := s.view(r.Context(), id, 300*time.Millisecond)
	w.Header().Set("Location", Prefix+"/sessions/"+id)
	WriteJSON(w, http.StatusCreated, sess)
}

// CreateRequest spawns a session. When Agent names a configured agent,
// non-empty Cwd/Backend/Model/Effort override that agent's defaults; without it they
// are the request in full. AutoApprove is a pointer: omitted keeps the agent's
// default, an explicit false turns it off for this session only.
type CreateRequest struct {
	Agent          string `json:"agent,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	InitialMessage string `json:"initial_message"`
	Backend        string `json:"backend,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	AutoApprove    *bool  `json:"auto_approve,omitempty"`
}

// Options resolves the request against the configured agents.
func (req CreateRequest) Options(agents *agentprofile.Store) (core.CreateOptions, error) {
	opts, err := agents.Resolve(req.Agent, req.Cwd, req.Backend, req.Model, req.Effort)
	if err != nil {
		return opts, err
	}
	opts.InitialMessage = req.InitialMessage
	if req.AutoApprove != nil {
		opts.AutoApprove = *req.AutoApprove
	}
	return opts, nil
}

// handleCreateSession spawns a session and answers as soon as the process is
// up; the first reply arrives on the session's events.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if !Decode(w, r, &req) {
		return
	}
	opts, err := req.Options(s.Agents)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.Router.StartSession(opts)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.WriteCreated(w, r, id)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.view(r.Context(), r.PathValue("id"), 300*time.Millisecond)
	if !ok {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	WriteJSON(w, http.StatusOK, sess)
}

// PatchRequest sets the flags usher keeps about a session; omitted fields are
// left as they are. Model and Effort switch the session from its next model
// request; Effort "default" drops a Claude switch. After a usher restart a
// Claude session keeps the model its last turn ran on but not the effort.
// ExtraArgs apply from the session's next spawn — pause and resume it to apply
// them now; an empty list clears them.
type PatchRequest struct {
	Title       *string   `json:"title,omitempty"`
	Archived    *bool     `json:"archived,omitempty"`
	Pinned      *bool     `json:"pinned,omitempty"`
	AutoApprove *bool     `json:"auto_approve,omitempty"`
	Model       string    `json:"model,omitempty"`
	Effort      string    `json:"effort,omitempty"`
	ExtraArgs   *[]string `json:"extra_args,omitempty"`
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	var req PatchRequest
	if !Decode(w, r, &req) {
		return
	}
	if req.Model != "" || req.Effort != "" {
		if err := s.Router.SetRuntime(r.Context(), id, req.Model, req.Effort); err != nil {
			WriteError(w, ErrStatus(err), err.Error())
			return
		}
	}
	if req.ExtraArgs != nil {
		if err := s.Router.SetExtraArgs(id, *req.ExtraArgs); err != nil {
			WriteError(w, ErrStatus(err), err.Error())
			return
		}
	}
	if req.Title != nil {
		if err := s.Router.Rename(r.Context(), id, strings.TrimSpace(*req.Title)); err != nil {
			switch {
			case errors.Is(err, core.ErrInvalidTitle):
				WriteError(w, http.StatusBadRequest, err.Error())
			case errors.Is(err, core.ErrSessionNotFound):
				WriteError(w, http.StatusNotFound, err.Error())
			default:
				WriteError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
	}
	if req.Archived != nil {
		if *req.Archived {
			s.Router.Archive(id)
		} else {
			s.Router.Unarchive(id)
		}
	}
	if req.Pinned != nil {
		if *req.Pinned {
			s.Router.Pin(id)
		} else {
			s.Router.Unpin(id)
		}
	}
	if req.AutoApprove != nil {
		s.Router.SetAutoApprove(id, *req.AutoApprove)
	}
	sess, _ := s.view(r.Context(), id, 0)
	WriteJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.Router.DeleteSession(r.PathValue("id")); err != nil {
		WriteError(w, ErrStatus(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFork branches a session at a past turn into a new session.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AfterUUID string `json:"after_uuid"` // fork point: the uuid a transcript turn carries
	}
	if !Decode(w, r, &req) {
		return
	}
	if req.AfterUUID == "" {
		WriteError(w, http.StatusBadRequest, "after_uuid is required")
		return
	}
	newID, err := s.Router.ForkSession(r.PathValue("id"), req.AfterUUID)
	if err != nil {
		WriteError(w, ErrStatus(err), err.Error())
		return
	}
	s.WriteCreated(w, r, newID)
}

// MessageRequest is a prompt for a session.
type MessageRequest struct {
	Text string `json:"text"`
}

// handleMessage routes a prompt to a session and answers as soon as it is
// accepted — never waiting for the turn.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req MessageRequest
	if !Decode(w, r, &req) {
		return
	}
	if req.Text == "" {
		WriteError(w, http.StatusBadRequest, "text is required")
		return
	}
	if err := s.Router.SendToSession(r.PathValue("id"), req.Text); err != nil {
		WriteError(w, ErrStatus(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.known(w, id) {
		return
	}
	if err := s.Router.CancelSend(id); err != nil {
		WriteError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.Router.PauseSession(r.PathValue("id")); err != nil {
		WriteError(w, ErrStatus(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.Router.ResumeSession(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, core.ErrSessionNotFound) {
			WriteError(w, http.StatusNotFound, err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TranscriptPage is a run of consecutive turns, oldest first. HasMore reports
// older turns before the first one; pass its cursor as ?before= to get them.
type TranscriptPage struct {
	Turns   []core.Turn `json:"turns"`
	HasMore bool        `json:"has_more,omitempty"`
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
		page.Turns, err = s.Router.ReadTurnsFrom(id, from)
	} else {
		page.Turns, page.HasMore, err = s.Router.ReadTurns(id, q.Get("before"), limit)
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

// Commands is the body of GET /sessions/{id}/commands.
type Commands struct {
	Source    string                 `json:"source"`
	Available bool                   `json:"available"`
	Commands  []backend.ComposerItem `json:"commands"`
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.Router.GetSession(id)
	if !ok {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	catalog, err := s.Router.ComposerItems(r.Context(), id, sess.Cwd)
	if err != nil {
		WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	if catalog.Items == nil {
		catalog.Items = []backend.ComposerItem{}
	}
	WriteJSON(w, http.StatusOK, Commands{Source: sess.Backend, Available: catalog.Available, Commands: catalog.Items})
}
