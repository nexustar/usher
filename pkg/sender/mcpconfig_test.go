package sender

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPConfigArgs(t *testing.T) {
	dir := t.TempDir()
	hookSock := filepath.Join(dir, "hook.sock")
	args := claudeMCPConfigArgs(hookSock, false, slog.Default())

	if len(args) != 2 || args[0] != "--mcp-config" {
		t.Fatalf("expected [--mcp-config <path>], got %v", args)
	}
	cfgPath := args[1]
	if filepath.Dir(cfgPath) != dir {
		t.Fatalf("config should sit next to hook sock; got %s", cfgPath)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command    string   `json:"command"`
			Args       []string `json:"args"`
			AlwaysLoad bool     `json:"alwaysLoad"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	srv, ok := cfg.MCPServers["usher"]
	if !ok {
		t.Fatalf("config missing usher server: %s", raw)
	}
	if len(srv.Args) != 1 || srv.Args[0] != "mcp-stdio" {
		t.Fatalf("expected args [mcp-stdio], got %v", srv.Args)
	}
	if !srv.AlwaysLoad {
		t.Fatal("server must set alwaysLoad:true to bypass Tool Search deferral")
	}
	if !filepath.IsAbs(srv.Command) || strings.TrimSpace(srv.Command) == "" {
		t.Fatalf("command should be an absolute exe path, got %q", srv.Command)
	}
}

// The router variant is a separate file, so an ordinary session can never load
// the routing tools by picking up the config usher writes for everyone.
func TestMCPConfigArgs_RouterTools(t *testing.T) {
	dir := t.TempDir()
	hookSock := filepath.Join(dir, "hook.sock")
	plain := claudeMCPConfigArgs(hookSock, false, slog.Default())
	args := claudeMCPConfigArgs(hookSock, true, slog.Default())

	if len(args) != 2 || args[1] == plain[1] {
		t.Fatalf("router config must be its own file: %v vs %v", args, plain)
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	if got := cfg.MCPServers["usher"].Args; len(got) != 2 || got[1] != "--router-tools" {
		t.Fatalf("router args = %v", got)
	}
}

// Empty hook sock disables the feature (returns nil) rather than erroring.
func TestMCPConfigArgs_NoHookSock(t *testing.T) {
	if got := claudeMCPConfigArgs("", false, slog.Default()); got != nil {
		t.Fatalf("expected nil for empty hook sock, got %v", got)
	}
}

func TestClaudeHookSettings(t *testing.T) {
	raw := claudeHookSettings("/tmp/usher-hook.sock", true, slog.Default())
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["statusLine"]; ok {
		t.Fatalf("unexpected statusLine in settings: %s", raw)
	}
	// usher's own MCP tools are pre-allowed at the server level; without MCP
	// injection no rule is emitted at all.
	if !strings.Contains(raw, `"permissions":{"allow":["mcp__usher"]}`) {
		t.Fatalf("usher MCP server not pre-allowed: %s", raw)
	}
	if bare := claudeHookSettings("/tmp/usher-hook.sock", false, slog.Default()); strings.Contains(bare, "permissions") {
		t.Fatalf("permissions emitted without MCP injection: %s", bare)
	}
	hooks := cfg["hooks"].(map[string]any)
	assertHook := func(event, matcher, commandSuffix string) {
		t.Helper()
		groups, ok := hooks[event].([]any)
		if !ok || len(groups) != 1 {
			t.Fatalf("%s groups = %#v", event, hooks[event])
		}
		group := groups[0].(map[string]any)
		if group["matcher"] != matcher {
			t.Errorf("%s matcher = %q, want %q", event, group["matcher"], matcher)
		}
		handlers := group["hooks"].([]any)
		handler := handlers[0].(map[string]any)
		command := handler["command"].(string)
		if !strings.HasPrefix(command, "USHER_HOOK_SOCK=/tmp/usher-hook.sock ") || !strings.HasSuffix(command, commandSuffix) {
			t.Errorf("%s command = %q", event, command)
		}
		if handler["timeout"] != float64(604800) {
			t.Errorf("%s timeout = %v", event, handler["timeout"])
		}
	}
	if _, ok := hooks["PermissionRequest"]; ok {
		t.Fatalf("PermissionRequest must use the stdio control protocol: %s", raw)
	}
	assertHook("PreToolUse", "AskUserQuestion", " hook PreToolUse")
}
