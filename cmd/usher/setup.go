package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runSetup wires usher into ~/.claude/settings.json by adding a PreToolUse
// hook that runs `usher hook PreToolUse`. Existing user-defined hooks are
// preserved; previous usher entries (identified by command suffix) are
// replaced so re-running is idempotent.
//
// The hook talks to usher over a Unix domain socket (see runHook). By
// default `usher hook` resolves the socket path from the data dir at
// runtime; pass --sock here to pin a specific path into the hook command
// (useful if you run usher with a non-default --data-dir).
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	sock := fs.String("sock", "", "USHER_HOOK_SOCK to set in the hook command (defaults to <data-dir>/hook.sock at runtime)")
	remove := fs.Bool("remove", false, "remove the usher hook from settings.json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	// usher modifies only the backends that are installed (their config dir
	// exists); at least one must be present.
	claudePresent := isDir(filepath.Join(home, ".claude"))
	codexPresent := isDir(filepath.Join(home, ".codex"))
	if !claudePresent && !codexPresent {
		return fmt.Errorf("no backend found: neither ~/.claude (Claude Code) nor ~/.codex (Codex) exists; install/run one first")
	}

	if claudePresent {
		if err := setupClaudeHook(home, exe, *sock, *remove); err != nil {
			return err
		}
	} else if !*remove {
		fmt.Println("claude not detected (~/.claude absent); skipped claude hook.")
	}

	if err := setupCodexHook(home, exe, *sock, *remove); err != nil {
		fmt.Fprintln(os.Stderr, "codex hook setup:", err)
	}

	if !*remove {
		fmt.Println()
		fmt.Println("Re-run with --remove to uninstall.")
	}
	return nil
}

// setupClaudeHook installs (or removes) the PreToolUse hook in
// ~/.claude/settings.json. The 7-day timeout is effectively unbounded — a human
// resolves the prompt via usher's web UI; on timeout claude falls back to its
// pane TUI prompt, which the web UI can't answer.
func setupClaudeHook(home, exe, sock string, remove bool) error {
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	settings, err := readSettings(settingsPath)
	if err != nil {
		return err
	}
	hooksRoot, _ := settings["hooks"].(map[string]any)
	if hooksRoot == nil {
		hooksRoot = map[string]any{}
	}
	preToolUse, _ := hooksRoot["PreToolUse"].([]any)
	preToolUse = stripUsherEntries(preToolUse)

	if !remove {
		cmd := exe + " hook PreToolUse"
		if sock != "" {
			cmd = "USHER_HOOK_SOCK=" + sock + " " + cmd
		}
		preToolUse = append(preToolUse, map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{"type": "command", "command": cmd, "timeout": 604800},
			},
		})
	}

	if len(preToolUse) == 0 {
		delete(hooksRoot, "PreToolUse")
	} else {
		hooksRoot["PreToolUse"] = preToolUse
	}
	if len(hooksRoot) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooksRoot
	}

	if err := writeSettings(settingsPath, settings); err != nil {
		return err
	}
	if remove {
		fmt.Printf("removed usher hook from %s\n", settingsPath)
	} else {
		fmt.Printf("installed usher hook in %s\n", settingsPath)
		fmt.Printf("  command: %s hook PreToolUse\n", exe)
		fmt.Printf("  timeout: 604800s (7d — web UI resolves permissions)\n")
	}
	return nil
}

// Codex's config.toml is TOML, which the standard library can't encode. Rather
// than parse it, usher manages a single marker-delimited block: setup strips any
// existing block and (re)appends a fresh one, so re-running is idempotent and a
// user's own config is never touched.
const (
	codexHookBegin = "# >>> usher codex permission hook (managed; do not edit this block) >>>"
	codexHookEnd   = "# <<< usher codex permission hook <<<"
)

// setupCodexHook installs (or, with remove, uninstalls) the Codex
// PermissionRequest hook in ~/.codex/config.toml. It is a no-op when Codex isn't
// installed (~/.codex absent) and nothing to remove.
func setupCodexHook(home, exe, sock string, remove bool) error {
	codexDir := filepath.Join(home, ".codex")
	configPath := filepath.Join(codexDir, "config.toml")

	if _, err := os.Stat(codexDir); errors.Is(err, os.ErrNotExist) {
		if !remove {
			fmt.Println()
			fmt.Println("codex not detected (~/.codex absent); skipped codex hook — re-run setup after installing codex.")
		}
		return nil
	}

	existing, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	content := stripCodexBlock(string(existing))

	if remove {
		if err := writeCodexConfig(configPath, content); err != nil {
			return err
		}
		fmt.Printf("removed usher hook from %s\n", configPath)
		return nil
	}

	// A single-table [hooks.PermissionRequest] can't coexist with our
	// array-of-tables append — refuse rather than write invalid TOML.
	if hasBareTable(content, "[hooks.PermissionRequest]") {
		return fmt.Errorf("%s already defines [hooks.PermissionRequest]; add the usher hook manually", configPath)
	}

	cmd := exe + " hook PermissionRequest"
	if sock != "" {
		cmd = "USHER_HOOK_SOCK=" + sock + " " + cmd
	}

	// Persist Codex's hook-trust (so no spawn-time bypass flag is needed) ONLY
	// when our hook is the sole PermissionRequest group and no [hooks.state]
	// exists: then the trust key is "<config-path>:permission_request:0:0" and
	// the [hooks.state."key"] sub-table can't collide. With pre-existing
	// PermissionRequest hooks or state, the positional key / table could be
	// wrong, so we register the hook but leave trusting to the user.
	trustState := ""
	autoTrust := !hasBareTable(content, "[[hooks.PermissionRequest]]") && !strings.Contains(content, "hooks.state")
	if autoTrust {
		key := configPath + ":permission_request:0:0"
		trustState = "\n[hooks.state." + tomlBasicString(key) + "]\n" +
			"trusted_hash = " + tomlBasicString(codexHookTrustedHash(cmd)) + "\n"
	}

	content = strings.TrimRight(content, "\n")
	if content != "" {
		content += "\n\n"
	}
	content += codexHookBlock(cmd, trustState)

	if err := writeCodexConfig(configPath, content); err != nil {
		return err
	}
	fmt.Printf("installed usher hook in %s\n", configPath)
	fmt.Printf("  command: %s hook PermissionRequest\n", exe)
	if autoTrust {
		fmt.Printf("  trust: persisted (no bypass flag needed)\n")
	} else {
		fmt.Printf("  trust: NOT auto-set (existing codex hooks/state) — trust it once in\n" +
			"         codex's /hooks, or run usher with --codex-args=--dangerously-bypass-hook-trust.\n")
	}
	return nil
}

