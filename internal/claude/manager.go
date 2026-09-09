// Package claude manages long-running Claude Code stream-json children.
package claude

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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/procutil"
)

type Result struct {
	IsError       bool
	Subtype       string
	Error         string
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
	foreign bool    // Claude's own command (cron, /loop); nothing waits on it
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
	logger        *slog.Logger
	mu            sync.Mutex
	turns         []*turnRequest // FIFO in Claude's drain order; head is the turn producing output
	controls      map[string]context.CancelFunc
	controlWait   map[string]chan controlResult
	commands      []Command
	commandsReady bool
	initDone      chan struct{}
	initErr       error
	leases        int
	lastUsed      time.Time
	stopping      bool
	done          chan struct{}
	tasks         map[string]struct{} // delegated agent tasks in flight, by task_id
}

// busy is the eviction guard. With mu.
func (p *process) busy() bool { return len(p.turns) > 0 || p.leases > 0 || len(p.tasks) > 0 }

// With mu.
func (p *process) describeBusy() string {
	foreign := 0
	for _, req := range p.turns {
		if req.foreign {
			foreign++
		}
	}
	return fmt.Sprintf("%s turns=%d foreign=%d leases=%d tasks=%d idle=%s",
		p.id, len(p.turns), foreign, p.leases, len(p.tasks), time.Since(p.lastUsed).Round(time.Second))
}

// Mirrors the Agent SDK's task tracking. Shells are left out: they may never
// reach a terminal status. Either terminal frame can be the only one sent.
var (
	deferringTaskTypes   = map[string]bool{"local_agent": true, "local_workflow": true}
	terminalTaskStatuses = map[string]bool{"completed": true, "failed": true, "stopped": true, "killed": true}
)

func trackTask(p *process, subtype, taskID, taskType, patchStatus string) {
	if taskID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch subtype {
	case "task_started":
		if deferringTaskTypes[taskType] {
			if p.tasks == nil {
				p.tasks = map[string]struct{}{}
			}
			p.tasks[taskID] = struct{}{}
		}
	case "task_notification":
		delete(p.tasks, taskID)
	case "task_updated":
		if terminalTaskStatuses[patchStatus] {
			delete(p.tasks, taskID)
		}
	}
}

// interruptGrace is how long a turn stays queued after an unanswered interrupt.
var interruptGrace = 30 * time.Second

// controlResult carries a control response back to its waiting request. Most
// subtypes answer with an empty payload.
type controlResult struct {
	payload json.RawMessage
	err     error
}

// Command is one slash command reported by Claude Code's system/init event.
type Command struct {
	Name string
	Kind string
}

type Manager struct {
	bin          string
	settings     string
	mcpArgs      []string
	hookSock     string
	maxLive      int
	logger       *slog.Logger
	interactions *interaction.Manager
	mu           sync.Mutex
	processes    map[string]*process
}

func New(bin, settings, hookSock string, mcpArgs []string, maxLive int, interactions *interaction.Manager, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if maxLive <= 0 {
		maxLive = 8
	}
	return &Manager{bin: bin, settings: settings, hookSock: hookSock, mcpArgs: append([]string(nil), mcpArgs...), maxLive: maxLive, interactions: interactions, logger: logger, processes: map[string]*process{}}
}

// ensureProcess resolves id's live process, spawning a cold one when needed.
// appendSystemPrompt and extraArgs only reach a spawn — a live process keeps
// what it started with. With lease, the process is pinned against eviction while a
// send preflight runs: the lease is taken under the same lock that resolves
// the process, so there is no window where a caller holds a process that
// another spawn may already have evicted. A successful send becomes
// eviction-safe through its queued turn before the lease is released.
func (m *Manager) ensureProcess(ctx context.Context, id, cwd, model, appendSystemPrompt string, extraArgs []string, resume, lease bool) (*process, bool, error) {
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
		var busyWorkers []string
		for _, p := range m.processes {
			// Sample both fields in one critical section: m.mu does not cover
			// process state, and readLoop writes lastUsed on every result line.
			p.mu.Lock()
			busy := p.busy()
			lastUsed := p.lastUsed
			if busy {
				busyWorkers = append(busyWorkers, p.describeBusy())
			}
			p.mu.Unlock()
			if !busy && (victim == nil || lastUsed.Before(victimLastUsed)) {
				victim, victimLastUsed = p, lastUsed
			}
		}
		if victim != nil {
			delete(m.processes, victim.id)
			go stop(victim)
			m.logger.Info("worker evicted", "session", victim.id, "for", id)
		} else {
			m.mu.Unlock()
			m.logger.Warn("no evictable worker", "for", id, "busy", busyWorkers)
			return nil, false, fmt.Errorf("maximum live Claude sessions (%d) are all busy", m.maxLive)
		}
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--include-partial-messages", "--verbose"}
	if m.interactions != nil {
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
	if appendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", appendSystemPrompt)
	}
	if m.settings != "" {
		args = append(args, "--settings", m.settings)
	}
	args = append(args, m.mcpArgs...)
	// Last, so a repeated flag from the session's agent profile wins.
	args = append(args, extraArgs...)
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
		id: id, cmd: cmd, in: in, cwd: cwd, logger: m.logger.With("session", id),
		controls: map[string]context.CancelFunc{}, controlWait: map[string]chan controlResult{},
		initDone: make(chan struct{}), lastUsed: time.Now(), done: make(chan struct{}),
	}
	if lease {
		p.leases = 1
	}
	m.processes[id] = p
	m.mu.Unlock()
	p.logger.Info("spawn", "args", backend.RedactSpawnArgs(cmd.Args))
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
	if _, err := m.controlRequest(ctx, p, map[string]any{"subtype": "initialize", "hooks": nil}); err != nil {
		return err
	}
	_, err := waitForCommands(ctx, p)
	return err
}

