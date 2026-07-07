package main

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/imutil"
)

// Cards use the JSON 2.0 schema (Feishu client 7.20+; older clients see the
// title plus an upgrade hint). 2.0 buys the full markdown dialect — code
// blocks in permission prompts render as real fences, and a future rich-text
// mirror gets headings/quotes/tables — at the cost of the retired 1.0
// "action" container: buttons carry behaviors and are laid out directly.

// Decision kinds carried in a card button's callback value, mirroring the
// telegram callback_data codec: a=allow once, s=allow for session, d=deny,
// i=ignore (an AskUserQuestion deny), q=ask option (with an "opt" index).
type decisionValue struct {
	Kind string `json:"k"`
	ID   string `json:"id"`
	Opt  string `json:"opt,omitempty"`
}

// decodeDecision maps a card action's value payload to a hook.Response,
// mirroring telegram's decodeDecision.
func decodeDecision(v decisionValue) (behavior, scope string, ok bool) {
	switch v.Kind {
	case "a":
		return "allow", "", true
	case "s":
		return "allow", "session", true
	case "d", "i":
		return "deny", "", true
	default:
		return "", "", false
	}
}

// parseActionValue extracts a decisionValue from the untyped map a card
// callback carries.
func parseActionValue(m map[string]any) (decisionValue, bool) {
	var v decisionValue
	data, err := json.Marshal(m)
	if err != nil {
		return v, false
	}
	if err := json.Unmarshal(data, &v); err != nil || v.Kind == "" || v.ID == "" {
		return v, false
	}
	return v, true
}

// --- card JSON -------------------------------------------------------------

// obj/arr keep the literal card structures readable. Builders return the
// object form: message sends marshal it (cardJSON), and card-callback
// responses embed it directly as the replacement card.
type obj = map[string]any
type arr = []any

// plainText renders untrusted (model-controlled) text: as a header title,
// button label, or inside a div, plain_text is inert — prompt text can't
// inject markup like <at id=all>.
func plainText(s string) obj { return obj{"tag": "plain_text", "content": s} }

// textDiv wraps plain text as a body element: 2.0 accepts plain_text only
// nested inside a div (or as header/button text), never as a bare element.
func textDiv(s string) obj {
	return obj{"tag": "div", "text": plainText(s)}
}

// markdownEl renders usher's own trusted markup (hints, mentions, fences).
func markdownEl(md string) obj {
	return obj{"tag": "markdown", "content": md}
}

// fencedEl renders untrusted text as a markdown code block. A ``` inside the
// text would close the fence and let the rest run as markup, so it is
// defanged first.
func fencedEl(s string) obj {
	s = strings.ReplaceAll(s, "```", "'''")
	return markdownEl("```\n" + s + "\n```")
}

func button(label, style string, v decisionValue) obj {
	return obj{
		"tag":  "button",
		"text": plainText(label),
		"type": style,
		"behaviors": arr{
			obj{"type": "callback", "value": v},
		},
	}
}

// buttonRow lays buttons out side by side (2.0 retired the "action"
// container; a column_set with auto-width columns replaces it).
func buttonRow(buttons ...obj) obj {
	cols := make(arr, 0, len(buttons))
	for _, b := range buttons {
		cols = append(cols, obj{"tag": "column", "width": "auto", "elements": arr{b}})
	}
	return obj{"tag": "column_set", "columns": cols}
}

func card(header, template string, elements arr) obj {
	return obj{
		"schema": "2.0",
		"header": obj{"title": plainText(header), "template": template},
		"body":   obj{"elements": elements},
	}
}

// rootCard renders a session's thread anchor: the title in the header so a
// rename can be patched in place (a text root would be stale forever — most
// sessions get their AI title only after the thread already exists), with the
// cwd and the backend/short-id line as the body.
func rootCard(title, cwd, meta string) obj {
	header := obj{
		"title":    plainText(imutil.Truncate(title, 120)),
		"template": "turquoise",
		"icon":     obj{"tag": "standard_icon", "token": "robot_outlined"},
	}
	var elements arr
	if cwd != "" {
		elements = append(elements, textDiv("CWD: "+imutil.Truncate(cwd, 300)))
	}
	elements = append(elements, textDiv(meta))
	return obj{
		"schema": "2.0",
		"header": header,
		"body":   obj{"elements": elements},
	}
}

// cardJSON renders a card object as message content.
func cardJSON(c obj) string {
	data, err := json.Marshal(c)
	if err != nil {
		return `{"schema":"2.0","body":{"elements":[]}}`
	}
	return string(data)
}

