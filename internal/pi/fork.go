package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

type rpcEntry struct {
	Type     string  `json:"type"`
	ID       string  `json:"id"`
	ParentID *string `json:"parentId"`
	Message  struct {
		Role string `json:"role"`
	} `json:"message"`
}

type entriesState struct {
	Entries []rpcEntry `json:"entries"`
	LeafID  string     `json:"leafId"`
}

// forkRPCCommand maps usher's "fork after this assistant turn" onto pi's
// public RPC vocabulary. A historical assistant is equivalent to forking
// before the next user message on the active branch; the final assistant is a
// clone of the current branch.
func forkRPCCommand(state entriesState, afterID string) (string, string, error) {
	if afterID == "" {
		return "", "", errors.New("pi fork point is required")
	}
	byID := make(map[string]rpcEntry, len(state.Entries))
	for _, e := range state.Entries {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}
	if byID[afterID].ID == "" {
		return "", "", fmt.Errorf("pi fork point %q not found", afterID)
	}
	if state.LeafID == "" {
		return "", "", errors.New("pi session has no active leaf")
	}

	var reverse []rpcEntry
	seen := map[string]bool{}
	for id := state.LeafID; id != "" && !seen[id]; {
		seen[id] = true
		e, ok := byID[id]
		if !ok {
			return "", "", fmt.Errorf("pi active branch has broken parent chain at %q", id)
		}
		reverse = append(reverse, e)
		if e.ParentID == nil {
			break
		}
		id = *e.ParentID
	}
	branch := make([]rpcEntry, len(reverse))
	for i := range reverse {
		branch[len(reverse)-1-i] = reverse[i]
	}
	point := -1
	for i := range branch {
		if branch[i].ID == afterID {
			point = i
			break
		}
	}
	if point < 0 {
		return "", "", fmt.Errorf("pi fork point %q is not on the active branch", afterID)
	}
	for _, e := range branch[point+1:] {
		if e.Type == "message" && e.Message.Role == "user" {
			return "fork", e.ID, nil
		}
	}
	return "clone", "", nil
}

// Fork implements backend.Forker using pi's own RPC session operations. The
// text returned by RPC fork is deliberately ignored: usher opens the new
// session with an empty composer instead of restoring the historical prompt.
func (r *Runtime) Fork(ctx context.Context, sourceID, path, afterID string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	r.mu.Lock()
	w := r.workers[sourceID]
	if w != nil && w.busy {
		r.mu.Unlock()
		return "", "", fmt.Errorf("pi session %s is busy", sourceID)
	}
	if w != nil {
		w.busy = true
	}
	r.mu.Unlock()

	cold := w == nil
	if cold {
		meta, err := ReadSessionMeta(path)
		if err != nil {
			return "", "", err
		}
		c, err := startClient(r.bin, meta.Cwd, path, r.sessionsDir, "", r.extra)
		if err != nil {
			return "", "", err
		}
		r.logger.Info("spawn", "session", sourceID, "args", backend.RedactSpawnArgs(c.cmd.Args))
		w = &worker{c: c, cwd: meta.Cwd, last: time.Now()}
	}
	switched := false
	fail := func(err error) (string, string, error) {
		r.cleanupForkFailure(sourceID, w, cold, switched)
		return "", "", err
	}

	raw, err := w.c.request(ctx, "get_entries", nil)
	if err != nil {
		return fail(err)
	}
	var state entriesState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fail(err)
	}
	command, entryID, err := forkRPCCommand(state, afterID)
	if err != nil {
		return fail(err)
	}
	fields := map[string]any(nil)
	if command == "fork" {
		fields = map[string]any{"entryId": entryID}
	}
	if _, err := w.c.request(ctx, command, fields); err != nil {
		return fail(err)
	}
	switched = true
	stateRaw, err := w.c.request(ctx, "get_state", nil)
	if err != nil {
		return fail(err)
	}
	var next struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
	}
	if err := json.Unmarshal(stateRaw, &next); err != nil {
		return fail(err)
	}
	if next.SessionID == "" || next.SessionFile == "" {
		return fail(errors.New("pi fork returned no session id or file"))
	}

	// pi's fork and clone commands switch the RPC process to the new session.
	// Switch it back so an already-live source session remains live, matching
	// the behavior of usher's other backends. The new branch starts lazily on
	// its first send.
	if _, err := w.c.request(ctx, "switch_session", map[string]any{"sessionPath": path}); err != nil {
		return fail(fmt.Errorf("switch pi back to source session: %w", err))
	}
	switched = false
	if cold {
		w.c.stop()
	} else {
		r.mu.Lock()
		if r.workers[sourceID] != w {
			r.mu.Unlock()
			w.c.stop()
			return "", "", fmt.Errorf("pi session %s stopped during fork", sourceID)
		}
		w.busy = false
		w.last = time.Now()
		r.mu.Unlock()
	}
	return next.SessionID, next.SessionFile, nil
}

func (r *Runtime) cleanupForkFailure(sourceID string, w *worker, cold, switched bool) {
	if cold {
		w.c.stop()
		return
	}
	r.mu.Lock()
	owned := r.workers[sourceID] == w
	if owned && switched {
		delete(r.workers, sourceID)
	}
	if owned && !switched {
		w.busy = false
		w.last = time.Now()
	}
	r.mu.Unlock()
	if owned && switched {
		w.c.stop()
	}
}