func (m *Manager) controlRequest(ctx context.Context, p *process, request map[string]any) (json.RawMessage, error) {
	requestID := fmt.Sprintf("usher-ctl-%d", time.Now().UnixNano())
	result := make(chan controlResult, 1)
	p.mu.Lock()
	p.controlWait[requestID] = result
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.controlWait, requestID)
		p.mu.Unlock()
	}()
	if err := write(p, map[string]any{
		"type": "control_request", "request_id": requestID, "request": request,
	}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("claude process exited during control request")
	case res := <-result:
		return res.payload, res.err
	}
}

type Settings struct {
	Model  string
	Effort string
}

// Settings reports what a live process resolved for this session. It is the
// only reading of effort — Claude writes it to neither the transcript nor any
// event — and reports none for a model that does not support it.
func (m *Manager) Settings(ctx context.Context, id string) (Settings, error) {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return Settings{}, fmt.Errorf("claude session %s has no live process", id)
	}
	raw, err := m.controlRequest(ctx, p, map[string]any{"subtype": "get_settings"})
	if err != nil {
		return Settings{}, err
	}
	var payload struct {
		Applied struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"applied"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Settings{}, err
	}
	return Settings{Model: payload.Applied.Model, Effort: payload.Applied.Effort}, nil
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

func (m *Manager) Send(ctx context.Context, id, prompt, cwd, model, appendSystemPrompt string, extraArgs []string, resume bool) (<-chan Result, <-chan Delta, bool, int, error) {
	p, fresh, err := m.ensureProcess(ctx, id, cwd, model, appendSystemPrompt, extraArgs, resume, true)
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
func (m *Manager) Resume(ctx context.Context, id, cwd, appendSystemPrompt string, extraArgs []string) error {
	_, _, err := m.ensureProcess(ctx, id, cwd, "", appendSystemPrompt, extraArgs, true, false)
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
			IsError    bool     `json:"is_error"`
			Errors     []string `json:"errors"`
			ModelUsage map[string]struct {
				ContextWindow int64 `json:"contextWindow"`
			} `json:"modelUsage"`
			SlashCommands []string `json:"slash_commands"`
			Skills        []string `json:"skills"`
			TaskID        string   `json:"task_id"`
			TaskType      string   `json:"task_type"`
			Patch         struct {
				Status string `json:"status"`
			} `json:"patch"`
			Event struct {
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
		if e.Type == "system" {
			trackTask(p, e.Subtype, e.TaskID, e.TaskType, e.Patch.Status)
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
			if len(p.turns) > 0 && p.turns[0].started {
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
					Error: strings.Join(e.Errors, "; "),
					Model: model, ContextWindow: usage.ContextWindow,
				}
			}
			p.mu.Unlock()
			continue
		}
		// Output with no turn behind it (a background subagent's tail) is dropped.
		p.mu.Lock()
		if len(p.turns) > 0 && !p.turns[0].foreign && e.Type == "stream_event" &&
			e.Event.Type == "content_block_delta" && e.Event.Delta.Type == "text_delta" && e.Event.Delta.Text != "" {
			select {
			case p.turns[0].deltas <- Delta{Text: e.Event.Delta.Text}:
			default: // preview may drop under backpressure; JSONL truth-up repairs it
			}
		}
		if len(p.turns) > 0 && e.Message.Model != "" {
			p.turns[0].model = e.Message.Model
		}
		p.mu.Unlock()
	}
	if err := s.Err(); err != nil {
		p.logger.Warn("stream-json read failed", "err", err)
		if p.cmd.Process != nil {
			_ = procutil.KillGroup(p.cmd)
		}
	}
}

