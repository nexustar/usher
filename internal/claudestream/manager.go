// Package claudestream manages long-running Claude Code stream-json children.
package claudestream

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/procutil"
)

type Result struct {
	IsError       bool
	Subtype       string
	Model         string
	ContextWindow int64
}

// Delta is ephemeral protocol output used for live preview. Session JSONL
// remains the canonical transcript.
type Delta struct{ Text string }

type turnRequest struct {
	done    chan Result
	deltas  chan Delta
	model   string
	uuid    string
	started bool
	runtime *Result // metadata from result; lifecycle still owns completion
}

// finish closes deltas before done, so a receiver of done may safely abandon
// deltas. Unread tail deltas are superseded by the canonical transcript.
func (r *turnRequest) finish(res Result) {
	close(r.deltas)
	r.done <- res
	close(r.done)
}

// process is both the stream client and the manager's bookkeeping record, so
// mu — not Manager.mu — guards every mutable field below it, including the
// leases and lastUsed the eviction scan consults. Manager.mu only guards the
// process map. Read those fields inside mu even while holding Manager.mu.
type process struct {
	id            string
	cmd           *exec.Cmd
	in            io.WriteCloser
	cwd           string
	mu            sync.Mutex
	turns         []*turnRequest // nil entry represents a spontaneous turn
	controls      map[string]context.CancelFunc
	controlWait   map[string]chan error
	commands      []Command
	commandsReady bool
	initDone      chan struct{}
	initErr       error
	leases        int
	lastUsed      time.Time
	stopping      bool
	done          chan struct{}
}

// Command is one slash command reported by Claude Code's system/init event.
type Command struct {
	Name string
	Kind string
}

type Manager struct {
	bin       string
	settings  string
	mcpArgs   []string
	hookSock  string
	maxLive   int
	logger    *slog.Logger
	hooks     *hook.Manager
	mu        sync.Mutex
	processes map[string]*process
}

func New(bin, settings, hookSock string, mcpArgs []string, maxLive int, hooks *hook.Manager, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if maxLive <= 0 {
		maxLive = 8
	}
	return &Manager{bin: bin, settings: settings, hookSock: hookSock, mcpArgs: append([]string(nil), mcpArgs...), maxLive: maxLive, hooks: hooks, logger: logger, processes: map[string]*process{}}
}

func (m *Manager) ensure(ctx context.Context, id, cwd, model string, resume bool) (*process, bool, error) {
	return m.ensureProcess(ctx, id, cwd, model, resume, false)
}

// leaseProcess is ensure plus a lease pinning the process against eviction
// while a send preflight runs. It spawns a cold process like ensure does; the
// lease is taken under the same lock that resolves it, so there is no window
// where a caller holds a process that another spawn may already have evicted.
// A successful send becomes eviction-safe through its queued turn before the
// lease is released.
func (m *Manager) leaseProcess(ctx context.Context, id, cwd, model string, resume bool) (*process, bool, error) {
	return m.ensureProcess(ctx, id, cwd, model, resume, true)
}

