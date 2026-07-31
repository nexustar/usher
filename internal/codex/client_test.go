package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/interaction"
)

type testWriteCloser struct{ bytes.Buffer }

func (*testWriteCloser) Close() error { return nil }

func TestThreadParamsCarryPolicySandboxAndNativeMCP(t *testing.T) {
	c := New("codex", interaction.New(""), map[string]any{"sandbox": "workspace-write"}, map[string]any{
		"mcp_servers.usher.command":                      "/usr/bin/usher",
		"mcp_servers.usher.args":                         []string{"mcp-stdio"},
		"mcp_servers.usher.env_vars":                     []string{"USHER_HOOK_SOCK"},
		"mcp_servers.usher.default_tools_approval_mode":  "approve",
		"features.code_mode.direct_only_tool_namespaces": []string{"usher"},
	}, nil, nil, nil)
	p := c.threadParams("/work/project", "gpt-test")
	if p["approvalPolicy"] != "on-request" || p["sandbox"] != "workspace-write" || p["cwd"] != "/work/project" {
		t.Fatalf("missing thread policy params: %#v", p)
	}
	cfg := p["config"].(map[string]any)
	if cfg["mcp_servers.usher.cwd"] != "/work/project" || cfg["model"] != "gpt-test" {
		t.Fatalf("missing per-thread config: %#v", cfg)
	}
	if cfg["mcp_servers.usher.default_tools_approval_mode"] != "approve" {
		t.Fatalf("usher MCP approval mode was dropped: %#v", cfg)
	}
	args, ok := cfg["mcp_servers.usher.args"].([]string)
	if !ok || len(args) != 1 || args[0] != "mcp-stdio" {
		t.Fatalf("MCP args are not a native array: %#v", cfg["mcp_servers.usher.args"])
	}
	direct, ok := cfg["features.code_mode.direct_only_tool_namespaces"].([]string)
	if !ok || len(direct) != 1 || direct[0] != "usher" {
		t.Fatalf("usher namespace must be direct: %#v", direct)
	}
}

func TestAgentMessageDeltaRoutesByThread(t *testing.T) {
	c := New("codex", nil, nil, nil, nil, nil, nil)
	stream := &turnStream{done: make(chan TurnResult, 1), deltas: make(chan Delta, 2)}
	c.turns["thread-1"] = stream
	c.dispatch(rpcMessage{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"hello"}`)})
	select {
	case d := <-stream.deltas:
		if d.Kind != "text" || d.Text != "hello" {
			t.Fatalf("delta = %+v", d)
		}
	default:
		t.Fatal("agent message delta not routed")
	}
}

func TestErrorNotificationSurfacesFinalMessageAtTurnEnd(t *testing.T) {
	c := New("codex", nil, nil, nil, nil, nil, nil)
	stream := &turnStream{done: make(chan TurnResult, 1), deltas: make(chan Delta, 1)}
	c.turns["thread-1"] = stream
	c.dispatch(rpcMessage{Method: "error", Params: json.RawMessage(`{"threadId":"thread-1","willRetry":true,"error":{"message":"Service Unavailable"}}`)})
	if stream.errMsg != "" {
		t.Fatalf("retry error stashed: %q", stream.errMsg)
	}
	c.dispatch(rpcMessage{Method: "error", Params: json.RawMessage(`{"threadId":"thread-1","willRetry":false,"error":{"message":"Service Unavailable"}}`)})
	c.dispatch(rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed"}}`)})
	select {
	case r := <-stream.done:
		if r.Status != "failed" || r.Error != "Service Unavailable" {
			t.Fatalf("result = %+v", r)
		}
	default:
		t.Fatal("turn/completed did not finish the stream")
	}
}

func TestInterruptStoppedClientIsNoop(t *testing.T) {
	c := New("definitely-not-a-command", interaction.New(""), nil, nil, nil, nil, nil)
	if err := c.Interrupt(context.Background(), "missing"); err != nil {
		t.Fatalf("interrupt stopped client: %v", err)
	}
}

func TestEnsureConcurrentWaitsForInitializedHandshake(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests")
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
IFS= read -r init
printf '%s\n' "$init" >> "$FAKE_LOG"
sleep 0.1
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"fake/1"}}'
IFS= read -r initialized
printf '%s\n' "$initialized" >> "$FAKE_LOG"
while IFS= read -r line; do :; done
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	c := New(script, interaction.New(""), nil, nil, []string{"FAKE_LOG=" + logPath}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- c.ensure(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var b []byte
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b, _ = os.ReadFile(logPath)
		if strings.Count(strings.TrimSpace(string(b)), "\n") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Shutdown()
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"method":"initialized"`) {
		t.Fatalf("handshake requests: %s", b)
	}
}

func TestClientTracksSpontaneousTurnActivity(t *testing.T) {
	c := New("unused", nil, nil, nil, nil, nil, nil)
	c.dispatch(rpcMessage{Method: "turn/started", Params: json.RawMessage(`{"threadId":"session-1","turn":{"id":"turn-1"}}`)})
	if !c.Busy("session-1") {
		t.Fatal("turn/started did not mark the session busy")
	}
	c.dispatch(rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}`)})
	if c.Busy("session-1") {
		t.Fatal("turn/completed did not clear session activity")
	}
}