func (m *Manager) finishControlRequest(p *process, raw []byte) {
	var msg struct {
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Error     string          `json:"error"`
			Payload   json.RawMessage `json:"response"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Response.RequestID == "" {
		return
	}
	var body struct {
		// Initialize reports a list even when it is empty; every other subtype
		// reports none, which must not read as "this session has no commands".
		Commands *[]struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	_ = json.Unmarshal(msg.Response.Payload, &body)
	p.mu.Lock()
	wait := p.controlWait[msg.Response.RequestID]
	delete(p.controlWait, msg.Response.RequestID)
	if msg.Response.Subtype == "success" && body.Commands != nil {
		commands := make([]Command, 0, len(*body.Commands))
		seen := make(map[string]struct{}, len(*body.Commands))
		for _, command := range *body.Commands {
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
	wait <- controlResult{payload: msg.Response.Payload, err: err}
}

func (m *Manager) finishLifecycle(p *process, uuid, state string) {
	if uuid == "" {
		return
	}
	p.mu.Lock()
	idx := -1
	for i, req := range p.turns {
		if req.uuid == uuid {
			idx = i
			break
		}
	}
	if state == "started" {
		if idx >= 0 {
			p.turns[idx].started = true
		} else {
			// Claude's own command (cron, /loop, deferred resume), uuid minted
			// by Claude. It is the one running, so it goes ahead of anything
			// not yet started.
			at := 0
			for at < len(p.turns) && p.turns[at].started {
				at++
			}
			p.turns = slices.Insert(p.turns, at, &turnRequest{uuid: uuid, started: true, foreign: true})
		}
		p.lastUsed = time.Now()
		p.mu.Unlock()
		return
	}
	if idx < 0 || !isTerminalLifecycle(state) {
		p.mu.Unlock()
		return
	}
	req := p.turns[idx]
	p.turns = append(p.turns[:idx], p.turns[idx+1:]...)
	p.lastUsed = time.Now()
	p.mu.Unlock()
	if !req.foreign {
		req.finish(terminalResult(req, state))
	}
}

// Claude emits exactly one terminal state per command. discarded: the session
// ended with it queued; refused: never admitted.
func isTerminalLifecycle(state string) bool {
	switch state {
	case "completed", "cancelled", "discarded", "refused":
		return true
	}
	return false
}

func terminalResult(req *turnRequest, state string) Result {
	result := Result{Model: req.model}
	if req.runtime != nil {
		result = *req.runtime
	}
	if state != "completed" {
		result.IsError = true
		result.Subtype = state
	} else if !result.IsError {
		result.Subtype = state
	}
	return result
}

// cancelUnlessAnswered drops head after interruptGrace if it is still the head.
// An interrupt only answers a running turn; one that threw sends no terminal
// frame at all.
func cancelUnlessAnswered(p *process, head *turnRequest) {
	time.AfterFunc(interruptGrace, func() {
		p.mu.Lock()
		if len(p.turns) == 0 || p.turns[0] != head {
			p.mu.Unlock()
			return
		}
		p.turns = p.turns[1:]
		p.lastUsed = time.Now()
		p.mu.Unlock()
		p.logger.Warn("interrupt unanswered; turn dropped", "foreign", head.foreign)
		if !head.foreign {
			head.finish(terminalResult(head, "cancelled"))
		}
	})
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
	if m.interactions == nil {
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
		resp, err := m.interactions.Submit(ctx, interaction.Request{
			SessionID:   p.id,
			ToolUseID:   msg.Request.ToolUseID,
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
		if !req.foreign {
			req.finish(Result{IsError: true, Subtype: "process_exited"})
		}
	}
	if err != nil && !wasStopping {
		p.logger.Warn("process exited", "err", err)
	}
}
func (m *Manager) Interrupt(id string) error {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if len(p.turns) > 0 {
		cancelUnlessAnswered(p, p.turns[0])
	}
	p.mu.Unlock()
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
func (m *Manager) LiveSessions() []backend.LiveSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]backend.LiveSession, 0, len(m.processes))
	for id, p := range m.processes {
		p.mu.Lock()
		out = append(out, backend.LiveSession{ID: id, Busy: p.busy()})
		p.mu.Unlock()
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
