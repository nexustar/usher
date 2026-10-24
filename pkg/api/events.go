package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/broker"
	"github.com/nexustar/usher/pkg/core"
	"github.com/nexustar/usher/pkg/interaction"
)

// EventInteractionPending is the event for a permission request waiting on an
// answer; its data is the interaction.Pending.
const EventInteractionPending = "interaction.pending"

// sessionEvents are the broker events a single session's stream carries.
var sessionEvents = map[string]bool{
	backend.EventTurnStarted: true,
	backend.EventTurnEnded:   true,
	backend.EventTurnFailed:  true,
	backend.EventTurnPart:    true,
	backend.EventTurnDelta:   true,
	backend.EventTurnStatus:  true,
	backend.EventTurnPrompt:  true,
	backend.EventTurnNotice:  true,
	backend.EventRuntime:     true,
	core.EventRelayLanded:    true,
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
// as its data.
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
	ch, cancel := s.Router.SubscribeSession(id)
	defer cancel()
	flusher, ok := SSEStart(w)
	if !ok {
		return
	}
	// Snapshot-on-connect: the broker has no replay, so a turn whose start (or
	// end) happened before this subscribe is invisible to the client. Emit the
	// current turn state on every connect so a refresh/reconnect can reconcile
	// either way: turn.active stands the bubble back up; turn.idle lets a client
	// that still thinks it's mid-turn (its turn.ended was dropped on a
	// dropped connection) finalize. The client handles both idempotently.
	state := "turn.idle"
	if sess, ok := s.Router.GetSession(id); ok && running(sess) {
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
	events, cancel := s.Router.SubscribeAllSessions()
	defer cancel()
	pending, cancelPending := s.Router.SubscribePendingInteractions()
	defer cancelPending()
	flusher, ok := SSEStart(w)
	if !ok {
		return
	}
	keep := func(t string) bool {
		return sessionEvents[t] || t == EventInteractionPending || t == "turn.active"
	}
	if q := r.URL.Query().Get("types"); q != "" {
		types := map[string]bool{}
		for _, t := range strings.Split(q, ",") {
			types[t] = true
		}
		public := keep
		keep = func(t string) bool { return types[t] && public(t) }
	}
	if keep("turn.active") {
		for _, sess := range s.Router.ListSessions() {
			if running(sess) && !writeEvent(w, flusher, broker.Event{SessionID: sess.ID, Type: "turn.active"}) {
				return
			}
		}
	}
	if keep(EventInteractionPending) {
		for _, p := range s.Router.ListPendingInteractions() {
			if !writeEvent(w, flusher, pendingEvent(p)) {
				return
			}
		}
	}
	stream(r.Context(), w, flusher, events, pending, keep)
}
