// Package appserver implements the newline-delimited JSON-RPC transport used
// by `codex app-server`.
package appserver

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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/procutil"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type response struct {
	result json.RawMessage
	err    error
}

// TurnResult is delivered when app-server announces a terminal turn state.
type TurnResult struct {
	Status string
	Error  string
}

// Delta is ephemeral protocol output used for live preview. The rollout file
// remains the canonical transcript.
type Delta struct {
	Kind string
	Text string
}

// Skill is the safe composer-facing subset of app-server skill metadata.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Enabled     bool   `json:"enabled"`
}

type turnStream struct {
	done   chan TurnResult
	deltas chan Delta
	errMsg string
}

// finish closes deltas before done, so a receiver of done may safely abandon
// deltas. Unread tail deltas are superseded by the canonical transcript.
func (s *turnStream) finish(r TurnResult) {
	close(s.deltas)
	s.done <- r
	close(s.done)
}

type initState struct {
	done     chan struct{}
	err      error
	finished bool
}

// Client owns one lazily-started app-server process. Manager assigns one
// Client to each live root session; derived Codex threads remain internal to
// that worker.
type Client struct {
	bin      string
	logger   *slog.Logger
	hooks    *hook.Manager
	mu       sync.Mutex
	cmd      *exec.Cmd
	in       io.WriteCloser
	pending  map[string]chan response
	turns    map[string]*turnStream
	active   map[string]string // thread id -> id of the turn app-server is running
	threads  map[string]string // thread id -> cwd
	next     atomic.Uint64
	init     *initState
	waitDone chan struct{}
	sandbox  map[string]any
	config   map[string]any
	env      []string
	args     []string
}

// New builds a client for one codex app-server process. args are extra binary
// arguments (config overrides) that must be fixed for the process's lifetime.
func New(bin string, hooks *hook.Manager, sandbox, config map[string]any, env, args []string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{bin: bin, hooks: hooks, sandbox: cloneMap(sandbox), config: cloneMap(config), env: append([]string(nil), env...), args: append([]string(nil), args...), logger: logger, pending: map[string]chan response{}, turns: map[string]*turnStream{}, active: map[string]string{}, threads: map[string]string{}}
}

func scrubEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		name := strings.SplitN(e, "=", 2)[0]
		if strings.HasPrefix(name, "CODEX_") && name != "CODEX_HOME" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (c *Client) ensure(ctx context.Context) error {
	c.mu.Lock()
	if c.cmd != nil {
		state := c.init
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-state.done:
			return state.err
		}
	}
	state := &initState{done: make(chan struct{})}
	args := append(append([]string(nil), c.args...), "app-server")
	cmd := exec.CommandContext(context.Background(), c.bin, args...)
	procutil.ConfigureGroup(cmd)
	cmd.Env = append(scrubEnv(), c.env...)
	in, err := cmd.StdinPipe()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		c.mu.Unlock()
		return err
	}
	waitDone := make(chan struct{})
	c.cmd, c.in, c.init, c.waitDone = cmd, in, state, waitDone
	c.mu.Unlock()
	go c.readLoop(cmd, out)
	go func() {
		err := cmd.Wait()
		close(waitDone)
		c.died(cmd, err)
	}()
	var init struct {
		UserAgent string `json:"userAgent"`
	}
	if err := c.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "usher", "version": "1"}}, &init); err != nil {
		err = fmt.Errorf("initialize app-server: %w", err)
		c.finishInit(cmd, err)
		c.stopProcess(cmd, err)
		return err
	}
	if err := c.write(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		c.finishInit(cmd, err)
		c.stopProcess(cmd, err)
		return err
	}
	c.finishInit(cmd, nil)
	c.logger.Info("codex app-server initialized", "server", init.UserAgent)
	return nil
}

func (c *Client) finishInit(cmd *exec.Cmd, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != cmd || c.init == nil || c.init.finished {
		return
	}
	c.init.err, c.init.finished = err, true
	close(c.init.done)
}

