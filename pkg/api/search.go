package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/nexustar/usher/pkg/core"
)

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
		WriteError(w, http.StatusBadRequest, "query is required")
		return
	}
	contextChars := queryInt(r, "context_chars", 120, 500)
	if id := r.URL.Query().Get("session_id"); id != "" {
		hits, truncated, err := s.Router.SearchSessionTranscript(id, query, queryInt(r, "limit", 20, 100), contextChars)
		if err != nil {
			WriteError(w, ErrStatus(err), err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, SearchResult{
			Scope: "session", SessionID: id, Hits: hits, Truncated: truncated,
		})
		return
	}
	results, truncated, err := s.Router.SearchAllSessions(query, queryInt(r, "limit", 20, 50), contextChars)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, SearchResult{Scope: "all", Results: results, Truncated: truncated})
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
