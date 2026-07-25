package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

// cancelGrace bounds the wait for agent_settled after cancellation.
var cancelGrace = 3 * time.Second

type rpcResponse struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

type client struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	mu      sync.Mutex
	seq     uint64
	pending map[string]chan rpcResponse
	events  chan json.RawMessage
	done    chan struct{}
	err     error
}

func startClient(bin, cwd, sessionPath, sessionsDir, model string, extra []string) (*client, error) {
	args := []string{"--mode", "rpc", "--session-dir", sessionsDir}
	if sessionPath != "" {
		args = append(args, "--session", sessionPath)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	// The official installer places pi and its required Node runtime in the
	// same bin directory. A daemon often does not source ~/.bashrc; without this
	// explicit PATH, pi's /usr/bin/env node shebang can select an older system
	// Node even when --pi points at the correct executable.
	if resolved, err := exec.LookPath(bin); err == nil {
		cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(resolved)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &client{cmd: cmd, in: in, pending: map[string]chan rpcResponse{}, events: make(chan json.RawMessage, 128), done: make(chan struct{})}
	go c.readLoop(out)
	return c, nil
}

func (c *client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		raw := append(json.RawMessage(nil), sc.Bytes()...)
		var head struct{ Type, ID string }
		if json.Unmarshal(raw, &head) != nil {
			continue
		}
		if head.Type == "response" && head.ID != "" {
			var resp rpcResponse
			if json.Unmarshal(raw, &resp) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[head.ID]
			delete(c.pending, head.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- resp
				close(ch)
			}
			continue
		}
		c.events <- raw
	}
	c.mu.Lock()
	c.err = sc.Err()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.events)
	// Every successful Start must be paired with exactly one Wait. Keeping it
	// in the sole stdout-reader goroutine also reaps clients that exit before a
	// caller explicitly stops them (notably ephemeral model-list clients).
	if err := c.cmd.Wait(); err != nil {
		c.mu.Lock()
		if c.err == nil {
			c.err = err
		}
		c.mu.Unlock()
	}
	close(c.done)
}

func (c *client) request(ctx context.Context, typ string, fields map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.seq++
	id := strconv.FormatUint(c.seq, 10)
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	v := map[string]any{"id": id, "type": typ}
	for k, x := range fields {
		v[k] = x
	}
	b, _ := json.Marshal(v)
	_, err := c.in.Write(append(b, '\n'))
	if err != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("pi RPC process exited")
		}
		if !resp.Success {
			return nil, fmt.Errorf("pi %s: %s", typ, resp.Error)
		}
		// Extensions report cancelled fork/clone/switch_session operations in
		// data even when success=true; callers must not commit those transitions.
		if rpcDataCancelled(resp.Data) {
			return nil, fmt.Errorf("pi %s: cancelled", typ)
		}
		return resp.Data, nil
	}
}

func rpcDataCancelled(raw json.RawMessage) bool {
	var data struct {
		Cancelled bool `json:"cancelled"`
	}
	return json.Unmarshal(raw, &data) == nil && data.Cancelled
}

func readRuntime(ctx context.Context, c *client) (core.SessionRuntime, error) {
	var runtime core.SessionRuntime
	stateRaw, err := c.request(ctx, "get_state", nil)
	if err != nil {
		return runtime, err
	}
	var state struct {
		Model *struct {
			ID string `json:"id"`
		} `json:"model"`
		ThinkingLevel string `json:"thinkingLevel"`
	}
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		return runtime, err
	}
	if state.Model != nil {
		runtime.Model = state.Model.ID
	}
	runtime.Effort = state.ThinkingLevel

	statsRaw, err := c.request(ctx, "get_session_stats", nil)
	if err != nil {
		return runtime, err
	}
	var stats struct {
		ContextUsage *struct {
			Tokens        *int64 `json:"tokens"`
			ContextWindow int64  `json:"contextWindow"`
		} `json:"contextUsage"`
	}
	if err := json.Unmarshal(statsRaw, &stats); err != nil {
		return runtime, err
	}
	if stats.ContextUsage != nil {
		runtime.ContextWindow = stats.ContextUsage.ContextWindow
		if stats.ContextUsage.Tokens != nil {
			runtime.ContextTokens = *stats.ContextUsage.Tokens
		}
	}
	return runtime, nil
}

func emitRuntime(out chan<- backend.Event, runtime core.SessionRuntime) {
	if runtime == (core.SessionRuntime{}) {
		return
	}
	raw, _ := json.Marshal(runtime)
	out <- backend.Event{Type: backend.EventRuntime, Raw: raw}
}

