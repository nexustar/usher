package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexustar/usher/pkg/mcptools"
	"github.com/nexustar/usher/pkg/pluginapi"
)

// runMCPStdio is the worker for `usher mcp-stdio`: a minimal, dependency-free
// MCP server (newline-delimited JSON-RPC 2.0 over stdin/stdout) launched by the
// backend as a child of a usher-spawned session, so it inherits USHER_HOOK_SOCK
// — the managed-session marker. By default it exposes only show_image. The
// explicit --router-tools flag adds session-management tools backed by usher's
// Unix control socket; generated backend configs carry it for the router
// session only. Unmanaged sessions get no tools.
func runMCPStdio(args []string) error {
	fs := flag.NewFlagSet("mcp-stdio", flag.ContinueOnError)
	routerTools := fs.Bool("router-tools", false,
		"expose usher session-routing tools in addition to show_image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A nil Router turns the routing tools off.
	var router *mcptools.Router
	if *routerTools {
		hookSock := os.Getenv("USHER_HOOK_SOCK")
		if hookSock == "" {
			return errors.New("--router-tools requires USHER_HOOK_SOCK from a running usher instance")
		}
		router = &mcptools.Router{
			API: pluginapi.NewClient(pluginapi.SocketPath(filepath.Dir(hookSock)), nil),
		}
	}

	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			if respErr := handleMCPLine(line, out, router); respErr != nil {
				return respErr
			}
			out.Flush()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// managed reports whether this is a usher-managed session (so we should expose
// the tool). USHER_HOOK_SOCK is the existing managed-session marker.
func mcpManaged() bool { return os.Getenv("USHER_HOOK_SOCK") != "" }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent ⇒ notification (no reply)
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// handleMCPLine parses one JSON-RPC message and writes its reply (if any). A
// nil router leaves the session-routing tools out entirely.
func handleMCPLine(line []byte, out *bufio.Writer, router *mcptools.Router) error {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return nil
	}
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		// Can't correlate without an id; drop malformed input rather than die.
		return nil
	}
	// A request with no id is a notification: act, never reply.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		return writeResult(out, req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "usher", "version": Version},
		})
	case "notifications/initialized", "notifications/cancelled":
		return nil
	case "ping":
		if isNotification {
			return nil
		}
		return writeResult(out, req.ID, map[string]any{})
	case "tools/list":
		return writeResult(out, req.ID, map[string]any{"tools": mcpTools(router)})
	case "tools/call":
		return writeToolCall(out, req.ID, req.Params, router)
	default:
		if isNotification {
			return nil
		}
		return writeError(out, req.ID, -32601, "method not found: "+req.Method)
	}
}

// mcpProtocolVersion echoes the client's requested protocol version so we never
// fight it on a handshake; falls back to a known-good default.
func mcpProtocolVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.ProtocolVersion != "" {
		return p.ProtocolVersion
	}
	return "2024-11-05"
}

func mcpTools(router *mcptools.Router) []map[string]any {
	if !mcpManaged() {
		return []map[string]any{}
	}
	tools := []map[string]any{{
		// Exempt from Tool Search deferral so the model always sees it (paired
		// with the server-level alwaysLoad in the spawned config).
		"_meta": map[string]any{"anthropic/alwaysLoad": true},
		"name":  "show_image",
		"description": "Show an image to the user. Call it whenever you have produced or found an image " +
			"the user should see (a chart, screenshot, render): saving a file or printing its path does not display it. " +
			"Supports .png, .jpg, .jpeg, .gif and .webp.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Image path, absolute or relative to the working directory.",
				},
			},
			"required": []string{"file_path"},
		},
	}}
	if router != nil {
		for _, tool := range mcptools.RouterTools() {
			b, _ := json.Marshal(tool)
			var generic map[string]any
			_ = json.Unmarshal(b, &generic)
			tools = append(tools, generic)
		}
	}
	return tools
}

// mcpImageExts mirrors the /image allowlist (raster only; see imageContentTypes
// for why SVG is excluded).
var mcpImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// writeToolCall dispatches one tool call. show_image is only validated and
// acked (it never returns bytes — rendering rides the jsonl transcript);
// everything else is a routing tool, available only when router is non-nil.
func writeToolCall(out *bufio.Writer, id json.RawMessage, params json.RawMessage, router *mcptools.Router) error {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.Name == "show_image" {
		var args struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(p.Arguments, &args)
		text, isErr := validateImage(args.FilePath)
		return writeResult(out, id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		})
	}
	if router == nil {
		return writeError(out, id, -32602, "unknown tool: "+p.Name)
	}
	value, err := router.Call(p.Name, p.Arguments)
	if err != nil {
		return writeToolResult(out, id, map[string]string{"error": err.Error()}, true)
	}
	return writeToolResult(out, id, value, false)
}

func writeToolResult(out *bufio.Writer, id json.RawMessage, value any, isErr bool) error {
	// Indented: the text is what a person reads in the tool card.
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		raw = []byte(`{"error":"failed to encode tool result"}`)
		isErr = true
	}
	result := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(raw)}},
		"isError": isErr,
	}
	// structuredContent is defined as an object; a list goes as text only.
	if len(raw) > 0 && raw[0] == '{' {
		result["structuredContent"] = value
	}
	return writeResult(out, id, result)
}

// validateImage resolves the path against the process cwd (= the session cwd on
// both backends) and checks it's an existing image file. On success it returns a
// JSON string with a `message` plus the image's `w`/`h` (header-only read) so the
// UI can reserve layout space; errors return a plain advisory message. The bool
// is the MCP isError flag. JSON-in-text (vs structuredContent) is the channel
// that survives into both backends' transcript jsonl for usher to parse.
func validateImage(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "show_image: file_path is required", true
	}
	abs := raw
	if !filepath.IsAbs(abs) {
		cwd, err := os.Getwd()
		if err != nil {
			return "show_image: cannot resolve working directory: " + err.Error(), true
		}
		abs = filepath.Join(cwd, abs)
	}
	abs = filepath.Clean(abs)

	ext := strings.ToLower(filepath.Ext(abs))
	if !mcpImageExts[ext] {
		return fmt.Sprintf("show_image: %q is not a supported image type (.png, .jpg, .jpeg, .gif, .webp)", raw), true
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Sprintf("show_image: file not found: %s", raw), true
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("show_image: not a regular file: %s", raw), true
	}

	payload := map[string]any{
		"message": fmt.Sprintf("Showing %s to the user.", filepath.Base(abs)),
	}
	// Dimensions are best-effort (webp isn't in stdlib → degrades to no dims).
	if w, h := imageDims(abs); w > 0 && h > 0 {
		payload["w"] = w
		payload["h"] = h
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("Showing %s to the user.", filepath.Base(abs)), false
	}
	return string(b), false
}

// imageDims reads just the image header to get its pixel dimensions, or (0,0)
// if the format isn't decodable (e.g. webp) or the file can't be read.
func imageDims(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// --- JSON-RPC framing ----------------------------------------------------

func writeResult(out *bufio.Writer, id json.RawMessage, result any) error {
	return writeRPC(out, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"result":  result,
	})
}

func writeError(out *bufio.Writer, id json.RawMessage, code int, msg string) error {
	return writeRPC(out, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"error":   map[string]any{"code": code, "message": msg},
	})
}

func writeRPC(out *bufio.Writer, msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = out.Write(b)
	return err
}

func rawOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}