func (m *Manager) ensureProcess(ctx context.Context, id, cwd, model string, resume, lease bool) (*process, bool, error) {
	m.mu.Lock()
	if p := m.processes[id]; p != nil {
		p.mu.Lock()
		if lease {
			p.leases++
		}
		p.lastUsed = time.Now()
		p.mu.Unlock()
		m.mu.Unlock()
		if err := waitForInitialization(ctx, p); err != nil {
			if lease {
				releaseProcess(p)
			}
			return nil, false, err
		}
		return p, false, nil
	}
	if len(m.processes) >= m.maxLive {
		var victim *process
		var victimLastUsed time.Time
		for _, p := range m.processes {
			// Sample both fields in one critical section: m.mu does not cover
			// process state, and readLoop writes lastUsed on every result line.
			p.mu.Lock()
			busy := len(p.turns) > 0 || p.leases > 0
			lastUsed := p.lastUsed
			p.mu.Unlock()
			if !busy && (victim == nil || lastUsed.Before(victimLastUsed)) {
				victim, victimLastUsed = p, lastUsed
			}
		}
		if victim != nil {
			delete(m.processes, victim.id)
			go stop(victim)
		} else {
			m.mu.Unlock()
			return nil, false, fmt.Errorf("maximum live Claude sessions (%d) are all busy", m.maxLive)
		}
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--include-partial-messages", "--verbose"}
	if m.hooks != nil {
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	if resume {
		args = append(args, "--resume", id)
	} else {
		args = append(args, "--session-id", id)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if m.settings != "" {
		args = append(args, "--settings", m.settings)
	}
	args = append(args, m.mcpArgs...)
	cmd := exec.CommandContext(context.Background(), m.bin, args...)
	procutil.ConfigureGroup(cmd)
	cmd.Dir = cwd
	cmd.Env = scrubEnv(m.hookSock)
	in, err := cmd.StdinPipe()
	if err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	p := &process{
		id: id, cmd: cmd, in: in, cwd: cwd,
		controls: map[string]context.CancelFunc{}, controlWait: map[string]chan error{},
		initDone: make(chan struct{}), lastUsed: time.Now(), done: make(chan struct{}),
	}
	if lease {
		p.leases = 1
	}
	m.processes[id] = p
	m.mu.Unlock()
	go m.readLoop(p, out)
	go func() { err := cmd.Wait(); m.died(p, err) }()
	err = m.initializeProcess(ctx, p)
	p.mu.Lock()
	p.initErr = err
	close(p.initDone)
	p.mu.Unlock()
	if err != nil {
		m.mu.Lock()
		if m.processes[id] == p {
			delete(m.processes, id)
		}
		m.mu.Unlock()
		stop(p)
		return nil, false, err
	}
	return p, true, nil
}

func waitForInitialization(ctx context.Context, p *process) error {
	// Injected process records are already initialized.
	if p.initDone == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("claude process exited during initialization")
	case <-p.initDone:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.initErr
	}
}

func (m *Manager) initializeProcess(ctx context.Context, p *process) error {
	requestID := fmt.Sprintf("usher-init-%d", time.Now().UnixNano())
	result := make(chan error, 1)
	p.mu.Lock()
	p.controlWait[requestID] = result
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.controlWait, requestID)
		p.mu.Unlock()
	}()
	if err := write(p, map[string]any{
		"type": "control_request", "request_id": requestID,
		"request": map[string]any{"subtype": "initialize", "hooks": nil},
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("claude process exited during initialization")
	case err := <-result:
		if err != nil {
			return err
		}
	}
	_, err := waitForCommands(ctx, p)
	return err
}

func scrubEnv(hookSock string) []string {
	out := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		name := e
		for i, c := range e {
			if c == '=' {
				name = e[:i]
				break
			}
		}
		if len(name) >= 6 && name[:6] == "CLAUDE" {
			continue
		}
		out = append(out, e)
	}
	if hookSock != "" {
		out = append(out, "USHER_HOOK_SOCK="+hookSock)
	}
	return out
}

func (m *Manager) Send(ctx context.Context, id, prompt, cwd, model string, resume bool) (<-chan Result, <-chan Delta, bool, int, error) {
	p, fresh, err := m.leaseProcess(ctx, id, cwd, model, resume)
	if err != nil {
		return nil, nil, false, 0, err
	}
	defer releaseProcess(p)
	if command, _, ok := backend.ParseSlashCommand(prompt); ok {
		commands, err := waitForCommands(ctx, p)
		if err != nil {
			return nil, nil, fresh, 0, err
		}
		if !hasCommand(commands, strings.TrimPrefix(command, "/")) {
			return nil, nil, fresh, 0, fmt.Errorf("unknown command: %s", command)
		}
	}
	req := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 256), uuid: messageUUID()}
	p.mu.Lock()
	queuedAhead := len(p.turns)
	p.turns = append(p.turns, req)
	p.lastUsed = time.Now()
	p.mu.Unlock()
	msg := map[string]any{
		"type": "user", "uuid": req.uuid,
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": prompt}}},
	}
	if err := write(p, msg); err != nil {
		p.mu.Lock()
		p.turns = p.turns[:len(p.turns)-1]
		p.mu.Unlock()
		return nil, nil, fresh, 0, err
	}
	return req.done, req.deltas, fresh, queuedAhead, nil
}

func messageUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
			b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return fmt.Sprintf("usher-%d", time.Now().UnixNano())
}

func releaseProcess(p *process) {
	p.mu.Lock()
	p.leases--
	// A send that failed its preflight never reached the turn queue, so this is
	// the only thing marking the process as just-used before it becomes an
	// eviction candidate again — the user is likely retyping the command.
	p.lastUsed = time.Now()
	p.mu.Unlock()
}

