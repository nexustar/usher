package sender

import (
	"log/slog"
	"testing"

	"github.com/nexustar/usher/internal/codex"
)

func TestCodexMCPConfigUsesNativeValues(t *testing.T) {
	cfg := codexMCPConfig(slog.Default())
	if _, ok := cfg["mcp_servers.usher.command"].(string); !ok {
		t.Fatalf("command: %#v", cfg)
	}
	if args, ok := cfg["mcp_servers.usher.args"].([]string); !ok || len(args) != 1 || args[0] != "mcp-stdio" {
		t.Fatalf("args must be a native string array: %#v", cfg["mcp_servers.usher.args"])
	}
	if cfg["mcp_servers.usher.default_tools_approval_mode"] != "approve" {
		t.Fatalf("usher MCP tools must be pre-approved: %#v", cfg["mcp_servers.usher.default_tools_approval_mode"])
	}
	direct, ok := cfg["features.code_mode.direct_only_tool_namespaces"].([]string)
	if !ok || len(direct) != 2 || direct[0] != "mcp__usher" || direct[1] != "usher" {
		t.Fatalf("usher namespace must bypass deferred loading: %#v", direct)
	}
}

func TestCodexHeadlessParams(t *testing.T) {
	sandbox, cfg := codex.ParseHeadlessArgs([]string{"--sandbox=read-only", "-c", "model_reasoning_effort=high", "-c", "features.foo=true"}, slog.Default())
	if sandbox["sandbox"] != "read-only" || cfg["model_reasoning_effort"] != "high" || cfg["features.foo"] != true {
		t.Fatalf("sandbox=%#v config=%#v", sandbox, cfg)
	}
}