func (c *Client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.in == nil {
		return errors.New("app-server is not running")
	}
	_, err = c.in.Write(b)
	return err
}
func (c *Client) call(ctx context.Context, method string, params any, dst any) error {
	id := fmt.Sprint(c.next.Add(1))
	ch := make(chan response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": json.Number(id), "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if dst != nil {
			return json.Unmarshal(r.result, dst)
		}
		return nil
	}
}

func (c *Client) readLoop(cmd *exec.Cmd, r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), 64<<20)
	for s.Scan() {
		var m rpcMessage
		if json.Unmarshal(s.Bytes(), &m) != nil {
			continue
		}
		c.dispatch(m)
	}
	err := s.Err()
	if err == nil {
		err = io.EOF
	}
	c.stopProcess(cmd, fmt.Errorf("app-server stdout closed: %w", err))
}
func idString(raw json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}
func (c *Client) dispatch(m rpcMessage) {
	if len(m.ID) > 0 && m.Method == "" {
		id := idString(m.ID)
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			if m.Error != nil {
				ch <- response{err: fmt.Errorf("rpc %d: %s", m.Error.Code, m.Error.Message)}
			} else {
				ch <- response{result: m.Result}
			}
		}
		return
	}
	if len(m.ID) > 0 && m.Method != "" {
		go c.handleServerRequest(m)
		return
	}
	if m.Method == "turn/started" {
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ThreadID != "" {
			c.mu.Lock()
			// Interrupts must name the running turn.
			c.active[p.ThreadID] = p.Turn.ID
			c.mu.Unlock()
		}
		return
	}
	if m.Method == "item/agentMessage/delta" || m.Method == "item/reasoning/summaryTextDelta" || m.Method == "item/reasoning/textDelta" {
		var p struct {
			ThreadID string `json:"threadId"`
			Delta    string `json:"delta"`
		}
		_ = json.Unmarshal(m.Params, &p)
		kind := "text"
		if m.Method != "item/agentMessage/delta" {
			kind = "reasoning"
		}
		c.mu.Lock()
		stream := c.turns[p.ThreadID]
		if stream != nil && p.Delta != "" {
			select {
			case stream.deltas <- Delta{Kind: kind, Text: p.Delta}:
			default: // preview may drop under backpressure; rollout truth-up repairs it
			}
		}
		c.mu.Unlock()
		return
	}
	if m.Method == "error" {
		var p struct {
			ThreadID  string `json:"threadId"`
			WillRetry bool   `json:"willRetry"`
			Error     struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(m.Params, &p)
		// Retry errors may still be followed by a successful turn.
		if !p.WillRetry && p.Error.Message != "" {
			c.mu.Lock()
			if stream := c.turns[p.ThreadID]; stream != nil {
				stream.errMsg = p.Error.Message
			}
			c.mu.Unlock()
		}
		return
	}
	if m.Method == "turn/completed" {
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				Status string `json:"status"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		stream := c.turns[p.ThreadID]
		errMsg := ""
		if stream != nil {
			errMsg = stream.errMsg
		}
		delete(c.turns, p.ThreadID)
		delete(c.active, p.ThreadID)
		c.mu.Unlock()
		if stream != nil {
			stream.finish(TurnResult{Status: p.Turn.Status, Error: errMsg})
		}
	}
}

func (c *Client) handleServerRequest(m rpcMessage) {
	switch m.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval",
		"execCommandApproval", "applyPatchApproval":
		c.approval(m)
	case "item/permissions/requestApproval":
		c.permissionsApproval(m)
	case "mcpServer/elicitation/request":
		c.mcpElicitation(m)
	case "item/tool/requestUserInput":
		c.replyError(m.ID, -32000, "request_user_input is not supported by this usher client")
	case "item/tool/call":
		c.replyError(m.ID, -32000, "dynamic client tools are not supported by this usher client")
	case "currentTime/read":
		c.replyResult(m.ID, map[string]any{"currentTimeAt": time.Now().Unix()})
	case "account/chatgptAuthTokens/refresh":
		c.replyError(m.ID, -32000, "ChatGPT token refresh is not supported by this usher client")
	case "attestation/generate":
		c.replyError(m.ID, -32000, "client attestation is not supported by this usher client")
	default:
		c.logger.Warn("rejecting unknown app-server request", "method", m.Method)
		c.replyError(m.ID, -32601, "unsupported app-server request: "+m.Method)
	}
}

// mcpElicitation handles confirmation forms; forms requiring input are declined.
func (c *Client) mcpElicitation(m rpcMessage) {
	var p struct {
		ThreadID        string `json:"threadId"`
		ServerName      string `json:"serverName"`
		Mode            string `json:"mode"`
		RequestedSchema struct {
			Required []string `json:"required"`
		} `json:"requestedSchema"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		c.replyError(m.ID, -32602, "invalid MCP elicitation parameters")
		return
	}

	c.mu.Lock()
	h := c.hooks
	cwd := c.threads[p.ThreadID]
	c.mu.Unlock()

	action := "decline"
	var content any
	confirmation := (p.Mode == "form" || p.Mode == "openai/form") && len(p.RequestedSchema.Required) == 0
	if confirmation && h != nil {
		tool := "MCP"
		if p.ServerName != "" {
			tool += ": " + p.ServerName
		}
		r, err := h.Submit(context.Background(), hook.Event{
			SessionID: p.ThreadID,
			Event:     "PermissionRequest",
			ToolName:  tool,
			ToolInput: json.RawMessage(m.Params),
			Cwd:       cwd,
		})
		if err == nil && r.Behavior == "allow" {
			action = "accept"
			content = map[string]any{}
		}
	}
	c.replyResult(m.ID, map[string]any{"action": action, "content": content})
}

