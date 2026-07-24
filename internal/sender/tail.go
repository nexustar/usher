package sender

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/jsonl"
)

// tailConfig tunes the turn tailer. Zero values fall back to sane defaults
// in tailTurn so callers (and tests) only set what they care about.
type tailConfig struct {
	poll       time.Duration // how often to re-check the file for growth
	appearWait time.Duration // how long to wait for a not-yet-created file

	// contentOnly leaves lifecycle completion to the backend protocol.
	contentOnly bool

	// turnComplete reports whether a session-log line is the backend's
	// end-of-turn marker. Backend-specific: Claude Code uses system/turn_duration
	// (the default below); Codex uses event_msg/task_complete. nil → Claude.
	turnComplete    func(line []byte) bool
	skipCompletions int // completed turns already ahead of this queued send
	turnAborted     func(line []byte) bool
}

func (c tailConfig) withDefaults() tailConfig {
	if c.poll <= 0 {
		c.poll = 100 * time.Millisecond
	}
	if c.appearWait <= 0 {
		c.appearWait = 15 * time.Second
	}
	if c.turnComplete == nil {
		c.turnComplete = isTurnComplete
	}
	return c
}

// tailTurn follows the session jsonl at path starting from byteOffset and
// streams the events appended during the current turn. byteOffset is the file
// size captured just before the prompt was injected, so the tailer reports
// only this turn's new lines.
//
// Output: each complete jsonl line becomes a StreamEvent{Type, Raw} (Type is
// the line's "type" field). When Claude Code logs its end-of-turn marker (a
// "system/turn_duration" event — see isTurnComplete), the tailer emits a
// synthesized "subprocess.exit" and closes the channel.
//
// The channel also closes on ctx cancellation, or if the file never appears
// within cfg.appearWait (brand-new sessions create their jsonl lazily). Process
// lifecycle is owned by the headless backend manager, not the tailer.
func tailTurn(ctx context.Context, path string, byteOffset int64, logger *slog.Logger, cfg tailConfig) <-chan StreamEvent {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	out := make(chan StreamEvent, 64)

	go func() {
		defer close(out)
		// A turn ends one of two ways: Claude Code logs its turn_duration marker,
		// or the send is cancelled — the cancel button, or an esc the user pressed
		// in the mirror, which interrupts claude but (being an interrupt) never
		// logs turn_duration. Emit subprocess.exit for both so the web UI finalizes
		// the turn instead of waiting forever on a marker that isn't coming.
		emitExit := func() {
			sendEvent(context.Background(), out, StreamEvent{Type: backend.EventProcessExit, Raw: json.RawMessage(`{}`)})
		}
		emitExitReason := func(reason string) {
			raw, _ := json.Marshal(map[string]string{"reason": reason})
			sendEvent(context.Background(), out, StreamEvent{Type: backend.EventProcessExit, Raw: raw})
		}
		emitIOError := func(msg string) {
			raw, _ := json.Marshal(map[string]string{"message": msg})
			sendEvent(context.Background(), out, StreamEvent{Type: backend.EventError, Raw: raw})
			emitExitReason("tail_error")
		}

		f, ok := openWhenReady(ctx, path, cfg, out)
		if !ok {
			// Cancelled before the file appeared: emit exit so the UI
			// finalizes the turn. The appearWait timeout (ctx not cancelled)
			// already emitted an error, so skip it there.
			if ctx.Err() != nil {
				emitExit()
			}
			return
		}
		defer f.Close()

		if _, err := f.Seek(byteOffset, io.SeekStart); err != nil {
			logger.Warn("tail seek", "path", path, "offset", byteOffset, "err", err)
			emitIOError("tail seek: " + err.Error())
			return
		}

		reader := bufio.NewReader(f)
		var pending []byte // partial trailing line not yet terminated by '\n'
		ticker := time.NewTicker(cfg.poll)
		defer ticker.Stop()
		cancelled := false

		for {
			for {
				chunk, err := reader.ReadBytes('\n')
				if len(chunk) > 0 {
					pending = append(pending, chunk...)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					logger.Warn("tail read", "path", path, "err", err)
					emitIOError("tail read: " + err.Error())
					return
				}
				// Complete line (ends in '\n').
				line := bytes.TrimRight(pending, "\r\n")
				pending = nil
				if len(line) == 0 {
					continue
				}
				// The turn is done when Claude Code logs its end-of-turn
				// "system/turn_duration" event — NOT when an assistant message
				// carries stop_reason "end_turn" (interactive claude stamps
				// end_turn on intermediate thinking/tool_use messages too, so
				// trusting it ends the turn before the tool even runs — which
				// released ownership and sent permission prompts to the pane).
				if !cfg.contentOnly && cfg.turnComplete(line) {
					if cfg.skipCompletions > 0 {
						cfg.skipCompletions--
						ev := StreamEvent{Type: lineType(line), Raw: append(json.RawMessage(nil), line...)}
						_ = sendEvent(context.Background(), out, ev)
						continue
					}
					emitExit()
					return
				}
				if !cfg.contentOnly && cfg.turnAborted != nil && cfg.turnAborted(line) {
					ev := StreamEvent{Type: lineType(line), Raw: append(json.RawMessage(nil), line...)}
					_ = sendEvent(context.Background(), out, ev)
					errRaw, _ := json.Marshal(map[string]string{"message": "turn aborted before completion"})
					_ = sendEvent(context.Background(), out, StreamEvent{Type: backend.EventError, Raw: errRaw})
					emitExitReason("turn_aborted")
					return
				}
				ev := StreamEvent{Type: lineType(line), Raw: append(json.RawMessage(nil), line...)}
				sendCtx := ctx
				if cancelled || ctx.Err() != nil {
					// Forward persisted records during the final EOF drain.
					sendCtx = context.Background()
				}
				if !sendEvent(sendCtx, out, ev) {
					emitExit() // ctx cancelled mid-stream
					return
				}
			}
			if cancelled {
				emitExit()
				return
			}

			select {
			case <-ctx.Done():
				// Read through EOF once more before exiting.
				cancelled = true
			case <-ticker.C:
			}
		}
	}()

	return out
}

