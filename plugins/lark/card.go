package main

import (
	"encoding/json"
	"strconv"

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

// obj/arr keep the literal card structures readable.
type obj = map[string]any
type arr = []any

func plainText(s string) obj { return obj{"tag": "plain_text", "content": s} }
func mdDiv(md string) obj {
	return obj{"tag": "div", "text": obj{"tag": "lark_md", "content": md}}
}

func button(label, style string, v decisionValue) obj {
	return obj{
		"tag":   "button",
		"text":  plainText(label),
		"type":  style,
		"value": obj{"k": v.Kind, "id": v.ID, "opt": v.Opt},
	}
}

func renderCard(header, template string, elements arr) string {
	card := obj{
		"config":   obj{"wide_screen_mode": true},
		"header":   obj{"title": plainText(header), "template": template},
		"elements": elements,
	}
	data, err := json.Marshal(card)
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
func permissionCard(p hook.Pending, mentions []string, resolved string) string {
	header := "🔐 Permission requested"
	if p.ToolName != "" {
		header += ": " + imutil.Truncate(p.ToolName, 80)
	}
	var elements arr
	if summary := imutil.CompactInput(p.ToolInput); summary != "" {
		elements = append(elements, mdDiv("```\n"+imutil.Truncate(summary, 600)+"\n```"))
	}
	if resolved != "" {
		elements = append(elements, mdDiv(resolved))
		return renderCard(header, "grey", elements)
	}
	if md := mentionMD(mentions); md != "" {
		elements = append(elements, mdDiv(md))
	}
	elements = append(elements, obj{"tag": "action", "actions": arr{
		button("✅ Allow", "primary", decisionValue{Kind: "a", ID: p.ID}),
		button("⛔ Deny", "danger", decisionValue{Kind: "d", ID: p.ID}),
		button("✅ Allow for session", "default", decisionValue{Kind: "s", ID: p.ID}),
	}})
	return renderCard(header, "orange", elements)
}

// askQuestion is the subset of an AskUserQuestion question we render.
type askQuestion struct {
	Header      string `json:"header"`
	Question    string `json:"question"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label string `json:"label"`
	} `json:"options"`
}

func parseQuestions(raw json.RawMessage) []askQuestion {
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	return in.Questions
}

// askCard renders an AskUserQuestion. A single-select question gets one
// button per option plus Ignore; multiSelect / free-form questions list the
// options and are answered by typing in the thread. resolved != "" renders
// the answered state without buttons.
func askCard(q askQuestion, pendingID string, mentions []string, resolved string) string {
	header := "❓ " + imutil.Truncate(q.Question, 150)
	if q.Header != "" {
		header = "❓ " + imutil.Truncate(q.Header, 150)
	}
	var elements arr
	if q.Header != "" {
		elements = append(elements, mdDiv(imutil.Truncate(q.Question, 800)))
	}
	if resolved != "" {
		elements = append(elements, mdDiv(resolved))
		return renderCard(header, "grey", elements)
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
		return renderCard(header, "blue", elements)
	}
	hint := "*reply in this thread with your answer*"
	if q.MultiSelect {
		hint = "*reply in this thread with your answer (comma-separated for multiple)*"
	}
	if len(q.Options) > 0 {
		var opts string
		for i, o := range q.Options {
			if i > 0 {
				opts += ", "
			}
			opts += o.Label
		}
		elements = append(elements, mdDiv("options: "+imutil.Truncate(opts, 600)))
	}
	elements = append(elements,
		mdDiv(hint),
		obj{"tag": "action", "actions": arr{
			button("🚫 Ignore", "danger", decisionValue{Kind: "i", ID: pendingID}),
		}})
	return renderCard(header, "blue", elements)
}

// multiStepCard tells the user a multi-question prompt needs the web UI (a
// single typed reply can't answer several questions), with Ignore to skip.
func multiStepCard(pendingID string, mentions []string, resolved string) string {
	header := "🔢 Multi-step question"
	var elements arr
	elements = append(elements, mdDiv("Please answer in the web UI."))
	if resolved != "" {
		elements = append(elements, mdDiv(resolved))
		return renderCard(header, "grey", elements)
	}
	if md := mentionMD(mentions); md != "" {
		elements = append(elements, mdDiv(md))
	}
	elements = append(elements, obj{"tag": "action", "actions": arr{
		button("🚫 Ignore", "danger", decisionValue{Kind: "i", ID: pendingID}),
	}})
	return renderCard(header, "blue", elements)
}
