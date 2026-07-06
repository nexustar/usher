package main

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/imutil"
)

// Decision kinds carried in a card button's value payload, mirroring the
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

func plainText(s string) obj { return obj{"tag": "plain_text", "content": s} }

// textDiv renders untrusted (model-controlled) text: plain_text is inert, so
// prompt text can't inject lark_md markup like <at id=all>.
func textDiv(s string) obj {
	return obj{"tag": "div", "text": plainText(s)}
}

// mdDiv renders usher's own trusted markup (hints, mentions).
func mdDiv(md string) obj {
	return obj{"tag": "div", "text": obj{"tag": "lark_md", "content": md}}
}

// fencedDiv renders untrusted text as a lark_md code block. A ``` inside the
// text would close the fence and let the rest run as markup, so it is
// defanged first.
func fencedDiv(s string) obj {
	s = strings.ReplaceAll(s, "```", "'''")
	return mdDiv("```\n" + s + "\n```")
}

func button(label, style string, v decisionValue) obj {
	return obj{
		"tag":   "button",
		"text":  plainText(label),
		"type":  style,
		"value": v,
	}
}

func card(header, template string, elements arr) obj {
	return obj{
		"config":   obj{"wide_screen_mode": true},
		"header":   obj{"title": plainText(header), "template": template},
		"elements": elements,
	}
}

// cardJSON renders a card object as message content.
func cardJSON(c obj) string {
	data, err := json.Marshal(c)
	if err != nil {
		return `{"elements":[]}`
	}
	return string(data)
}

// mentionMD renders inline @-mentions of the whitelisted users for lark_md
// content, so blocking prompts (permission / question) notify them. Empty when
// no whitelist is configured.
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
		elements = append(elements, fencedDiv(imutil.Truncate(summary, 600)))
	}
	if resolved != "" {
		elements = append(elements, textDiv(resolved))
		return card(header, "grey", elements)
	}
	if md := mentionMD(mentions); md != "" {
		elements = append(elements, mdDiv(md))
	}
	elements = append(elements, obj{"tag": "action", "actions": arr{
		button("✅ Allow", "primary", decisionValue{Kind: "a", ID: p.ID}),
		button("⛔ Deny", "danger", decisionValue{Kind: "d", ID: p.ID}),
		button("✅ Allow for session", "default", decisionValue{Kind: "s", ID: p.ID}),
	}})
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
		elements = append(elements, mdDiv(md))
	}
	if !q.MultiSelect && len(q.Options) > 0 {
		var buttons arr
		for i, o := range q.Options {
			buttons = append(buttons, button(imutil.Truncate(o.Label, 60), "default",
				decisionValue{Kind: "q", ID: pendingID, Opt: strconv.Itoa(i)}))
		}
		buttons = append(buttons, button("🚫 Ignore", "danger", decisionValue{Kind: "i", ID: pendingID}))
		elements = append(elements,
			mdDiv("*tap an option, or type your answer in this thread*"),
			obj{"tag": "action", "actions": buttons})
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
		mdDiv(hint),
		obj{"tag": "action", "actions": arr{
			button("🚫 Ignore", "danger", decisionValue{Kind: "i", ID: pendingID}),
		}})
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
		elements = append(elements, mdDiv(md))
	}
	elements = append(elements, obj{"tag": "action", "actions": arr{
		button("🚫 Ignore", "danger", decisionValue{Kind: "i", ID: pendingID}),
	}})
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