func (c *Client) replyResult(id json.RawMessage, result any) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *Client) replyError(id json.RawMessage, code int, message string) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

// permissionsApproval returns a granted profile rather than a decision field.
func (c *Client) permissionsApproval(m rpcMessage) {
	var p struct {
		ThreadID    string          `json:"threadId"`
		Cwd         string          `json:"cwd"`
		Permissions json.RawMessage `json:"permissions"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		c.replyError(m.ID, -32602, "invalid permissions approval parameters")
		return
	}

	c.mu.Lock()
	h := c.hooks
	if p.Cwd == "" {
		p.Cwd = c.threads[p.ThreadID]
	}
	c.mu.Unlock()

	result := map[string]any{"permissions": map[string]any{}}
	if h != nil {
		r, err := h.Submit(context.Background(), hook.Event{
			SessionID:   p.ThreadID,
			Event:       "PermissionRequest",
			ToolName:    "Permissions",
			ToolInput:   json.RawMessage(m.Params),
			Cwd:         p.Cwd,
			AllowAlways: true,
		})
		if err == nil && r.Behavior == "allow" {
			var granted map[string]any
			if json.Unmarshal(p.Permissions, &granted) == nil && granted != nil {
				result["permissions"] = granted
			}
			if r.Scope == "session" {
				result["scope"] = "session"
			} else {
				result["scope"] = "turn"
			}
		}
	}
	c.replyResult(m.ID, result)
}

func (c *Client) approval(m rpcMessage) {
	var p map[string]json.RawMessage
	_ = json.Unmarshal(m.Params, &p)
	var sid, cwd, command string
	_ = json.Unmarshal(p["threadId"], &sid)
	_ = json.Unmarshal(p["cwd"], &cwd)
	_ = json.Unmarshal(p["command"], &command)
	tool := "Bash"
	if strings.Contains(strings.ToLower(m.Method), "filechange") || strings.Contains(strings.ToLower(m.Method), "applypatch") {
		tool = "Edit"
	}
	c.mu.Lock()
	h := c.hooks
	if cwd == "" {
		cwd = c.threads[sid]
	}
	c.mu.Unlock()
	input := json.RawMessage(m.Params)
	if tool == "Bash" && command != "" {
		input, _ = json.Marshal(map[string]any{"command": command})
	}
	decision := "decline"
	if h != nil {
		allowAlways := supportsAllowAlways(m.Method, p["availableDecisions"])
		if r, err := h.Submit(context.Background(), hook.Event{SessionID: sid, Event: "PermissionRequest", ToolName: tool, ToolInput: input, Cwd: cwd, AllowAlways: allowAlways}); err == nil && r.Behavior == "allow" {
			if r.Scope == "session" && allowAlways {
				decision = "acceptForSession"
			} else {
				decision = "accept"
			}
		}
	}
	// The legacy request methods use the older ReviewDecision enum.
	if m.Method == "execCommandApproval" || m.Method == "applyPatchApproval" {
		if decision == "acceptForSession" {
			decision = "approved_for_session"
		} else if decision == "accept" {
			decision = "approved"
		} else {
			decision = "denied"
		}
	}
	c.replyResult(m.ID, map[string]any{"decision": decision})
}

func supportsAllowAlways(method string, availableDecisions json.RawMessage) bool {
	return method == "execCommandApproval" || method == "applyPatchApproval" ||
		rawJSONContainsString(availableDecisions, "acceptForSession")
}

func rawJSONContainsString(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return false
	}
	for _, value := range values {
		var decision string
		if json.Unmarshal(value, &decision) == nil && decision == want {
			return true
		}
	}
	return false
}

func (c *Client) died(cmd *exec.Cmd, err error) {
	c.failProcess(cmd, fmt.Errorf("codex app-server exited: %w", err))
}

func (c *Client) failProcess(cmd *exec.Cmd, err error) {
	c.mu.Lock()
	if c.cmd != cmd {
		c.mu.Unlock()
		return
	}
	if c.init != nil && !c.init.finished {
		c.init.err = err
		c.init.finished = true
		close(c.init.done)
	}
	c.cmd = nil
	c.in = nil
	c.waitDone = nil
	pending := c.pending
	turns := c.turns
	c.pending = map[string]chan response{}
	c.turns = map[string]*turnStream{}
	c.active = map[string]string{}
	c.threads = map[string]string{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- response{err: err}
	}
	for _, stream := range turns {
		stream.finish(TurnResult{Status: "failed"})
	}
}
func (c *Client) stopProcess(cmd *exec.Cmd, reason error) {
	c.mu.Lock()
	if c.cmd != cmd {
		c.mu.Unlock()
		return
	}
	in := c.in
	c.mu.Unlock()
	if in != nil {
		_ = in.Close()
	}
	_ = procutil.KillGroup(cmd)
	c.failProcess(cmd, reason)
}
func (c *Client) Shutdown() {
	c.mu.Lock()
	cmd := c.cmd
	in := c.in
	done := c.waitDone
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	if in != nil {
		_ = in.Close()
	}
	if done != nil {
		select {
		case <-done:
			return
		case <-time.After(2 * time.Second):
		}
	}
	c.stopProcess(cmd, errors.New("app-server shutdown timeout"))
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func (c *Client) threadParams(cwd, model string) map[string]any {
	config := cloneMap(c.config)
	if model != "" {
		config["model"] = model
	}
	if _, ok := config["mcp_servers.usher.command"]; ok {
		config["mcp_servers.usher.cwd"] = cwd
	}
	p := map[string]any{"cwd": cwd, "approvalPolicy": "on-request", "config": config}
	for k, v := range c.sandbox {
		p[k] = v
	}
	return p
}

func (c *Client) StartThread(ctx context.Context, cwd, model string) (string, error) {
	if err := c.ensure(ctx); err != nil {
		return "", err
	}
	params := c.threadParams(cwd, model)
	var out struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := c.call(ctx, "thread/start", params, &out); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.threads[out.Thread.ID] = cwd
	c.mu.Unlock()
	return out.Thread.ID, nil
}

func (c *Client) Skills(ctx context.Context, cwd string) ([]Skill, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			Cwd    string  `json:"cwd"`
			Skills []Skill `json:"skills"`
		} `json:"data"`
	}
	if err := c.call(ctx, "skills/list", map[string]any{"cwds": []string{cwd}, "forceReload": false}, &out); err != nil {
		return nil, err
	}
	var skills []Skill
	for _, entry := range out.Data {
		if entry.Cwd != "" && entry.Cwd != cwd {
			continue
		}
		skills = append(skills, entry.Skills...)
	}
	return skills, nil
}

func (c *Client) StartTurn(ctx context.Context, id, prompt, cwd string) (<-chan TurnResult, <-chan Delta, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	_, ok := c.threads[id]
	c.mu.Unlock()
	if !ok {
		params := c.threadParams(cwd, "")
		params["threadId"] = id
		if err := c.call(ctx, "thread/resume", params, nil); err != nil {
			return nil, nil, err
		}
		c.mu.Lock()
		c.threads[id] = cwd
		c.mu.Unlock()
	}
	return c.startOperation(ctx, id, "turn/start", map[string]any{
		"threadId": id, "input": []map[string]string{{"type": "text", "text": prompt}},
	})
}

func (c *Client) startOperation(ctx context.Context, id, method string, params any) (<-chan TurnResult, <-chan Delta, error) {
	stream := &turnStream{done: make(chan TurnResult, 1), deltas: make(chan Delta, 256)}
	c.mu.Lock()
	c.turns[id] = stream
	c.mu.Unlock()
	if err := c.call(ctx, method, params, nil); err != nil {
		c.mu.Lock()
		delete(c.turns, id)
		c.mu.Unlock()
		return nil, nil, err
	}
	return stream.done, stream.deltas, nil
}

func (c *Client) Compact(ctx context.Context, id string) (<-chan TurnResult, <-chan Delta, error) {
	return c.startOperation(ctx, id, "thread/compact/start", map[string]any{"threadId": id})
}

func (c *Client) Review(ctx context.Context, id, instructions string) (<-chan TurnResult, <-chan Delta, error) {
	target := map[string]any{"type": "uncommittedChanges"}
	if instructions != "" {
		target = map[string]any{"type": "custom", "instructions": instructions}
	}
	return c.startOperation(ctx, id, "review/start", map[string]any{
		"threadId": id, "delivery": "inline", "target": target,
	})
}

func (c *Client) ResumeThread(ctx context.Context, id, cwd string) error {
	if err := c.ensure(ctx); err != nil {
		return err
	}
	params := c.threadParams(cwd, "")
	params["threadId"] = id
	if err := c.call(ctx, "thread/resume", params, nil); err != nil {
		return err
	}
	c.mu.Lock()
	c.threads[id] = cwd
	c.mu.Unlock()
	return nil
}

// RenameThread updates display metadata without starting a turn.
func (c *Client) RenameThread(ctx context.Context, id, title string) error {
	if err := c.ensure(ctx); err != nil {
		return err
	}
	return c.call(ctx, "thread/name/set", map[string]any{
		"threadId": id,
		"name":     title,
	}, nil)
}

func (c *Client) Interrupt(ctx context.Context, id string) error {
	c.mu.Lock()
	running := c.cmd != nil
	_, turning := c.turns[id]
	turnID, active := c.active[id]
	if active {
		turning = true
	}
	c.mu.Unlock()
	if !running || !turning {
		return nil
	}
	// An empty turn ID covers the window before turn/started arrives.
	return c.call(ctx, "turn/interrupt", map[string]any{"threadId": id, "turnId": turnID}, nil)
}
func (c *Client) Has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.threads[id]
	return ok
}

func (c *Client) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil
}

func (c *Client) Busy(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, turning := c.turns[id]
	_, active := c.active[id]
	return turning || active
}
func (c *Client) LiveSessions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.threads))
	for id := range c.threads {
		out = append(out, id)
	}
	return out
}
func (c *Client) Kill(ctx context.Context, id string) error {
	_ = c.Interrupt(ctx, id)
	c.mu.Lock()
	stream := c.turns[id]
	delete(c.threads, id)
	delete(c.turns, id)
	c.mu.Unlock()
	if stream != nil {
		stream.finish(TurnResult{Status: "interrupted"})
	}
	return nil
}
