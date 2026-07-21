package backend

import "testing"

func TestParseSlashCommand(t *testing.T) {
	tests := []struct {
		text, command, args string
		ok                  bool
	}{
		{text: "/compact", command: "/compact", ok: true},
		{text: "/review check auth", command: "/review", args: "check auth", ok: true},
		{text: "/review\ncheck auth", command: "/review", args: "check auth", ok: true},
		{text: "/review\u00a0check", command: "/review", args: "check", ok: true},
		{text: "/review\u0085check", command: "/review", args: "check", ok: true},
		{text: "/review\u1680check", command: "/review", args: "check", ok: true},
		{text: "/review\u3000check", command: "/review", args: "check", ok: true},
		{text: "/codex:rescue go", command: "/codex:rescue", args: "go", ok: true},
		{text: "/skill:brave-search", command: "/skill:brave-search", ok: true},
		{text: "//not-a-command"},
		{text: "normal prompt"},
		{text: " /compact"},
		// A name is required, so a bare sigil is prose.
		{text: "/"},
		{text: "/ compact"},
		// Interior slash: a pasted path, not a command. These must reach the
		// backend as ordinary prompts instead of failing as unknown commands.
		{text: "/home/dev/x.go is broken"},
		{text: "/usr/lib"},
		{text: "/etc/hosts"},
	}
	for _, tt := range tests {
		command, args, ok := ParseSlashCommand(tt.text)
		if command != tt.command || args != tt.args || ok != tt.ok {
			t.Errorf("ParseSlashCommand(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.text, command, args, ok, tt.command, tt.args, tt.ok)
		}
	}
}