// mentionMD renders inline @-mentions of the whitelisted users, so blocking
// prompts (permission / question) notify them. Empty when no whitelist is
// configured.
func mentionMD(openIDs []string) string {
	var md string
	for _, id := range openIDs {
		md += ` <at id=` + id + `></at>`
	}
	return md
}

// permissionCard renders a pending permission interaction as an interactive
// card: tool name in the header, its input in a code block, allow/deny
// buttons. resolved != "" renders the post-decision state instead: the
// outcome line, no buttons (so a stale card can't be re-tapped).
func permissionCard(p hook.Pending, mentions []string, resolved string) obj {
	header := "🔐 Permission requested"
	if p.ToolName != "" {
		header += ": " + imutil.Truncate(p.ToolName, 80)
	}
	var elements arr
	if summary := imutil.CompactInput(p.ToolInput); summary != "" {
		elements = append(elements, fencedEl(imutil.Truncate(summary, 600)))
	}
	if resolved != "" {
		elements = append(elements, textDiv(resolved))
		return card(header, "grey", elements)
	}
	if md := mentionMD(mentions); md != "" {
		elements = append(elements, markdownEl(md))
	}
	elements = append(elements,
		buttonRow(
			button("✅ Allow", "primary", decisionValue{Kind: "a", ID: p.ID}),
			button("⛔ Deny", "danger", decisionValue{Kind: "d", ID: p.ID}),
		),
		buttonRow(
			button("✅ Allow for session", "default", decisionValue{Kind: "s", ID: p.ID}),
		),
	)
	return card(header, "orange", elements)
}

// askCard renders an AskUserQuestion. A single-select question gets one
// button per option plus Ignore; multiSelect / free-form questions list the
// options and are answered by typing in the thread. resolved != "" renders
// the answered state without buttons.
func askCard(q imutil.AskQuestion, pendingID string, mentions []string, resolved string) obj {
	header := "❓ " + imutil.Truncate(q.Question, 150)
	if q.Header != "" {
		header = "❓ " + imutil.Truncate(q.Header, 150)
	}
	var elements arr
	if q.Header != "" {
		elements = append(elements, textDiv(imutil.Truncate(q.Question, 800)))
	}
	if resolved != "" {
		elements = append(elements, textDiv(resolved))
		return card(header, "grey", elements)
	}
	if md := mentionMD(mentions); md != "" {
		elements = append(elements, markdownEl(md))
	}
	if !q.MultiSelect && len(q.Options) > 0 {
		elements = append(elements, markdownEl("*tap an option, or type your answer in this thread*"))
		// One option per row, telegram-style: labels can be long.
		for i, o := range q.Options {
			elements = append(elements, button(imutil.Truncate(o.Label, 60), "default",
				decisionValue{Kind: "q", ID: pendingID, Opt: strconv.Itoa(i)}))
		}
		elements = append(elements, button("🚫 Ignore", "danger", decisionValue{Kind: "i", ID: pendingID}))
		return card(header, "blue", elements)
	}
	hint := "*reply in this thread with your answer*"
	if q.MultiSelect {
		hint = "*reply in this thread with your answer (comma-separated for multiple)*"
	}
	if len(q.Options) > 0 {
		var opts []string
		for _, o := range q.Options {
			opts = append(opts, o.Label)
		}
		elements = append(elements, textDiv("options: "+imutil.Truncate(strings.Join(opts, ", "), 600)))
	}
	elements = append(elements,
		markdownEl(hint),
		button("🚫 Ignore", "danger", decisionValue{Kind: "i", ID: pendingID}),
	)
	return card(header, "blue", elements)
}

// multiStepCard tells the user a multi-question prompt needs the web UI (a
// single typed reply can't answer several questions), with Ignore to skip.
func multiStepCard(pendingID string, mentions []string, resolved string) obj {
	header := "🔢 Multi-step question"
	elements := arr{textDiv("Please answer in the web UI.")}
	if resolved != "" {
		elements = append(elements, textDiv(resolved))
		return card(header, "grey", elements)
	}
	if md := mentionMD(mentions); md != "" {
		elements = append(elements, markdownEl(md))
	}
	elements = append(elements, button("🚫 Ignore", "danger", decisionValue{Kind: "i", ID: pendingID}))
	return card(header, "blue", elements)
}

// resolvedCard re-renders the card for a decided interaction: same body, an
// outcome line, no buttons. Used as the card-callback replacement so a stale
// card can't be re-tapped.
func resolvedCard(p hook.Pending, outcome string) obj {
	if p.ToolName == "AskUserQuestion" {
		if qs := imutil.ParseQuestions(p.ToolInput); len(qs) == 1 {
			return askCard(qs[0], p.ID, nil, outcome)
		}
		return multiStepCard(p.ID, nil, outcome)
	}
	return permissionCard(p, nil, outcome)
}