func hasCommand(commands []Command, name string) bool {
	for _, command := range commands {
		if command.Name == name {
			return true
		}
	}
	return false
}

func waitForCommands(ctx context.Context, p *process) ([]Command, error) {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		commands := append([]Command(nil), p.commands...)
		ready := p.commandsReady
		p.mu.Unlock()
		if ready {
			return commands, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.done:
			return nil, errors.New("claude process exited before advertising commands")
		case <-deadline.C:
			return nil, errors.New("timed out waiting for Claude command list")
		case <-ticker.C:
		}
	}
}

// Resume starts an idle process for an existing session without submitting a
// user turn. It is idempotent when the process is already live.
func (m *Manager) Resume(ctx context.Context, id, cwd string) error {
	_, _, err := m.ensure(ctx, id, cwd, "", true)
	return err
}

// Commands returns the command catalog most recently advertised by a live
// Claude process. Claude sends it in system/init; a cold process has none yet.
func (m *Manager) Commands(id string) []Command {
	commands, _ := m.CommandsIfLive(id)
	return commands
}

// CommandsIfLive also reports whether system/init has supplied the complete
// catalog. A process can exist briefly before that event arrives.
func (m *Manager) CommandsIfLive(id string) ([]Command, bool) {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Command(nil), p.commands...), p.commandsReady
}

func write(p *process, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping {
		return errors.New("claude process is stopping")
	}
	_, err = p.in.Write(b)
	return err
}
func (m *Manager) readLoop(p *process, r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), 64<<20)
	for s.Scan() {
		var e struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
			IsError    bool `json:"is_error"`
			ModelUsage map[string]struct {
				ContextWindow int64 `json:"contextWindow"`
			} `json:"modelUsage"`
			SlashCommands []string `json:"slash_commands"`
			Skills        []string `json:"skills"`
			Event         struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			} `json:"event"`
			CommandUUID string `json:"command_uuid"`
			State       string `json:"state"`
		}
		if json.Unmarshal(s.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "control_response" {
			m.finishControlRequest(p, s.Bytes())
			continue
		}
		if e.Type == "system" && e.Subtype == "init" {
			skills := make(map[string]struct{}, len(e.Skills))
			for _, name := range e.Skills {
				skills[name] = struct{}{}
			}
			commands := make([]Command, 0, len(e.SlashCommands))
			seen := make(map[string]struct{}, len(e.SlashCommands))
			for _, name := range e.SlashCommands {
				name = strings.TrimPrefix(strings.TrimSpace(name), "/")
				if name == "" {
					continue
				}
				if _, ok := seen[name]; ok {
					continue
				}
				seen[name] = struct{}{}
				// Claude only identifies skills explicitly. slash_commands does
				// not distinguish built-ins from plugin or custom commands, so do
				// not claim a more specific origin for the remaining entries.
				kind := "command"
				if _, ok := skills[name]; ok {
					kind = "skill"
				}
				commands = append(commands, Command{Name: name, Kind: kind})
			}
			p.mu.Lock()
			p.commands = commands
			p.commandsReady = true
			p.mu.Unlock()
		}
		if e.Type == "control_request" {
			m.handleControlRequest(p, append([]byte(nil), s.Bytes()...))
			continue
		}
		if e.Type == "control_cancel_request" {
			m.cancelControlRequest(p, s.Bytes())
			continue
		}
		if e.Type == "command_lifecycle" {
			m.finishLifecycle(p, e.CommandUUID, e.State)
			continue
		}
		if e.Type == "result" {
			// Result carries metadata; lifecycle owns completion.
			p.mu.Lock()
			if len(p.turns) > 0 && p.turns[0] != nil && p.turns[0].started {
				req := p.turns[0]
				model := req.model
				usage, ok := e.ModelUsage[model]
				if !ok && len(e.ModelUsage) == 1 {
					for fallbackModel, fallbackUsage := range e.ModelUsage {
						model, usage = fallbackModel, fallbackUsage
					}
				}
				req.runtime = &Result{
					IsError: e.IsError, Subtype: e.Subtype,
					Model: model, ContextWindow: usage.ContextWindow,
				}
			}
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		if len(p.turns) == 0 && marksSpontaneousTurn(e.Type, e.Subtype, e.Event.Type) {
			p.turns = append(p.turns, nil)
		}
		if len(p.turns) > 0 && p.turns[0] != nil && e.Type == "stream_event" &&
			e.Event.Type == "content_block_delta" && e.Event.Delta.Type == "text_delta" && e.Event.Delta.Text != "" {
			select {
			case p.turns[0].deltas <- Delta{Text: e.Event.Delta.Text}:
			default: // preview may drop under backpressure; JSONL truth-up repairs it
			}
		}
		if len(p.turns) > 0 && p.turns[0] != nil && e.Message.Model != "" {
			p.turns[0].model = e.Message.Model
		}
		p.mu.Unlock()
	}
	if err := s.Err(); err != nil {
		m.logger.Warn("claude stream-json read failed", "session", p.id, "err", err)
		if p.cmd.Process != nil {
			_ = procutil.KillGroup(p.cmd)
		}
	}
}

