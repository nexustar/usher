package pi

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// PrepareUsherRouterExtension materializes the extension that registers
// usher's routing tools, as an MCP server, for the session designated as
// usher's router. It needs pi 1.0, the first with MCP support.
func PrepareUsherRouterExtension(dataDir, hookSock string) (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		return "", err
	}
	binJSON, _ := json.Marshal(exe)
	sockJSON, _ := json.Marshal(hookSock)
	body := strings.NewReplacer(
		"__USHER_BIN__", string(binJSON),
		"__USHER_HOOK_SOCK__", string(sockJSON),
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