// openWhenReady opens path, waiting up to cfg.appearWait for it to exist. A
// brand-new session's jsonl is created only once claude writes its first
// event, so SendNew callers race the file into existence.
func openWhenReady(ctx context.Context, path string, cfg tailConfig, out chan<- StreamEvent) (*os.File, bool) {
	deadline := time.NewTimer(cfg.appearWait)
	defer deadline.Stop()
	ticker := time.NewTicker(cfg.poll)
	defer ticker.Stop()

	for {
		if f, err := os.Open(path); err == nil {
			return f, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-deadline.C:
			errMsg, _ := json.Marshal(map[string]string{"message": "session jsonl did not appear: " + path})
			sendEvent(context.Background(), out, StreamEvent{Type: backend.EventError, Raw: errMsg})
			return nil, false
		case <-ticker.C:
		}
	}
}

func lineType(line []byte) string {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return ""
	}
	return head.Type
}

// isTurnComplete reports whether line is Claude Code's end-of-turn marker: a
// "system" event with subtype "turn_duration" (carries durationMs/messageCount).
// It is written once, after the final assistant message, only when the turn has
// truly finished — so unlike assistant stop_reason it does not fire mid-turn
// (during thinking or a pending tool call).
func isTurnComplete(line []byte) bool {
	return jsonl.IsTurnComplete(line)
}

// isClaudeTurnAborted matches the "[Request interrupted …]" user record Claude
// writes on cancel (the abort path logs no turn_duration). It matches a whole
// text block, not a substring, so a prompt quoting the phrase is not mistaken
// for the marker.
func isClaudeTurnAborted(line []byte) bool {
	var o struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &o) != nil || o.Type != "user" {
		return false
	}
	for _, b := range o.Message.Content {
		if b.Type == "text" && strings.HasPrefix(b.Text, "[Request interrupted by user") {
			return true
		}
	}
	return false
}

// terminalMarker reports whether line is this backend's end-of-turn marker
// (completion or abort). The control protocol owns lifecycle under contentOnly;
// the drain uses this only as the deterministic "content finished" signal to
// skip the silence window. Backends without a durable marker leave both nil.
func (c tailConfig) terminalMarker(line []byte) bool {
	if c.turnComplete != nil && c.turnComplete(line) {
		return true
	}
	if c.turnAborted != nil && c.turnAborted(line) {
		return true
	}
	return false
}
