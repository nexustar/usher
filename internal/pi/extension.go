package pi

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexustar/usher/internal/mcptools"
)

//go:embed usher-extension.ts
var usherExtension []byte

//go:embed usher-router-extension.ts
var usherRouterExtension string

// PrepareUsherExtension materializes the embedded pi extension beside usher's
// other generated runtime configuration and returns its absolute path.
func PrepareUsherExtension(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "pi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(dir, "usher-extension.ts"))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, usherExtension, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// PrepareUsherRouterExtension materializes the routing-tools bridge extension
// for the session designated as usher's router. The tool catalog is baked in
// from mcptools.RouterTools() and every call bridges to a spawned
// `usher mcp-stdio --router-tools`, keeping schemas, descriptions, and
// dispatch single-sourced in Go; regeneration at every serve start is what
// keeps the copy current.
func PrepareUsherRouterExtension(dataDir, hookSock string) (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		return "", err
	}
	// Only the fields the bridge consumes: the MCP-side extras (_meta) would
	// trip excess-property checks in the generated literal.
	type toolSpec struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	var specs []toolSpec
	for _, tool := range mcptools.RouterTools() {
		specs = append(specs, toolSpec{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	catalog, err := json.Marshal(specs)
	if err != nil {
		return "", err
	}
	binJSON, _ := json.Marshal(exe)
	sockJSON, _ := json.Marshal(hookSock)
	body := strings.NewReplacer(
		"__USHER_BIN__", string(binJSON),
		"__USHER_HOOK_SOCK__", string(sockJSON),
		"__ROUTER_TOOLS_JSON__", string(catalog),
	).Replace(usherRouterExtension)
	dir := filepath.Join(dataDir, "pi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(dir, "usher-router-extension.ts"))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
