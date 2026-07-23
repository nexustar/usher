package sender

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

// cancelGrace bounds the wait for a backend result after cancellation.
var cancelGrace = 3 * time.Second

// loggedTurnConfig describes the small protocol-specific edges around the
// shared content-plane loop: live deltas and the terminal protocol result.
type loggedTurnConfig[R, D any] struct {
	backend string
	idKey   string
	id      string
	cwd     string
	fresh   bool
	path    string
	offset  int64
	locate  func() string
	tail    tailConfig
	done    <-chan R
	deltas  <-chan D
	delta   func(D) (kind, text string, emit bool)
	result  func(context.Context, chan<- StreamEvent, R)
	logger  *slog.Logger
}

// mergeLoggedTurn is the common Claude/Codex turn loop. Their control
// protocols start turns differently, but both merge protocol deltas and a
// terminal result with the authoritative persisted-log tail.
func mergeLoggedTurn[R, D any](ctx context.Context, cfg loggedTurnConfig[R, D]) <-chan StreamEvent {
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}
	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		started, _ := json.Marshal(backend.ProcessStartedPayload{Cwd: cfg.cwd, Fresh: cfg.fresh})
		if !sendEvent(ctx, out, StreamEvent{Type: backend.EventProcessStarted, Raw: started}) {
			return
		}
		path := cfg.path
		if path == "" {
			path = cfg.locate()
		}
		if path == "" {
			emitError(ctx, out, cfg.backend+" session log did not appear after prompt")
			return
		}
		// Keep the tail alive until backend completion supplies final records.
		tailCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()
		events := tailTurn(tailCtx, path, cfg.offset, cfg.logger, cfg.tail)
		deltas := cfg.deltas
		for {
			select {
			case delta, ok := <-deltas:
				if !ok {
					deltas = nil
					continue
				}
				kind, value, emit := cfg.delta(delta)
				if emit && !emitLiveDelta(ctx, out, kind, value) {
					finishCancelled(out, cfg, events, cancel)
					return
				}
			case ev, ok := <-events:
				if !ok {
					select {
					case result := <-cfg.done:
						cfg.result(context.WithoutCancel(ctx), out, result)
					case <-ctx.Done():
					}
					return
				}
				if !sendEvent(ctx, out, ev) {
					finishCancelled(out, cfg, events, cancel, ev)
					return
				}
			case result := <-cfg.done:
				// Detach finalization from a concurrent cancellation.
				finalCtx := context.WithoutCancel(ctx)
				cfg.result(finalCtx, out, result)
				drainTail(finalCtx, out, events, cancel)
				return
			case <-ctx.Done():
				finishCancelled(out, cfg, events, cancel)
				return
			}
		}
	}()
	return out
}

// finishCancelled waits briefly for the backend, drains the tail, and ensures
// the cancelled turn emits subprocess.exit. pending is a raced tail event.
func finishCancelled[R, D any](out chan<- StreamEvent, cfg loggedTurnConfig[R, D], events <-chan StreamEvent, cancel context.CancelFunc, pending ...StreamEvent) {
	ctx := context.Background()
	exited := false
	send := func(ev StreamEvent) {
		exited = exited || ev.Type == backend.EventProcessExit
		sendEvent(ctx, out, ev)
	}
	for _, ev := range pending {
		send(ev)
	}
	grace := time.NewTimer(cancelGrace)
	defer grace.Stop()
	for waiting := true; waiting; {
		select {
		case ev, ok := <-events:
			if !ok {
				events, waiting = nil, false
				continue
			}
			send(ev)
		case result := <-cfg.done:
			cfg.result(ctx, out, result)
			waiting = false
		case <-grace.C:
			cfg.logger.Warn(cfg.backend+" cancelled turn finalized without a backend result", cfg.idKey, cfg.id)
			waiting = false
		}
	}
	if events != nil {
		// Stopping the tailer triggers its final EOF drain.
		cancel()
		for ev := range events {
			send(ev)
		}
	}
	if !exited {
		// User-requested cancellation reconciles as a normal turn end.
		send(StreamEvent{Type: backend.EventProcessExit, Raw: json.RawMessage(`{}`)})
	}
}