func (m *Manager) finishControlRequest(p *process, raw []byte) {
	var msg struct {
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
			Response  struct {
				Commands []struct {
					Name string `json:"name"`
				} `json:"commands"`
			} `json:"response"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Response.RequestID == "" {
		return
	}
	p.mu.Lock()
	wait := p.controlWait[msg.Response.RequestID]
	delete(p.controlWait, msg.Response.RequestID)
	if msg.Response.Subtype == "success" {
		commands := make([]Command, 0, len(msg.Response.Response.Commands))
		seen := make(map[string]struct{}, len(msg.Response.Response.Commands))
		for _, command := range msg.Response.Response.Commands {
			name := strings.TrimPrefix(strings.TrimSpace(command.Name), "/")
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			commands = append(commands, Command{Name: name, Kind: "command"})
		}
		p.commands = commands
		p.commandsReady = true
	}
	p.mu.Unlock()
	if wait == nil {
		return
	}
	var err error
	if msg.Response.Subtype == "error" {
		err = errors.New(msg.Response.Error)
	}
	wait <- err
}

func (m *Manager) finishLifecycle(p *process, uuid, state string) {
	if uuid == "" {
		return
	}
	p.mu.Lock()
	if state == "started" {
		for _, candidate := range p.turns {
			if candidate != nil && candidate.uuid == uuid {
				candidate.started = true
				break
			}
		}
		p.lastUsed = time.Now()
		p.mu.Unlock()
		return
	}
	if state != "completed" && state != "cancelled" {
		p.mu.Unlock()
		return
	}
	var req *turnRequest
	for i, candidate := range p.turns {
		if candidate != nil && candidate.uuid == uuid {
			req = candidate
			p.turns = append(p.turns[:i], p.turns[i+1:]...)
			break
		}
	}
	if req == nil && len(p.turns) > 0 && p.turns[0] == nil {
		// Close the placeholder for an externally submitted turn.
		p.turns = p.turns[1:]
	}
	p.lastUsed = time.Now()
	p.mu.Unlock()
	if req != nil {
		result := Result{Model: req.model}
		if req.runtime != nil {
			result = *req.runtime
		}
		result.Subtype = state
		if state == "cancelled" {
			result.IsError = true
		}
		req.finish(result)
	}
}

// handleControlRequest implements the permission callback protocol used by
// the Claude Agent SDK. Permission prompts in -p mode do not enter the normal
// terminal dialog (and therefore do not reliably fire PermissionRequest
// command hooks); --permission-prompt-tool stdio instead sends can_use_tool
// requests over the stream-json transport.
func (m *Manager) handleControlRequest(p *process, raw []byte) {
	var msg struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype               string            `json:"subtype"`
			ToolName              string            `json:"tool_name"`
			Input                 json.RawMessage   `json:"input"`
			ToolUseID             string            `json:"tool_use_id"`
			PermissionSuggestions []json.RawMessage `json:"permission_suggestions"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.RequestID == "" || msg.Request.Subtype != "can_use_tool" {
		return
	}
	if m.hooks == nil {
		m.writeControlError(p, msg.RequestID, "permission handler unavailable")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	if p.controls == nil {
		p.controls = map[string]context.CancelFunc{}
	}
	p.controls[msg.RequestID] = cancel
	p.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			p.mu.Lock()
			delete(p.controls, msg.RequestID)
			p.mu.Unlock()
		}()
		go func() {
			select {
			case <-p.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		resp, err := m.hooks.Submit(ctx, hook.Event{
			SessionID:   p.id,
			ToolUseID:   msg.Request.ToolUseID,
			Event:       "PermissionRequest",
			ToolName:    msg.Request.ToolName,
			ToolInput:   msg.Request.Input,
			Cwd:         p.cwd,
			AllowAlways: hasAllowSuggestion(msg.Request.PermissionSuggestions),
		})
		if err != nil {
			m.writeControlError(p, msg.RequestID, err.Error())
			return
		}
		decision := map[string]any{"behavior": resp.Behavior}
		if resp.Behavior == "allow" {
			// The SDK always echoes the original input for an allow decision.
			decision["updatedInput"] = json.RawMessage(msg.Request.Input)
			if resp.Scope == "session" {
				if suggestions := allowSuggestions(msg.Request.PermissionSuggestions); len(suggestions) > 0 {
					decision["updatedPermissions"] = suggestions
				}
			}
		} else if resp.Reason != "" {
			decision["message"] = resp.Reason
		}
		_ = write(p, map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype": "success", "request_id": msg.RequestID, "response": decision,
			},
		})
	}()
}