func TestRawJSONContainsString(t *testing.T) {
	raw := json.RawMessage(`["accept",{"acceptWithExecpolicyAmendment":{}},"acceptForSession","decline"]`)
	if !rawJSONContainsString(raw, "acceptForSession") {
		t.Fatal("decision was not found")
	}
	if rawJSONContainsString(raw, "cancel") {
		t.Fatal("missing decision was reported present")
	}
	if rawJSONContainsString(json.RawMessage(`not json`), "accept") {
		t.Fatal("invalid JSON matched")
	}
}

func TestSupportsAllowAlways(t *testing.T) {
	if !supportsAllowAlways("execCommandApproval", nil) || !supportsAllowAlways("applyPatchApproval", nil) {
		t.Fatal("legacy approval method did not expose approved_for_session")
	}
	if !supportsAllowAlways("item/commandExecution/requestApproval", json.RawMessage(`["accept","acceptForSession"]`)) {
		t.Fatal("modern acceptForSession was not detected")
	}
	if supportsAllowAlways("item/commandExecution/requestApproval", json.RawMessage(`["accept","decline"]`)) {
		t.Fatal("modern request without acceptForSession exposed allow always")
	}
}

func TestPermissionsApprovalUsesPermissionProfileResponse(t *testing.T) {
	interactions := interaction.New("")
	interactions.SetAutoApprove("thread-1", true)
	out := new(testWriteCloser)
	c := New("unused", interactions, nil, nil, nil, nil, nil)
	c.in = out
	c.permissionsApproval(rpcMessage{
		ID:     json.RawMessage(`7`),
		Method: "item/permissions/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","cwd":"/work","startedAtMs":1,"permissions":{"fileSystem":{"read":["/outside/image.png"]}}}`),
	})

	var response struct {
		Result struct {
			Permissions map[string]any `json:"permissions"`
			Scope       string         `json:"scope"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.Scope != "turn" || response.Result.Permissions["fileSystem"] == nil {
		t.Fatalf("unexpected permissions response: %s", out.Bytes())
	}
}

func TestPermissionsApprovalDenialReturnsEmptyProfile(t *testing.T) {
	out := new(testWriteCloser)
	c := New("unused", nil, nil, nil, nil, nil, nil)
	c.in = out
	c.permissionsApproval(rpcMessage{
		ID:     json.RawMessage(`8`),
		Method: "item/permissions/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","cwd":"/work","startedAtMs":1,"permissions":{"network":{"enabled":true}}}`),
	})

	var response struct {
		Result struct {
			Permissions map[string]any `json:"permissions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Result.Permissions) != 0 {
		t.Fatalf("denial granted permissions: %s", out.Bytes())
	}
}

func TestNewServerRequestsUseTheirOwnResponseShapes(t *testing.T) {
	tests := []struct {
		method string
		want   string
	}{
		{"mcpServer/elicitation/request", `"action":"decline"`},
		{"currentTime/read", `"currentTimeAt":`},
		{"item/tool/requestUserInput", `"error":`},
		{"unknown/newRequest", `"code":-32601`},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			out := new(testWriteCloser)
			c := New("unused", nil, nil, nil, nil, nil, nil)
			c.in = out
			c.handleServerRequest(rpcMessage{ID: json.RawMessage(`9`), Method: tt.method, Params: json.RawMessage(`{}`)})
			if !strings.Contains(out.String(), tt.want) {
				t.Fatalf("response = %s, want %s", out.String(), tt.want)
			}
		})
	}
}

func TestMcpConfirmationElicitationUsesPermissionDecision(t *testing.T) {
	interactions := interaction.New("")
	interactions.SetAutoApprove("thread-1", true)
	out := new(testWriteCloser)
	c := New("unused", interactions, nil, nil, nil, nil, nil)
	c.in = out
	c.mcpElicitation(rpcMessage{
		ID:     json.RawMessage(`10`),
		Method: "mcpServer/elicitation/request",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","serverName":"other","mode":"form","message":"Allow this tool?","requestedSchema":{"type":"object","properties":{}}}`),
	})
	if !strings.Contains(out.String(), `"action":"accept"`) || !strings.Contains(out.String(), `"content":{}`) {
		t.Fatalf("response = %s", out.String())
	}
}

func TestMcpInputFormIsNotAcceptedWithoutAnswers(t *testing.T) {
	interactions := interaction.New("")
	interactions.SetAutoApprove("thread-1", true)
	out := new(testWriteCloser)
	c := New("unused", interactions, nil, nil, nil, nil, nil)
	c.in = out
	c.mcpElicitation(rpcMessage{
		ID:     json.RawMessage(`11`),
		Method: "mcpServer/elicitation/request",
		Params: json.RawMessage(`{"threadId":"thread-1","serverName":"other","mode":"form","message":"Enter a value","requestedSchema":{"type":"object","required":["value"],"properties":{"value":{"type":"string"}}}}`),
	})
	if !strings.Contains(out.String(), `"action":"decline"`) || !strings.Contains(out.String(), `"content":null`) {
		t.Fatalf("response = %s", out.String())
	}
}
