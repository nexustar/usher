package api

import (
	"net/http"

	"github.com/nexustar/usher/pkg/agentprofile"
	"github.com/nexustar/usher/pkg/backend"
)

// AgentList is the body of GET /agents.
type AgentList struct {
	Agents []agentprofile.Profile `json:"agents"`
}

func (s *Server) handleAgents(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, AgentList{Agents: s.Agents.List()})
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var profile agentprofile.Profile
	if !Decode(w, r, &profile) {
		return
	}
	created, err := s.Agents.Create(profile)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	var profile agentprofile.Profile
	if !Decode(w, r, &profile) {
		return
	}
	updated, err := s.Agents.Update(r.PathValue("name"), profile)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.Agents.Delete(r.PathValue("name")); err != nil {
		WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Backend is one enabled agent CLI and the models it offers.
type Backend struct {
	Name      string          `json:"name"`
	Models    []backend.Model `json:"models"`
	Approvals []string        `json:"approvals"`
	Writable  bool            `json:"writable"`
}

// BackendList is the body of GET /backends.
type BackendList struct {
	Backends []Backend `json:"backends"`
}

// handleBackends gives a backend whose catalog cannot be read an empty model list.
func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	names := s.Router.Backends()
	out := make([]Backend, 0, len(names))
	for _, name := range names {
		models, err := s.Router.Models(r.Context(), name)
		if err != nil || models == nil {
			models = []backend.Model{}
		}
		out = append(out, Backend{Name: name, Models: models, Approvals: s.Router.Approvals(name), Writable: s.Router.Writable(name)})
	}
	WriteJSON(w, http.StatusOK, BackendList{Backends: out})
}
