package telegram

import (
	"html"
	"strings"

	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/imutil"
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
	if summary := imutil.CompactInput(p.ToolInput); summary != "" {
		b.WriteString("\n<pre>")
		b.WriteString(html.EscapeString(imutil.Truncate(summary, 600)))
		b.WriteString("</pre>")
	}
	return b.String()
}
