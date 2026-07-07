package main

import (
	"encoding/json"
	"strings"
)

// postMD wraps assistant markdown as post-message content: one md paragraph
// (CommonMark + GFM), which Feishu renders in a plain bubble — no card frame.
func postMD(md string) string {
	content, err := json.Marshal(map[string]any{
		"zh_cn": map[string]any{
			"content": [][]map[string]any{{{"tag": "md", "text": md}}},
		},
	})
	if err != nil {
		return `{"zh_cn":{"content":[]}}`
	}
	return string(content)
}

// quoteMD renders text as a markdown blockquote; the blank-line separation
// from what follows is the caller's job (a bare next line would be lazily
// continued into the quote).
func quoteMD(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// sanitizeMarkdown prepares assistant markdown for Feishu's markdown
// renderers (the post md paragraph; same dialect family as the card
// markdown component). The dialect is close enough to standard markdown
// that text passes through — bold, lists, headings, fences all render as
// written. The one hazard is the renderer's HTML-like extension tags:
// model-controlled text containing <at id=all>, <text_tag>, <link> etc.
// would execute as markup (an @all pierces every member's mute). Escaping
// "<" outside code disarms them all; inside fenced blocks and inline code
// spans text renders literally, so it is left untouched (an entity there
// would show verbatim).
func sanitizeMarkdown(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	inFence := false
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		if isFenceLine(line) {
			inFence = !inFence
			b.WriteString(line)
			continue
		}
		if inFence {
			b.WriteString(line)
			continue
		}
		b.WriteString(escapeOutsideCodeSpans(line))
	}
	return b.String()
}

// isFenceLine reports whether a line opens or closes a fenced code block:
// up to three spaces of indent, then three-plus backticks (CommonMark).
func isFenceLine(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	return len(line)-len(trimmed) <= 3 && strings.HasPrefix(trimmed, "```")
}

// escapeOutsideCodeSpans escapes "<" in a single line, skipping inline code
// spans: a backtick run opens a span closed by the next run of equal length
// (CommonMark); an unmatched run is literal text and gets escaped like the
// rest.
func escapeOutsideCodeSpans(line string) string {
	var b strings.Builder
	b.Grow(len(line) + 8)
	for i := 0; i < len(line); {
		if line[i] != '`' {
			if line[i] == '<' {
				b.WriteString("&#60;")
			} else {
				b.WriteByte(line[i])
			}
			i++
			continue
		}
		run := backtickRun(line[i:])
		if end := findClosingRun(line[i+run:], run); end >= 0 {
			// Code span: emit delimiters and content verbatim.
			b.WriteString(line[i : i+run+end+run])
			i += run + end + run
			continue
		}
		// Unmatched run: literal backticks, keep scanning after them.
		b.WriteString(line[i : i+run])
		i += run
	}
	return b.String()
}

// backtickRun returns the length of the backtick run at the start of s.
func backtickRun(s string) int {
	n := 0
	for n < len(s) && s[n] == '`' {
		n++
	}
	return n
}

// findClosingRun returns the offset in s of a backtick run of exactly length
// n (CommonMark: longer or shorter runs don't close the span), or -1.
func findClosingRun(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := backtickRun(s[i:])
		if run == n {
			return i
		}
		i += run
	}
	return -1
}
