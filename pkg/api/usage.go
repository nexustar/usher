package api

import (
	"net/http"
	"sync"

	"github.com/nexustar/usher/pkg/backend"
)

// BackendUsage keeps the last known Usage alongside a failed refresh's Error.
type BackendUsage struct {
	Name  string         `json:"name"`
	Usage *backend.Usage `json:"usage,omitempty"`
	Error string         `json:"error,omitempty"`
	Login *backend.Login `json:"login,omitempty"`
}

// UsageList is the body of GET /usage.
type UsageList struct {
	Backends []BackendUsage `json:"backends"`
}

// handleUsage reads the caches; ?refresh=1 probes in parallel, only the
// ?backend= one if named.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	only := r.URL.Query().Get("backend")
	names := s.Router.Backends()
	results := make([]*BackendUsage, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			u, ok, err := s.Router.Usage(r.Context(), name, refresh && (only == "" || only == name))
			if !ok {
				return
			}
			out := &BackendUsage{Name: name}
			if !u.UpdatedAt.IsZero() || u.Account != nil {
				out.Usage = &u
			}
			if err != nil {
				out.Error = err.Error()
			}
			if l := s.Router.LoginStatus(name); l.State != "" {
				out.Login = &l
			}
			results[i] = out
		})
	}
	wg.Wait()
	list := UsageList{Backends: []BackendUsage{}}
	for _, u := range results {
		if u != nil {
			list.Backends = append(list.Backends, *u)
		}
	}
	WriteJSON(w, http.StatusOK, list)
}

// handleStartLogin answers 202 with the prompt; GET /usage reports the outcome.
func (s *Server) handleStartLogin(w http.ResponseWriter, r *http.Request) {
	l, err := s.Router.StartLogin(r.Context(), r.PathValue("name"))
	if err != nil {
		WriteError(w, ErrStatus(err), err.Error())
		return
	}
	WriteJSON(w, http.StatusAccepted, l)
}

func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !Decode(w, r, &body) {
		return
	}
	if err := s.Router.SubmitLoginCode(r.PathValue("name"), body.Code); err != nil {
		WriteError(w, ErrStatus(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCancelLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.Router.CancelLogin(r.PathValue("name")); err != nil {
		WriteError(w, ErrStatus(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
