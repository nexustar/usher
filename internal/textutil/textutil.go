// Package textutil holds the small string helpers shared by usher's three
// transcript parsers and the surfaces that render them. Fence and ClampBody in
// particular were copied verbatim into each backend parser, so a fix to the
// backtick-escaping or the size caps only landed in one of three places.
package textutil

import "strings"

// Truncate cuts s to n runes, marking the cut with an ellipsis.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// FirstLine returns s up to its first newline, trimmed.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// ShortID abbreviates a session/permission uuid for display.
func ShortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// Fence wraps body in a markdown code fence whose backtick run is widened past
// any run inside body, so a payload containing ``` cannot close the block
// early. lang may be empty.
func Fence(lang, body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	ticks := strings.Repeat("`", max(3, longest+1))
	return ticks + lang + "\n" + body + "\n" + ticks
}

// ClampBody caps a tool body so one huge file or output cannot bloat the
// transcript payload. Generous, because the block is collapsed by default.
func ClampBody(s string) string {
	const maxBytes = 32 * 1024
	const maxLines = 400
	if len(s) > maxBytes {
		s = s[:maxBytes] + "\n… (truncated)"
	}
	if lines := strings.Split(s, "\n"); len(lines) > maxLines {
		s = strings.Join(append(lines[:maxLines], "… (truncated)"), "\n")
	}
	return s
}