func (m *Manager) cancelControlRequest(p *process, raw []byte) {
	var msg struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.RequestID == "" {
		return
	}
	p.mu.Lock()
	cancel := p.controls[msg.RequestID]
	delete(p.controls, msg.RequestID)
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) writeControlError(p *process, requestID, message string) {
	_ = write(p, map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype": "error", "request_id": requestID, "error": message,
		},
	})
}

func hasAllowSuggestion(suggestions []json.RawMessage) bool {
	return len(allowSuggestions(suggestions)) > 0
}

func allowSuggestions(suggestions []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for _, raw := range suggestions {
		var suggestion struct {
			Behavior string `json:"behavior"`
		}
		if json.Unmarshal(raw, &suggestion) == nil && suggestion.Behavior == "allow" {
			out = append(out, raw)
		}
	}
	return out
}

func marksSpontaneousTurn(typ, subtype, eventType string) bool {
	if typ == "control_response" || typ == "rate_limit_event" || typ == "command_lifecycle" {
		return false
	}
	if typ == "system" {
		return subtype == "task_started" || subtype == "turn_started"
	}
	if typ == "stream_event" {
		// Under --include-partial-messages a spontaneous turn's first output
		// is a stream_event, so mark on message_start (deltas alone must not
		// create phantom turns). This only restores the pre-partial-messages
		// window: a Send landing before the first output line still races.
		return eventType == "message_start"
	}
	return typ == "assistant" || typ == "user"
}
func (m *Manager) died(p *process, err error) {
	close(p.done)
	m.mu.Lock()
	if m.processes[p.id] == p {
		delete(m.processes, p.id)
	}
	m.mu.Unlock()
	p.mu.Lock()
	turns := p.turns
	p.turns = nil
	controls := p.controls
	p.controls = nil
	wasStopping := p.stopping
	p.stopping = true
	p.mu.Unlock()
	for _, cancel := range controls {
		cancel()
	}
	for _, req := range turns {
		if req != nil {
			req.finish(Result{IsError: true, Subtype: "process_exited"})
		}
	}
	if err != nil && !wasStopping {
		m.logger.Warn("claude process exited", "session", p.id, "err", err)
	}
}
func (m *Manager) Interrupt(id string) error {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := map[string]any{"type": "control_request", "request_id": fmt.Sprintf("usher-%d", time.Now().UnixNano()), "request": map[string]any{"subtype": "interrupt"}}
	done := make(chan error, 1)
	go func() { done <- write(p, req) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}
func (m *Manager) Kill(id string) error {
	m.mu.Lock()
	p := m.processes[id]
	delete(m.processes, id)
	m.mu.Unlock()
	if p != nil {
		stop(p)
	}
	return nil
}
func stop(p *process) {
	p.mu.Lock()
	p.stopping = true
	in := p.in
	cmd := p.cmd
	p.mu.Unlock()
	_ = in.Close()
	select {
	case <-p.done:
		return
	case <-time.After(2 * time.Second):
	}
	_ = procutil.KillGroup(cmd)
	select {
	case <-p.done:
	case <-time.After(time.Second):
	}
}
func (m *Manager) Has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processes[id] != nil
}
func (m *Manager) LiveSessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.processes))
	for id := range m.processes {
		out = append(out, id)
	}
	return out
}
func (m *Manager) Shutdown() {
	m.mu.Lock()
	ps := make([]*process, 0, len(m.processes))
	for _, p := range m.processes {
		ps = append(ps, p)
	}
	m.processes = map[string]*process{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Add(1)
		go func() { defer wg.Done(); stop(p) }()
	}
	wg.Wait()
}
