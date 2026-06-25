package telegram

import (
	"encoding/json"
	"strings"
)

// imageExts mirrors the show_image allowlist (cmd/usher/mcpcmd.go mcpImageExts
// and web's imageContentTypes): raster only, SVG excluded.
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// imageRefs extracts the file paths of any show_image MCP tool calls in an
// assistant jsonl line. The tool surfaces as a tool_use block whose name ends
// in "show_image" (MCP namespaces it, e.g. mcp__usher__show_image) carrying a
// file_path input — the same shape the web UI renders as an inline image.
func imageRefs(raw json.RawMessage) []string {
	var line struct {
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					FilePath string `json:"file_path"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return nil
	}
	var out []string
	for _, b := range line.Message.Content {
		if b.Type == "tool_use" && isShowImage(b.Name) && b.Input.FilePath != "" {
			out = append(out, b.Input.FilePath)
		}
	}
	return out
}

func isShowImage(name string) bool {
	return name == "show_image" || strings.HasSuffix(name, "__show_image")
}
