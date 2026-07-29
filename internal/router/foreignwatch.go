package router

// Foreign-turn watcher. Sessions complete turns usher did not start —
// background workflow continuations (task-notifications), prompts typed
// straight into an attached tmux pane. runSend's tailer only covers
// usher-initiated turns, so without this nothing observes those completions:
// a deep-research report lands in the transcript and the chat that was
// promised "the reply will follow" never hears about it.
//
// The watcher deliberately does NOT tail: it polls every session log's size
// (a stat each, every few seconds), and when a file has grown past its
// per-session baseline AND its last line is the backend's end-of-turn
// marker, it reads the finished turn with the same whole-file readers the
// transcript API uses and hands the text to the registered handler. No tail
// offsets, no partial-line state; the only coordination with usher's own
// sends is the size baseline, advanced under sendMu whenever an usher turn
// ends, so usher-relayed turns are never re-relayed here.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
)

// ForeignTurnHandler receives the flattened assistant text of a completed
// foreign turn. Called from the watcher goroutine.
type ForeignTurnHandler func(sessionID, text string)

// SetForeignTurnHandler registers the (single) foreign-turn consumer. Call
// before RunForeignWatch.
func (r *Router) SetForeignTurnHandler(h ForeignTurnHandler) { r.onForeignTurn = h }

// RunForeignWatch polls for foreign turns until ctx ends. interval <= 0
// defaults to 2s.
func (r *Router) RunForeignWatch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.scanForeignTurns()
		}
	}
}

// scanForeignTurns runs one poll pass over every discovered session. Subagent
// transcripts participate so an open read-only detail view can refresh when a
// child turn completes; they are still excluded from normal session listings.
func (r *Router) scanForeignTurns() {
	for _, sess := range r.discovery.ListAll() {
		r.checkForeignTurn(sess.ID)
	}
}

func (r *Router) checkForeignTurn(id string) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	size := fi.Size()

	r.sendMu.Lock()
	_, busy := r.activeSend[id]
	queued := len(r.sendQueue[id]) > 0
	base, seen := r.foreignBase[id]
	if !seen {
		// First sight (startup / newly discovered): everything already in
		// the file predates the watcher — baseline it, don't relay history.
		r.setForeignBaseLocked(id, size)
	}
	r.sendMu.Unlock()
	if !seen || busy || queued || size <= base {
		return
	}

	// The file grew outside any usher send. The turn is only DONE when the
	// last line is the end-of-turn marker; otherwise check again next tick.
	line, err := lastFileLine(path, size)
	format, formatErr := r.transcriptForBackend(r.backendOf(id))
	if err != nil || formatErr != nil || !format.IsTurnComplete(line) {
		return
	}

	// Commit the baseline before relaying so a failure can't double-relay.
	// Re-check under the lock: if an usher send started (or already bumped
	// the baseline) while we read the file, this completion is theirs to
	// report — skipping a rare overlapped foreign turn beats duplicating it.
	r.sendMu.Lock()
	if _, nowBusy := r.activeSend[id]; nowBusy || r.foreignBase[id] != base {
		r.sendMu.Unlock()
		return
	}
	r.setForeignBaseLocked(id, size)
	r.sendMu.Unlock()

	turns := foreignTurnsBetweenWith(path, format.NewAssembler(), base, size)
	sess, _ := r.discovery.Get(id)
	if !sess.IsSubagent {
		r.publishForeignTurnEvents(id, turns)
	}

	// Wake any open session-detail view: its turn-end handler refetches the
	// transcript on exit events, so the foreign turn appears without a
	// reload. No relay collector can be subscribed here (sends were
	// excluded above), so nobody mistakes this for an usher turn. The
	// payload carries the same turn stamps a live exit does, built from the
	// just-parsed region (no re-read, no race with newer prompts).
	payload := map[string]any{}
	applyTurnTimestamps(payload, turns)
	exitRaw, err := json.Marshal(payload)
	if err != nil {
		exitRaw = json.RawMessage(`{}`)
	}
	r.broker.Publish(broker.Event{SessionID: id, Type: backend.EventProcessExit, Raw: exitRaw})

	// Subagents are read-only transcript children, not independent foreign
	// conversations. The exit above only wakes their open detail view; never
	// replay their parts or relay their answer into the main chat.
	if sess.IsSubagent {
		return
	}

	if h := r.onForeignTurn; h != nil {
		// Relay every turn completed in (base, size] — chained background
		// continuations can finish more than one turn between polls, and
		// the marker gate above only proves the LAST one is done.
		for _, tn := range turns {
			if tn.Role != "assistant" {
				continue
			}
			if text := flattenTurnText(tn, false); strings.TrimSpace(text) != "" {
				h(id, text)
			}
		}
	}
}

func (r *Router) publishForeignTurnEvents(sessionID string, turns []core.Turn) {
	for _, tn := range turns {
		switch tn.Role {
		case "user":
			raw, err := json.Marshal(map[string]any{"role": "user", "content": tn.Content, "ts": tn.Time})
			if err == nil {
				r.broker.Publish(broker.Event{SessionID: sessionID, Type: backend.EventTurnUser, Raw: raw})
			}
		case "assistant":
			for _, p := range tn.Parts {
				raw, err := json.Marshal(map[string]any{
					"role": "assistant", "ts": tn.Time, "model": tn.Model, "part": p,
				})
				if err == nil {
					r.broker.Publish(broker.Event{SessionID: sessionID, Type: backend.EventPart, Raw: raw})
				}
			}
		}
	}
}

// foreignTurnsBetween parses the log region (base, size] and returns its
// completed turns in order, via the same per-backend assembler the
// live path uses. base always sits at a line boundary in practice (it is
// captured at turn ends / first sight); if a write ever races the capture,
// the torn first line fails to parse and is skipped, never mis-grouped.
func foreignTurnsBetweenWith(path string, asm backend.Assembler, base, size int64) []core.Turn {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, size-base)
	if _, err := f.ReadAt(buf, base); err != nil && err != io.EOF {
		return nil
	}
	var out []core.Turn
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		completed, _ := asm.FeedLine(line)
		out = append(out, completed...)
	}
	// Assemblers close an assistant turn on the NEXT user prompt; the
	// region's final turn (proven complete by the marker gate) is still
	// open and needs an explicit flush.
	if tn := asm.Flush(); tn != nil {
		out = append(out, *tn)
	}
	return out
}

// setForeignBaseLocked records the session's log size below which growth is
// already accounted for. Caller holds sendMu.
func (r *Router) setForeignBaseLocked(id string, size int64) {
	if r.foreignBase == nil {
		r.foreignBase = map[string]int64{}
	}
	r.foreignBase[id] = size
}

// bumpForeignBaseLocked advances the baseline to the log's current size —
// called when an usher-initiated turn ends (markSendIdle/releaseSend, both
// under sendMu), so the watcher never re-reports a turn usher relayed.
func (r *Router) bumpForeignBaseLocked(sessionID string) {
	path, ok := r.discovery.Path(sessionID)
	if !ok {
		return
	}
	if fi, err := os.Stat(path); err == nil {
		r.setForeignBaseLocked(sessionID, fi.Size())
	}
}

// lastFileLine returns the file's final non-empty line, reading at most the
// trailing 8KB (end-of-turn markers are small single lines).
func lastFileLine(path string, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	const window = 8 * 1024
	off := size - window
	if off < 0 {
		off = 0
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	buf = bytes.TrimRight(buf, "\r\n")
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	return buf, nil
}
