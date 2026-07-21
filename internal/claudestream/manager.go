// Package claudestream manages long-running Claude Code stream-json children.
package claudestream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

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
	done   chan Result
	deltas chan Delta
	model  string
}

// finish closes deltas before done, so a receiver of done may safely abandon
// deltas. Unread tail deltas are superseded by the canonical transcript.
func (r *turnRequest) finish(res Result) {
	close(r.deltas)
	r.done <- res
	close(r.done)
}

type process struct {
	id       string
	cmd      *exec.Cmd
	in       io.WriteCloser
	cwd      string
	mu       sync.Mutex
	turns    []*turnRequest // nil entry represents a spontaneous turn
	controls map[string]context.CancelFunc
	lastUsed time.Time
	stopping bool
	done     chan struct{}
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
	m.mu.Lock()
	if p := m.processes[id]; p != nil {
		p.lastUsed = time.Now()
		m.mu.Unlock()
		return p, false, nil
	}
	if len(m.processes) >= m.maxLive {
		var victim *process
		for _, p := range m.processes {
			p.mu.Lock()
			busy := len(p.turns) > 0
			p.mu.Unlock()
			if !busy && (victim == nil || p.lastUsed.Before(victim.lastUsed)) {
				victim = p
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
	p := &process{id: id, cmd: cmd, in: in, cwd: cwd, controls: map[string]context.CancelFunc{}, lastUsed: time.Now(), done: make(chan struct{})}
	m.processes[id] = p
	m.mu.Unlock()
	go m.readLoop(p, out)
	go func() { err := cmd.Wait(); m.died(p, err) }()
	return p, true, nil
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
	p, fresh, err := m.ensure(ctx, id, cwd, model, resume)
	if err != nil {
		return nil, nil, false, 0, err
	}
	req := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 256)}
	p.mu.Lock()
	queuedAhead := len(p.turns)
	p.turns = append(p.turns, req)
	p.lastUsed = time.Now()
	p.mu.Unlock()
	msg := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": prompt}}}}
	if err := write(p, msg); err != nil {
		p.mu.Lock()
		p.turns = p.turns[:len(p.turns)-1]
		p.mu.Unlock()
		return nil, nil, fresh, 0, err
	}
	return req.done, req.deltas, fresh, queuedAhead, nil
}

// Resume starts an idle process for an existing session without submitting a
// user turn. It is idempotent when the process is already live.
func (m *Manager) Resume(ctx context.Context, id, cwd string) error {
	_, _, err := m.ensure(ctx, id, cwd, "", true)
	return err
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
			IsError bool   `json:"is_error"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
			ModelUsage map[string]struct {
				ContextWindow int64 `json:"contextWindow"`
			} `json:"modelUsage"`
			Event struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			} `json:"event"`
		}
		if json.Unmarshal(s.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "control_request" {
			m.handleControlRequest(p, append([]byte(nil), s.Bytes()...))
			continue
		}
		if e.Type == "control_cancel_request" {
			m.cancelControlRequest(p, s.Bytes())
			continue
		}
		if e.Type != "result" {
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
			continue
		}
		p.mu.Lock()
		var req *turnRequest
		if len(p.turns) > 0 {
			req = p.turns[0]
			p.turns = p.turns[1:]
		}
		p.lastUsed = time.Now()
		p.mu.Unlock()
		if req != nil {
			model := req.model
			usage, ok := e.ModelUsage[model]
			if !ok && len(e.ModelUsage) == 1 {
				for fallbackModel, fallbackUsage := range e.ModelUsage {
					model, usage = fallbackModel, fallbackUsage
				}
			}
			req.finish(Result{IsError: e.IsError, Subtype: e.Subtype, Model: model, ContextWindow: usage.ContextWindow})
		}
	}
	if err := s.Err(); err != nil {
		m.logger.Warn("claude stream-json read failed", "session", p.id, "err", err)
		if p.cmd.Process != nil {
			_ = procutil.KillGroup(p.cmd)
		}
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
