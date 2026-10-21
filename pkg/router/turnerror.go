package router

import (
	"time"

	"github.com/nexustar/usher/pkg/core"
)

// turnError is kept in memory because most backends report errors only as a
// live event.
type turnError struct {
	message string
	at      time.Time
	started time.Time
}

func (r *Router) noteTurnError(id, message string, started time.Time) {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if r.turnErrors == nil {
		r.turnErrors = map[string]turnError{}
	}
	r.turnErrors[id] = turnError{message: message, at: time.Now().UTC(), started: started}
}

func (r *Router) clearTurnError(id string) {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	delete(r.turnErrors, id)
}

func (r *Router) withTurnError(id string, turns []core.Turn) []core.Turn {
	r.errMu.Lock()
	e, ok := r.turnErrors[id]
	r.errMu.Unlock()
	if !ok {
		return turns
	}
	if n := len(turns); n > 0 {
		last := turns[n-1]
		// The log moved on, or already holds this turn's error.
		if last.Time.After(e.at) || (last.Role == "error" && !last.Time.Before(e.started)) {
			return turns
		}
	}
	return append(turns, core.Turn{Role: "error", Content: e.message, Time: e.at})
}