func (c *client) send(v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.in.Write(append(b, '\n'))
	return err
}

// Models reads the last account-aware catalog captured from a real pi worker.
// IDs include the provider because model ids are not globally unique in pi.
type Models struct {
	Path string
}

type modelsDocument struct {
	Models []backend.Model `json:"models"`
}

func modelsFromRPC(data json.RawMessage) ([]backend.Model, error) {
	var payload struct {
		Models []struct {
			ID, Name, Provider string
			Reasoning          bool
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	out := make([]backend.Model, 0, len(payload.Models))
	for _, model := range payload.Models {
		if model.ID == "" || model.Provider == "" {
			continue
		}
		levels := []string(nil)
		if model.Reasoning {
			levels = []string{"off", "minimal", "low", "medium", "high", "xhigh"}
		}
		name := model.Name
		if name == "" {
			name = model.ID
		}
		out = append(out, backend.Model{ID: model.Provider + "/" + model.ID, DisplayName: name, ThinkingLevels: levels})
	}
	return out, nil
}

func (m Models) Models(context.Context) ([]backend.Model, error) {
	raw, err := os.ReadFile(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc modelsDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Models, nil
}

func (m Models) refresh(ctx context.Context, c *client) error {
	data, err := c.request(ctx, "get_available_models", nil)
	if err != nil {
		return err
	}
	models, err := modelsFromRPC(data)
	if err != nil {
		return err
	}
	return m.write(models)
}

func (m Models) write(models []backend.Model) error {
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.Path), ".pi-models-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(modelsDocument{Models: models}); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.Path); err != nil {
		return err
	}
	ok = true
	return nil
}

func (m Models) ValidateModel(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	models, err := m.Models(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range models {
		if candidate.ID == id {
			return nil
		}
	}
	return fmt.Errorf("unknown pi model %q", id)
}
func (Models) DefaultEffort(context.Context, string) (string, error) { return "", nil }
func (c *client) stop() {
	_ = c.in.Close()
	select {
	case <-c.done:
	case <-time.After(time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

type worker struct {
	c      *client
	busy   bool
	leases int
	last   time.Time
	cwd    string
	path   string
}

// Runtime owns warm pi RPC processes. Persisted JSONL remains the content
// source; RPC supplies control, deltas, and the definitive settled signal.
type Runtime struct {
	bin, sessionsDir string
	extra            []string
	max              int
	models           Models
	logger           *slog.Logger
	hooks            *hook.Manager
	mu               sync.Mutex
	workers          map[string]*worker
}

// Rename uses RPC when live and native metadata when idle.
func (r *Runtime) Rename(ctx context.Context, id, path, title string) error {
	w := r.leaseWorkerIfLive(id)
	if w == nil {
		return RenameSession(path, title)
	}
	defer r.releaseWorker(w)
	_, err := w.c.request(ctx, "set_session_name", map[string]any{"name": title})
	return err
}

func NewRuntime(bin, sessionsDir string, extra []string, max int, models Models, hooks *hook.Manager, logger *slog.Logger) *Runtime {
	if max <= 0 {
		max = 8
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{bin: bin, sessionsDir: sessionsDir, extra: append([]string(nil), extra...), max: max, models: models, hooks: hooks, logger: logger, workers: map[string]*worker{}}
}

func (r *Runtime) refreshModels(ctx context.Context, c *client) {
	if r.models.Path == "" {
		return
	}
	if err := r.models.refresh(ctx, c); err != nil {
		r.logger.Debug("pi model catalog refresh failed", "err", err)
	}
}

type extensionUIRequest struct {
	Type    string   `json:"type"`
	ID      string   `json:"id"`
	Method  string   `json:"method"`
	Title   string   `json:"title"`
	Options []string `json:"options"`
}

var piPermissionSystemOptions = []string{"Allow Once", "Allow Always", "Reject", "Reject with Reason"}

// isPiPermissionSystemRequest deliberately recognizes only the stable prompt
// shape emitted by npm:pi-permission-system. Pi's extension UI protocol is
// generic and carries no semantic kind or extension identity, so treating all
// select dialogs as permissions would incorrectly capture ordinary questions.
func isPiPermissionSystemRequest(req extensionUIRequest) bool {
	if req.Method != "select" || !strings.HasPrefix(req.Title, "Permission Required") || len(req.Options) != len(piPermissionSystemOptions) {
		return false
	}
	for i := range req.Options {
		if req.Options[i] != piPermissionSystemOptions[i] {
			return false
		}
	}
	return true
}

func (r *Runtime) handleExtensionUI(ctx context.Context, sessionID string, w *worker, raw json.RawMessage) {
	var req extensionUIRequest
	if json.Unmarshal(raw, &req) != nil || req.ID == "" {
		return
	}
	respond := func(fields map[string]any) {
		fields["type"] = "extension_ui_response"
		fields["id"] = req.ID
		if err := w.c.send(fields); err != nil {
			r.logger.Warn("pi extension UI response failed", "session", sessionID, "err", err)
		}
	}
	if !isPiPermissionSystemRequest(req) || r.hooks == nil {
		// Unsupported extension dialogs must be resolved, otherwise pi waits
		// forever for an RPC client response that usher will never render.
		respond(map[string]any{"cancelled": true})
		return
	}
	input, _ := json.Marshal(map[string]string{"request": req.Title})
	decision, err := r.hooks.Submit(ctx, hook.Event{
		SessionID:   sessionID,
		ToolUseID:   req.ID,
		Event:       "PermissionRequest",
		ToolName:    "pi-permission-system",
		ToolInput:   input,
		Cwd:         w.cwd,
		AllowAlways: true,
	})
	if err != nil {
		respond(map[string]any{"cancelled": true})
		return
	}
	value := "Reject"
	if decision.Behavior == "allow" {
		value = "Allow Once"
		if decision.Scope == "session" {
			value = "Allow Always"
		}
	}
	respond(map[string]any{"value": value})
}

func (r *Runtime) Start(ctx context.Context, req backend.StartRequest) (string, <-chan backend.Event, error) {
	model := req.Model
	if model == "default" {
		model = ""
	}
	c, err := startClient(r.bin, req.Cwd, "", r.sessionsDir, model, r.extra)
	if err != nil {
		return "", nil, err
	}
	data, err := c.request(ctx, "get_state", nil)
	if err != nil {
		c.stop()
		return "", nil, err
	}
	var state struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
	}
	if json.Unmarshal(data, &state) != nil || state.SessionID == "" {
		c.stop()
		return "", nil, errors.New("pi get_state returned no session id")
	}
	r.refreshModels(ctx, c)
	w := &worker{c: c, cwd: req.Cwd, path: state.SessionFile, last: time.Now()}
	w.leases = 1
	if err := r.add(state.SessionID, w); err != nil {
		c.stop()
		return "", nil, err
	}
	defer r.releaseWorker(w)
	ch, err := r.prompt(ctx, state.SessionID, w, req.Prompt, true)
	if err != nil {
		r.mu.Lock()
		if r.workers[state.SessionID] == w {
			delete(r.workers, state.SessionID)
		}
		r.mu.Unlock()
		c.stop()
		return "", nil, err
	}
	return state.SessionID, ch, err
}

func (r *Runtime) Send(ctx context.Context, id, prompt, cwd string) (<-chan backend.Event, error) {
	w := r.leaseWorkerIfLive(id)
	if w == nil {
		path := r.locate(id)
		if path == "" {
			return nil, fmt.Errorf("pi session %s not found", id)
		}
		c, err := startClient(r.bin, cwd, path, r.sessionsDir, "", r.extra)
		if err != nil {
			return nil, err
		}
		r.refreshModels(ctx, c)
		w = &worker{c: c, cwd: cwd, path: path, last: time.Now(), leases: 1}
		if err = r.add(id, w); err != nil {
			c.stop()
			return nil, err
		}
	}
	defer r.releaseWorker(w)
	if command, args, ok := backend.ParseSlashCommand(prompt); ok {
		switch command {
		case "/name":
			if args == "" {
				return nil, errors.New("usage: /name <name>")
			}
			if _, err := w.c.request(ctx, "set_session_name", map[string]any{"name": args}); err != nil {
				return nil, err
			}
			return backend.CompletedOperation(cwd), nil
		case "/compact":
			fields := map[string]any{}
			if args != "" {
				fields["customInstructions"] = args
			}
			return r.rpcOperation(ctx, id, w, "compact", fields)
		}
	}
	return r.prompt(ctx, id, w, prompt, false)
}

func (r *Runtime) rpcOperation(ctx context.Context, id string, w *worker, typ string, fields map[string]any) (<-chan backend.Event, error) {
	r.mu.Lock()
	if w.busy {
		r.mu.Unlock()
		return nil, fmt.Errorf("pi session %s is busy", id)
	}
	w.busy = true
	w.last = time.Now()
	r.mu.Unlock()

	out := make(chan backend.Event, 4)
	go func() {
		defer close(out)
		defer func() {
			r.mu.Lock()
			if r.workers[id] == w {
				w.busy = false
				w.last = time.Now()
			}
			r.mu.Unlock()
		}()

		started, _ := json.Marshal(backend.ProcessStartedPayload{Cwd: w.cwd})
		out <- backend.Event{Type: backend.EventProcessStarted, Raw: started}
		if typ == "compact" {
			raw, _ := json.Marshal(backend.TurnStatusPayload{Status: "compacting"})
			out <- backend.Event{Type: backend.EventTurnStatus, Raw: raw}
		}
		if _, err := w.c.request(ctx, typ, fields); err != nil {
			raw, _ := json.Marshal(backend.ErrorPayload{Message: err.Error()})
			out <- backend.Event{Type: backend.EventError, Raw: raw}
		} else {
			runtimeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			runtime, err := readRuntime(runtimeCtx, w.c)
			cancel()
			if err != nil {
				r.logger.Warn("pi runtime snapshot", "session", id, "err", err)
			}
			emitRuntime(out, runtime)
		}
		// RPC events emitted by the operation precede its response on stdout.
		// They must not leak into the next model turn.
		for {
			select {
			case _, ok := <-w.c.events:
				if !ok {
					out <- backend.Event{Type: backend.EventProcessExit, Raw: json.RawMessage(`{}`)}
					return
				}
			default:
				out <- backend.Event{Type: backend.EventProcessExit, Raw: json.RawMessage(`{}`)}
				return
			}
		}
	}()
	return out, nil
}

// Resume starts an idle RPC worker for an existing session without prompting
// it. Send will reuse the worker on the next turn.
func (r *Runtime) Resume(ctx context.Context, id, cwd string) error {
	r.mu.Lock()
	w := r.workers[id]
	r.mu.Unlock()
	if w != nil {
		return nil
	}
	path := r.locate(id)
	if path == "" {
		return fmt.Errorf("pi session %s not found", id)
	}
	c, err := startClient(r.bin, cwd, path, r.sessionsDir, "", r.extra)
	if err != nil {
		return err
	}
	r.refreshModels(ctx, c)
	w = &worker{c: c, cwd: cwd, path: path, last: time.Now()}
	if err := r.add(id, w); err != nil {
		c.stop()
		return err
	}
	return nil
}

func (r *Runtime) prompt(ctx context.Context, id string, w *worker, text string, fresh bool) (<-chan backend.Event, error) {
	if err := validatePiCommand(ctx, w.c, text); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if w.busy {
		r.mu.Unlock()
		return nil, fmt.Errorf("pi session %s is busy", id)
	}
	w.busy = true
	w.last = time.Now()
	r.mu.Unlock()
	var offset int64
	if info, err := os.Stat(w.path); err == nil {
		offset = info.Size()
	}
	if _, err := w.c.request(ctx, "prompt", map[string]any{"message": text}); err != nil {
		r.mu.Lock()
		w.busy = false
		r.mu.Unlock()
		return nil, err
	}
	out := make(chan backend.Event, 128)
	go func() {
		defer close(out)
		defer func() {
			r.mu.Lock()
			if r.workers[id] == w {
				w.busy = false
				w.last = time.Now()
			}
			r.mu.Unlock()
		}()
		started, _ := json.Marshal(backend.ProcessStartedPayload{Cwd: w.cwd, Fresh: fresh})
		out <- backend.Event{Type: backend.EventProcessStarted, Raw: started}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		// Detach after cancellation to collect final transcript records.
		tailCtx := ctx
		emitTail := func() bool {
			grew, err := tailPiJSONL(tailCtx, w.path, &offset, out)
			if err != nil {
				r.logger.Warn("pi session tail", "session", id, "err", err)
				raw, _ := json.Marshal(backend.ErrorPayload{Message: "pi session tail: " + err.Error()})
				out <- backend.Event{Type: backend.EventError, Raw: raw}
			}
			return grew
		}
		emitExit := func(reason string) {
			payload := map[string]string{}
			if reason != "" {
				payload["reason"] = reason
			}
			raw, _ := json.Marshal(payload)
			out <- backend.Event{Type: backend.EventProcessExit, Raw: raw}
		}
		// Drain through agent_settled so its final records and shared event do
		// not leak into the next turn.
		done := ctx.Done()
		var grace <-chan time.Time
		for {
			select {
			case <-done:
				timer := time.NewTimer(cancelGrace)
				defer timer.Stop()
				done, grace, tailCtx = nil, timer.C, context.WithoutCancel(ctx)
			case <-grace:
				r.logger.Warn("pi cancelled turn finalized without agent_settled", "session", id)
				emitTail()
				emitExit("")
				return
			case <-ticker.C:
				emitTail()
			case raw, ok := <-w.c.events:
				if !ok {
					emitTail()
					errRaw, _ := json.Marshal(backend.ErrorPayload{Message: "pi RPC process exited before the agent settled"})
					out <- backend.Event{Type: backend.EventError, Raw: errRaw}
					emitExit("rpc_exit")
					return
				}
				var e struct {
					Type         string                       `json:"type"`
					Assistant    struct{ Type, Delta string } `json:"assistantMessageEvent"`
					Error        string                       `json:"error"`
					ErrorMessage string                       `json:"errorMessage"`
					FinalError   string                       `json:"finalError"`
					Success      bool                         `json:"success"`
				}
				if json.Unmarshal(raw, &e) != nil {
					continue
				}
				if e.Type == "extension_ui_request" {
					emitTail()
					r.handleExtensionUI(ctx, id, w, raw)
					continue
				}
				switch e.Type {
				case "message_update":
					if e.Assistant.Type == "text_delta" && e.Assistant.Delta != "" {
						b, _ := json.Marshal(backend.PartDeltaPayload{Delta: e.Assistant.Delta})
						out <- backend.Event{Type: backend.EventPartDelta, Raw: b}
					} else if e.Assistant.Type == "thinking_delta" {
						b, _ := json.Marshal(backend.TurnStatusPayload{Status: "thinking"})
						out <- backend.Event{Type: backend.EventTurnStatus, Raw: b}
					}
				case "agent_settled":
					// Read through EOF before finalizing.
					emitTail()
					runtimeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
					runtime, err := readRuntime(runtimeCtx, w.c)
					cancel()
					if err != nil {
						r.logger.Warn("pi runtime snapshot", "session", id, "err", err)
					}
					emitRuntime(out, runtime)
					emitExit("")
					return
				case "extension_error":
					msg := e.Error
					if msg == "" {
						msg = "pi extension error"
					}
					b, _ := json.Marshal(backend.ErrorPayload{Message: msg})
					out <- backend.Event{Type: backend.EventError, Raw: b}
				case "auto_retry_start":
					b, _ := json.Marshal(backend.TurnStatusPayload{Status: "retrying"})
					out <- backend.Event{Type: backend.EventTurnStatus, Raw: b}
				case "compaction_start":
					b, _ := json.Marshal(backend.TurnStatusPayload{Status: "compacting"})
					out <- backend.Event{Type: backend.EventTurnStatus, Raw: b}
				case "auto_retry_end":
					if !e.Success && e.FinalError != "" {
						b, _ := json.Marshal(backend.ErrorPayload{Message: e.FinalError})
						out <- backend.Event{Type: backend.EventError, Raw: b}
					}
				case "compaction_end":
					if e.ErrorMessage != "" {
						b, _ := json.Marshal(backend.ErrorPayload{Message: e.ErrorMessage})
						out <- backend.Event{Type: backend.EventError, Raw: b}
					}
				}
			}
		}
	}()
	return out, nil
}

func validatePiCommand(ctx context.Context, c *client, text string) error {
	command, _, ok := backend.ParseSlashCommand(text)
	if !ok {
		return nil
	}
	data, err := c.request(ctx, "get_commands", nil)
	if err != nil {
		return fmt.Errorf("resolve command %s: %w", command, err)
	}
	items, err := composerItemsFromRPC(data)
	if err != nil {
		return fmt.Errorf("resolve command %s: %w", command, err)
	}
	name := strings.TrimPrefix(command, "/")
	for _, item := range items {
		if item.Name == name {
			return nil
		}
	}
	return fmt.Errorf("unknown command: %s", command)
}

func composerItemsFromRPC(data json.RawMessage) ([]backend.ComposerItem, error) {
	var payload struct {
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	out := []backend.ComposerItem{
		{Name: "name", Kind: "command", Description: "Set session display name"},
		{Name: "compact", Kind: "command", Description: "Manually compact the session context"},
	}
	for _, command := range payload.Commands {
		if command.Name == "" || command.Name == "name" || command.Name == "compact" {
			continue
		}
		kind := command.Source
		if kind != "extension" && kind != "prompt" && kind != "skill" {
			kind = "command"
		}
		out = append(out, backend.ComposerItem{
			Name:        command.Name,
			Kind:        kind,
			Description: command.Description,
		})
	}
	return out, nil
}

// ComposerItems returns pi's extension commands, prompt templates, and skills
// without starting a cold worker merely to populate speculative UI. Pi already
// returns skill names in their invocable form (skill:name), so no rewrite is
// needed before the frontend adds the leading slash.
func (r *Runtime) ComposerItems(ctx context.Context, id, _ string) (backend.ComposerCatalog, error) {
	w := r.leaseWorkerIfLive(id)
	if w == nil {
		return backend.ComposerCatalog{}, nil
	}
	defer r.releaseWorker(w)
	data, err := w.c.request(ctx, "get_commands", nil)
	if err != nil {
		return backend.ComposerCatalog{}, err
	}
	items, err := composerItemsFromRPC(data)
	return backend.ComposerCatalog{Items: items, Available: err == nil}, err
}

// leaseWorkerIfLive pins an already-live worker against LRU eviction while a
// short RPC or send preflight runs, and reports nil rather than starting one:
// composer completion is speculative and must never pay for a cold start. Send
// covers the nil case by building its own worker. A successful send becomes
// eviction-safe through busy before releasing this lease.
func (r *Runtime) leaseWorkerIfLive(id string) *worker {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.workers[id]
	if w != nil {
		w.leases++
		w.last = time.Now()
	}
	return w
}

func (r *Runtime) releaseWorker(w *worker) {
	r.mu.Lock()
	w.leases--
	// Refresh on release too: a worker that just finished a long lease is the
	// most recently used one, not the stalest LRU eviction candidate.
	w.last = time.Now()
	r.mu.Unlock()
}

// tailPiJSONL emits complete records appended since offset. A partial trailing
// record is left for the next poll, although pi normally writes each JSONL
// entry atomically with its newline.
func tailPiJSONL(ctx context.Context, path string, offset *int64, out chan<- backend.Event) (bool, error) {
	if path == "" {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if *offset > info.Size() {
		*offset = 0
	}
	if _, err := f.Seek(*offset, io.SeekStart); err != nil {
		return false, err
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	lastNewline := bytes.LastIndexByte(chunk, '\n')
	if lastNewline < 0 {
		return false, nil
	}
	complete := chunk[:lastNewline+1]
	grew := len(complete) > 0
	for _, line := range bytes.Split(complete, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &head) != nil || head.Type == "" {
			continue
		}
		raw := append(json.RawMessage(nil), line...)
		select {
		case out <- backend.Event{Type: head.Type, Raw: raw}:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	*offset += int64(len(complete))
	return grew, nil
}

func (r *Runtime) add(id string, w *worker) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.workers[id]; old != nil {
		return fmt.Errorf("pi session %s already live", id)
	}
	if len(r.workers) >= r.max {
		var victimID string
		var victim *worker
		for k, x := range r.workers {
			if x.busy || x.leases > 0 {
				continue
			}
			if victim == nil || x.last.Before(victim.last) {
				victimID, victim = k, x
			}
		}
		if victim == nil {
			return fmt.Errorf("maximum live pi sessions (%d) are all busy", r.max)
		}
		delete(r.workers, victimID)
		go victim.c.stop()
	}
	r.workers[id] = w
	return nil
}
func (r *Runtime) locate(id string) string {
	var found string
	_ = filepath.Walk(r.sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(path) == ".jsonl" && SessionIDFromPath(path) == id {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
func (r *Runtime) Has(id string) bool { r.mu.Lock(); defer r.mu.Unlock(); return r.workers[id] != nil }
func (r *Runtime) LiveSessions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.workers))
	for id := range r.workers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
func (r *Runtime) Interrupt(id string) error {
	r.mu.Lock()
	w := r.workers[id]
	r.mu.Unlock()
	if w == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := w.c.request(ctx, "abort", nil)
	return err
}
func (r *Runtime) Kill(id string) error {
	r.mu.Lock()
	w := r.workers[id]
	delete(r.workers, id)
	r.mu.Unlock()
	if w != nil {
		w.c.stop()
	}
	return nil
}
func (r *Runtime) Shutdown() {
	r.mu.Lock()
	ws := r.workers
	r.workers = map[string]*worker{}
	r.mu.Unlock()
	for _, w := range ws {
		w.c.stop()
	}
}
