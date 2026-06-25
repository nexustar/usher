package telegram

import (
	"html"
	"regexp"
	"strings"
)

// Markdown → Telegram-HTML (a limited entity set: no tables/lists; headings
// become bold, keeping their # so the level shows). Italic is intentionally NOT
// handled — single * / _ false-match snake_case too often. Malformed output is
// caught by the caller's plain-text fallback, so best-effort is safe.
var (
	mdLink   = regexp.MustCompile(`\[([^\]\n]+?)\]\(([^)\s\n]+?)\)`)
	mdBold   = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	mdStrike = regexp.MustCompile(`~~([^~\n]+?)~~`)
	mdHeader = regexp.MustCompile(`(?m)^[ \t]{0,3}(#{1,6}[ \t]+.+?)[ \t]*#*$`)
)

// toTelegramHTML converts a markdown chunk to Telegram's HTML subset: ``` →
// <pre>, `inline` → <code>, and in prose **bold** / ~~strike~~ / [links] /
// # headings. Content is entity-escaped first; an unclosed ``` (split mid-fence)
// runs to the chunk end so the HTML stays balanced.
func toTelegramHTML(s string) string {
	var b strings.Builder
	rest := s
	for {
		open := strings.Index(rest, "```")
		if open < 0 {
			b.WriteString(formatProse(rest))
			break
		}
		b.WriteString(formatProse(rest[:open]))
		rest = rest[open+3:]

		// Drop an optional language tag on the opening fence line (```go).
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			if lang := rest[:nl]; lang != "" && !strings.ContainsAny(lang, " \t`") {
				rest = rest[nl+1:]
			}
		}

		closed := true
		end := strings.Index(rest, "```")
		var code string
		if end < 0 {
			code, rest, closed = rest, "", false
		} else {
			code, rest = rest[:end], rest[end+3:]
		}
		code = strings.TrimPrefix(code, "\n")
		code = strings.TrimSuffix(code, "\n")
		b.WriteString("<pre>")
		b.WriteString(html.EscapeString(code))
		b.WriteString("</pre>")
		if !closed {
			break
		}
	}
	return b.String()
}

// formatProse handles a non-code-fence segment: it protects `inline code`
// spans, then applies the prose marks to the rest.
func formatProse(s string) string {
	var b strings.Builder
	parts := strings.Split(s, "`")
	for i, p := range parts {
		if i%2 == 1 && i != len(parts)-1 {
			b.WriteString("<code>")
			b.WriteString(html.EscapeString(p))
			b.WriteString("</code>")
			continue
		}
		if i%2 == 1 {
			b.WriteByte('`') // unmatched opening backtick → literal
		}
		b.WriteString(inlineMarks(p))
	}
	return b.String()
}

// inlineMarks escapes text, then converts links / bold / strikethrough /
// headings to Telegram HTML. Order: escape first (so the only tags are the ones
// we add), links before bold (so a bold link's ** is converted inside the <a>).
func inlineMarks(s string) string {
	s = html.EscapeString(s)
	s = mdLink.ReplaceAllString(s, `<a href="$2">$1</a>`)
	s = mdBold.ReplaceAllString(s, `<b>$1</b>`)
	s = mdStrike.ReplaceAllString(s, `<s>$1</s>`)
	// Bold the whole heading line — but if it already contains a tag (e.g. a
	// bold/link span), don't wrap it again: Telegram rejects nested <b>, and the
	// inner formatting already stands out.
	s = mdHeader.ReplaceAllStringFunc(s, func(line string) string {
		m := mdHeader.FindStringSubmatch(line)
		if m == nil {
			return line
		}
		if strings.Contains(m[1], "<") {
			return m[1] // already has markup; leave as-is rather than nest <b>
		}
		return "<b>" + m[1] + "</b>"
	})
	return s
}
