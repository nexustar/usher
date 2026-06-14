package main

import (
	"strings"
	"testing"
)

func TestStripCodexBlock_RoundTrip(t *testing.T) {
	user := "[projects.\"/home/dev\"]\ntrust_level = \"trusted\"\n"
	block := codexHookBlock("/usr/bin/usher hook PermissionRequest", "\n[hooks.state.\"k\"]\ntrusted_hash = \"sha256:abc\"\n")

	// Install = append the block after the user's config.
	installed := strings.TrimRight(user, "\n") + "\n\n" + block
	if !strings.Contains(installed, "[[hooks.PermissionRequest]]") {
		t.Fatal("install did not contain the hook table")
	}

	// Strip must return exactly the user's config (block fully removed).
	got := stripCodexBlock(installed)
	if got != user {
		t.Fatalf("round-trip mismatch:\n got: %q\nwant: %q", got, user)
	}

	// Re-stripping is a no-op (idempotent).
	if again := stripCodexBlock(got); again != got {
		t.Errorf("second strip changed content: %q", again)
	}
}

func TestStripCodexBlock_NoBlock(t *testing.T) {
	content := "model = \"gpt-5.5\"\n"
	if got := stripCodexBlock(content); got != content {
		t.Errorf("no-block strip altered content: %q", got)
	}
}

func TestStripCodexBlock_MalformedNoEnd(t *testing.T) {
	// A begin marker with no end (hand-mangled) cuts from the marker on.
	content := "keep = 1\n\n" + codexHookBegin + "\n[[hooks.PermissionRequest]]\n"
	got := stripCodexBlock(content)
	if strings.Contains(got, codexHookBegin) {
		t.Errorf("malformed block not removed: %q", got)
	}
	if !strings.Contains(got, "keep = 1") {
		t.Errorf("dropped preceding content: %q", got)
	}
}

func TestHasBareTable(t *testing.T) {
	// Single-table form is detected (would collide with our [[...]] append).
	if !hasBareTable("[hooks.PermissionRequest]\n", "[hooks.PermissionRequest]") {
		t.Error("did not detect bare single-table header")
	}
	// Our own array-of-tables must NOT be mistaken for the bare table.
	if hasBareTable("[[hooks.PermissionRequest]]\n", "[hooks.PermissionRequest]") {
		t.Error("array-of-tables wrongly flagged as bare table")
	}
}

func TestTomlBasicString(t *testing.T) {
	if got := tomlBasicString(`/usr/bin/usher hook PermissionRequest`); got != `"/usr/bin/usher hook PermissionRequest"` {
		t.Errorf("plain path: %q", got)
	}
	if got := tomlBasicString(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("escapes: %q", got)
	}
}

func TestCodexHookTrustedHash_MatchesCodex(t *testing.T) {
	// Golden value: codex 0.139.0's own persisted trusted_hash for this exact
	// command (captured via its /hooks "Trust all" flow). Locks our canonical
	// JSON to Codex's. If this breaks, Codex changed the hashed hook identity.
	const cmd = "/tmp/usher-trust hook PermissionRequest"
	const want = "sha256:4964a28b890c7a8358f72cf3641330d2c3feae4014987c0e5f7e6eed618ea92e"
	if got := codexHookTrustedHash(cmd); got != want {
		t.Errorf("codexHookTrustedHash mismatch:\n got: %s\nwant: %s", got, want)
	}
}
