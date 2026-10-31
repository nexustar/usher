package api

import (
	"errors"
	"net/http"

	"github.com/nexustar/usher/pkg/agentupdate"
)

// UpdateList is the body of GET /updates.
type UpdateList struct {
	Agents []agentupdate.Status `json:"agents"`
}

// handleUpdates reads the last check; ?refresh=1 checks first.
func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		s.Updates.Check(r.Context())
	}
	WriteJSON(w, http.StatusOK, UpdateList{Agents: s.Updates.List()})
}

// handleStartUpdate answers 202; GET /updates reports the outcome.
func (s *Server) handleStartUpdate(w http.ResponseWriter, r *http.Request) {
	switch err := s.Updates.Update(r.PathValue("name")); {
	case errors.Is(err, agentupdate.ErrUnknown):
		WriteError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, agentupdate.ErrBusy):
		WriteError(w, http.StatusConflict, err.Error())
	case err != nil:
		WriteError(w, http.StatusInternalServerError, err.Error())
	default:
		WriteJSON(w, http.StatusAccepted, UpdateList{Agents: s.Updates.List()})
	}
}
