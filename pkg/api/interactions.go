package api

import (
	"errors"
	"net/http"

	"github.com/nexustar/usher/pkg/interaction"
)

// InteractionList is the body of GET /interactions.
type InteractionList struct {
	Interactions []interaction.Pending `json:"interactions"`
}

func (s *Server) handleListInteractions(w http.ResponseWriter, r *http.Request) {
	list := s.Router.ListPendingInteractions()
	if list == nil {
		list = []interaction.Pending{}
	}
	WriteJSON(w, http.StatusOK, InteractionList{Interactions: list})
}

func (s *Server) handleRespondInteraction(w http.ResponseWriter, r *http.Request) {
	var resp interaction.Response
	if !Decode(w, r, &resp) {
		return
	}
	if resp.Behavior != "allow" && resp.Behavior != "deny" {
		WriteError(w, http.StatusBadRequest, "behavior must be allow|deny")
		return
	}
	if resp.Scope != "" && resp.Scope != "once" && resp.Scope != "always" {
		WriteError(w, http.StatusBadRequest, "scope must be once|always")
		return
	}
	if err := s.Router.RespondInteraction(r.PathValue("id"), resp); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, interaction.ErrNotFound) {
			status = http.StatusNotFound
		}
		WriteError(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
