package backend

import (
	"strings"
	"unicode"
)

// ParseSlashCommand splits a command token in column zero from its arguments.
// A double slash escapes command handling and remains ordinary prompt text.
//
// The name must be non-empty and free of interior slashes. Backends namespace
// commands with ':' (codex:rescue, skill:brave-search) and never with '/', so a
// token like "/home/dev/x.go" is a pasted absolute path opening a prompt about
// a file — reading it as a command would reject the whole prompt as unknown.
func ParseSlashCommand(text string) (command, args string, ok bool) {
	if !strings.HasPrefix(text, "/") || strings.HasPrefix(text, "//") {
		return "", "", false
	}
	command = text
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		command, args = text[:i], strings.TrimSpace(text[i:])
	}
	if name := command[1:]; name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return command, args, true
}