// codexHookTimeoutSec is the hook command timeout (7d ≈ unbounded; a human
// resolves the prompt in the usher UI). Folded into the trust hash, so it must
// match what Codex normalizes to.
const codexHookTimeoutSec = 604800

// codexHookTrustedHash mirrors Codex's content hash for the hook: sha256 of the
// canonical JSON (sorted keys) of the normalized identity
//
//	{"event_name":"permission_request",
//	 "hooks":[{"async":false,"command":<cmd>,"timeout":604800,"type":"command"}]}
//
// Struct fields are declared alphabetically so Go's marshal matches Codex's
// sorted-key form. Pinned by a golden test against codex 0.139.0.
func codexHookTrustedHash(command string) string {
	type handler struct {
		Async   bool   `json:"async"`
		Command string `json:"command"`
		Timeout uint64 `json:"timeout"`
		Type    string `json:"type"`
	}
	type identity struct {
		EventName string    `json:"event_name"`
		Hooks     []handler `json:"hooks"`
	}
	b, _ := json.Marshal(identity{
		EventName: "permission_request",
		Hooks:     []handler{{Async: false, Command: command, Timeout: codexHookTimeoutSec, Type: "command"}},
	})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// codexHookBlock renders the marker-delimited block: the PermissionRequest hook
// registration (matcher omitted = all tools) plus trustState (the optional
// [hooks.state."key"] sub-table, or "" to leave trusting to the user).
func codexHookBlock(cmd, trustState string) string {
	return codexHookBegin + "\n" +
		"[[hooks.PermissionRequest]]\n" +
		"[[hooks.PermissionRequest.hooks]]\n" +
		"type = \"command\"\n" +
		"command = " + tomlBasicString(cmd) + "\n" +
		fmt.Sprintf("timeout = %d\n", codexHookTimeoutSec) +
		trustState +
		codexHookEnd + "\n"
}

// stripCodexBlock removes a previously-installed usher block (between the
// markers, inclusive) from TOML content, leaving the rest untouched.
func stripCodexBlock(content string) string {
	i := strings.Index(content, codexHookBegin)
	if i < 0 {
		return content
	}
	head := strings.TrimRight(content[:i], "\n ")
	end := len(content)
	if k := strings.Index(content[i:], codexHookEnd); k >= 0 {
		end = i + k + len(codexHookEnd)
		for end < len(content) && content[end] == '\n' {
			end++
		}
	}
	tail := content[end:]
	switch {
	case head == "":
		return tail
	case tail == "":
		return head + "\n"
	default:
		return head + "\n\n" + tail
	}
}

// hasBareTable reports whether content has a line that is exactly table (a
// single-bracket TOML table header), ignoring our own [[…]] array-of-tables.
func hasBareTable(content, table string) bool {
	for _, ln := range strings.Split(content, "\n") {
		if strings.TrimSpace(ln) == table {
			return true
		}
	}
	return false
}

// tomlBasicString quotes s as a TOML basic string (double-quoted, backslash and
// quote escaped) — enough for an executable path + command.
func tomlBasicString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func writeCodexConfig(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

const usherHookCmdSuffix = " hook PreToolUse"

// stripUsherEntries returns the slice with any matcher group whose only
// command is usher's hook removed; groups containing both ours and other
// hooks have only ours filtered out.
func stripUsherEntries(groups []any) []any {
	var kept []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		inner, _ := gm["hooks"].([]any)
		var keptInner []any
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			cmd, _ := hm["command"].(string)
			if isUsherHookCmd(cmd) {
				continue
			}
			keptInner = append(keptInner, h)
		}
		if len(keptInner) == 0 {
			continue
		}
		gm["hooks"] = keptInner
		kept = append(kept, gm)
	}
	return kept
}

func isUsherHookCmd(cmd string) bool {
	if cmd == "" {
		return false
	}
	// Tolerate optional leading env var assignments. Both the current
	// USHER_HOOK_SOCK and the historical USHER_ADDR get stripped by
	// suffix match alone, so re-running setup cleans up old entries.
	return strings.HasSuffix(strings.TrimSpace(cmd), usherHookCmdSuffix)
}

func readSettings(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s == nil {
		s = map[string]any{}
	}
	return s, nil
}

func writeSettings(path string, s map[string]any) error {
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
