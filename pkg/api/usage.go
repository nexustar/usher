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
}

// UsageList is the body of GET /usage.
type UsageList struct {
	Backends []BackendUsage `json:"backends"`
}

// handleUsage reads the caches; ?refresh=1 probes in parallel.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	names := s.Router.Backends()
	results := make([]*BackendUsage, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			u, ok, err := s.Router.Usage(r.Context(), name, refresh)
			if !ok {
				return
			}
			out := &BackendUsage{Name: name}
			if !u.UpdatedAt.IsZero() {
				out.Usage = &u
			}
			if err != nil {
				out.Error = err.Error()
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
