package telegram

import (
	"encoding/json"
	"html"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/hook"
)

// telegramMaxMessage is the Bot API per-message text limit (in UTF-16 code
// units; we approximate with runes, which is conservative for BMP text).
const telegramMaxMessage = 4096

// encodeDecision/decodeDecision map a button to compact callback_data of the
// form "<kind>:<pendingID>" (kind a=allow once, d=deny, s=allow for session,
// i=ignore), well within Telegram's 64-byte callback_data limit.
func encodeDecision(kind, pendingID string) string { return kind + ":" + pendingID }

func decodeDecision(data string) (behavior, scope, id string, ok bool) {
	kind, id, found := strings.Cut(data, ":")
	if !found || id == "" {
		return "", "", "", false
	}
	switch kind {
	case "a":
		return "allow", "", id, true
	case "s":
		return "allow", "session", id, true
	case "d":
		return "deny", "", id, true
	case "i":
		// "ignore" on an AskUserQuestion: a deny under the hood (claude skips
		// the question and continues), matching the web UI's wording.
		return "deny", "", id, true
	default:
		return "", "", "", false
	}
}

// permissionHTML renders a concise prompt body for a pending interaction as
// Telegram HTML: the tool name in bold and its input (Bash command / compact
// JSON) in a <pre> monospace block so a long command stays readable.
func permissionHTML(p hook.Pending) string {
	var b strings.Builder
	b.WriteString("🔐 <b>Permission requested</b>")
	if p.ToolName != "" {
		b.WriteString(": ")
		b.WriteString(html.EscapeString(p.ToolName))
	}
	if summary := compactInput(p.ToolInput); summary != "" {
		b.WriteString("\n<pre>")
		b.WriteString(html.EscapeString(truncate(summary, 600)))
		b.WriteString("</pre>")
	}
	return b.String()
}

// compactInput flattens a tool input to a one-or-few-line summary. Bash shows
// its command; everything else shows compact JSON.
func compactInput(raw json.RawMessage) string {
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

// turnDuration reads the elapsed time of a turn from a subprocess.exit event,
// using the user_ts / assistant_ts the router stamps onto it. Returns false
// when either timestamp is missing (best-effort enrichment) or out of order.
func turnDuration(raw json.RawMessage) (time.Duration, bool) {
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

// humanizeDuration renders a turn duration compactly (e.g. "8s", "1m12s").
func humanizeDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return d.Round(time.Second).String()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// extractUserText flattens a user message's prompt text. Content may be a plain
// string or an array of blocks; only text blocks contribute, so a tool_result
// user event (tool output fed back to claude) yields "" and is not echoed.
func extractUserText(raw json.RawMessage) string {
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
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(line.Message.Content, &blocks); err != nil {
		return ""
	}
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

// assistantText flattens the text blocks of an assistant jsonl line. tool_use
// and thinking blocks contribute nothing, so a tool-only message yields "".
func assistantText(raw json.RawMessage) string {
	var line struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range line.Message.Content {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// splitMessage breaks text into <=telegramMaxMessage chunks, preferring to cut
// at a newline boundary so code/paragraphs stay intact.
func splitMessage(text string) []string {
	runes := []rune(text)
	if len(runes) <= telegramMaxMessage {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		n := telegramMaxMessage
		if n > len(runes) {
			n = len(runes)
		} else if idx := lastNewline(runes[:n]); idx > telegramMaxMessage/2 {
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

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
