// Package imutil holds helpers shared by usher's IM frontends (the in-process
// Telegram hub and out-of-process IM plugins): flattening session jsonl events
// into plain text, and IM-agnostic text utilities.
package imutil

import (
	"encoding/json"
	"strings"
	"time"
)

// ExtractUserText flattens a user message's prompt text. Content may be a plain
// string or an array of blocks; only text blocks contribute, so a tool_result
// user event (tool output fed back to claude) yields "" and is not echoed.
func ExtractUserText(raw json.RawMessage) string {
	var line struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(line.Message.Content, &s); err == nil {
		return s
	}
	var blocks []textBlock
	if err := json.Unmarshal(line.Message.Content, &blocks); err != nil {
		return ""
	}
	return joinTextBlocks(blocks)
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// joinTextBlocks concatenates the text blocks with blank lines between them;
// non-text blocks contribute nothing.
func joinTextBlocks(blocks []textBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// AssistantText flattens the text blocks of an assistant jsonl line. tool_use
// and thinking blocks contribute nothing, so a tool-only message yields "".
func AssistantText(raw json.RawMessage) string {
	var line struct {
		Message struct {
			Content []textBlock `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return ""
	}
	return joinTextBlocks(line.Message.Content)
}

// ImageExts mirrors the show_image allowlist (cmd/usher/mcpcmd.go mcpImageExts
// and web's imageContentTypes): raster only, SVG excluded.
var ImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// ImageRefs extracts the file paths of any show_image MCP tool calls in an
// assistant jsonl line. The tool surfaces as a tool_use block whose name ends
// in "show_image" (MCP namespaces it, e.g. mcp__usher__show_image) carrying a
// file_path input — the same shape the web UI renders as an inline image.
func ImageRefs(raw json.RawMessage) []string {
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

// AskQuestion is the subset of an AskUserQuestion question IM frontends
// render: the prompt text, its short header, and the option labels.
type AskQuestion struct {
	Header      string `json:"header"`
	Question    string `json:"question"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// ParseQuestions extracts the questions of an AskUserQuestion tool input.
func ParseQuestions(raw json.RawMessage) []AskQuestion {
	var in struct {
		Questions []AskQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	return in.Questions
}

// TurnDuration reads the elapsed time of a turn from a subprocess.exit event,
// using the user_ts / assistant_ts the router stamps onto it. Returns false
// when either timestamp is missing (best-effort enrichment) or out of order.
func TurnDuration(raw json.RawMessage) (time.Duration, bool) {
	var x struct {
		UserTS      time.Time `json:"user_ts"`
		AssistantTS time.Time `json:"assistant_ts"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return 0, false
	}
	if x.UserTS.IsZero() || x.AssistantTS.IsZero() || !x.AssistantTS.After(x.UserTS) {
		return 0, false
	}
	return x.AssistantTS.Sub(x.UserTS), true
}

// HumanizeDuration renders a turn duration compactly (e.g. "8s", "1m12s").
func HumanizeDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return d.Round(time.Second).String()
}

// CompactInput flattens a tool input to a one-or-few-line summary. Bash shows
// its command; everything else shows compact JSON.
func CompactInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asObj map[string]any
	if err := json.Unmarshal(raw, &asObj); err != nil || asObj == nil {
		return string(raw) // not a JSON object (array/scalar) → show it verbatim
	}
	if cmd, ok := asObj["command"].(string); ok {
		return cmd
	}
	compact, err := json.Marshal(asObj)
	if err != nil {
		return string(raw)
	}
	return string(compact)
}

// ShortID returns the first 8 characters of a session id.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// SplitMessage breaks text into chunks of at most max runes, preferring to cut
// at a newline boundary so code/paragraphs stay intact. A non-positive max is
// nonsense from a miscomputed limit; return the text whole rather than loop.
func SplitMessage(text string, max int) []string {
	runes := []rune(text)
	if max <= 0 || len(runes) <= max {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		n := max
		if n > len(runes) {
			n = len(runes)
		} else if idx := lastNewline(runes[:n]); idx > max/2 {
			n = idx
		}
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
		// Skip a single leading newline left at the cut point.
		if len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	return chunks
}

func lastNewline(runes []rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == '\n' {
			return i
		}
	}
	return -1
}

// Truncate caps s at max runes.
func Truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
