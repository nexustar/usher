package agentprofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/core"
)

func TestLoadAndResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	err := os.WriteFile(path, []byte(`{"agents":[
		{"name":"dev","cwd":"/work/dev","backend":"codex","model":"gpt-test","append_system_prompt":"Be careful."}
	]}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	override := "/work/other"
	got, err := store.Resolve("dev", override, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := core.CreateOptions{
		Backend: "codex", Cwd: override, Model: "gpt-test",
		AppendSystemPrompt: "Be careful.",
	}
	if got != want {
		t.Fatalf("Resolve = %+v, want %+v", got, want)
	}
}

func TestResolveUnknown(t *testing.T) {
	_, err := New(nil).Resolve("missing", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("Resolve error = %v", err)
	}
}

func TestResolveWithoutAgentPassesRequestThrough(t *testing.T) {
	got, err := New(nil).Resolve("", "/w", "codex", "gpt-test")
	want := core.CreateOptions{Backend: "codex", Cwd: "/w", Model: "gpt-test"}
	if err != nil || got != want {
		t.Fatalf("Resolve = %+v, %v, want %+v", got, err, want)
	}
}

// "default" must beat a configured profile model, then normalize to the
// backend's own default rather than reaching a backend as a literal model.
func TestResolveDefaultModelOverridesProfile(t *testing.T) {
	store := New([]Profile{{Name: "dev", Model: "specific"}})
	got, err := store.Resolve("dev", "", "", "default")
	if err != nil || got.Model != "" {
		t.Fatalf("Resolve default model = %q, %v", got.Model, err)
	}
}

func TestLoadRejectsDuplicateName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	err := os.WriteFile(path, []byte(`{"agents":[
		{"name":"same"},{"name":"same"}
	]}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate agent name") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestCRUDPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(Profile{Name: "dev", Backend: "codex"})
	if err != nil || created.Name != "dev" {
		t.Fatalf("Create = %+v, %v", created, err)
	}
	updated, err := store.Update("dev", Profile{
		Backend: "claude", AppendSystemPrompt: "Be concise.",
	})
	if err != nil || updated.Name != "dev" || updated.Backend != "claude" {
		t.Fatalf("Update = %+v, %v", updated, err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.List()
	if len(got) != 1 || got[0] != updated {
		t.Fatalf("reloaded = %+v, want %+v", got, updated)
	}
	if err := reloaded.Delete("dev"); err != nil {
		t.Fatal(err)
	}
	empty, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.List(); len(got) != 0 {
		t.Fatalf("after delete = %+v", got)
	}
}

func TestCRUDValidation(t *testing.T) {
	store, err := Load(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Rejected because some surface the name travels through would mangle it.
	for _, bad := range []string{
		"",          // nothing to address it by
		"bad name",  // chat clients cut --agent at the space
		"tab\tname", // any whitespace, not just the ASCII space
		"zero​wid",  // invisible: two names that look identical
		"a/b",       // path segment in /api/agents/{name}
		"..",        // ditto — proxies rewrite it before usher sees it
	} {
		if _, err := store.Create(Profile{Name: bad}); err == nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
	// Anything else is the user's business: leading punctuation, CJK, accents.
	for _, ok := range []string{"-lead", "前端", "café"} {
		if _, err := store.Create(Profile{Name: ok}); err != nil {
			t.Fatalf("name %q rejected: %v", ok, err)
		}
	}
	if _, err := store.Create(Profile{Name: "OK"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Profile{Name: "OK"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	// Case is significant: no folding on the way in or on the way out.
	if _, err := store.Create(Profile{Name: "ok"}); err != nil {
		t.Fatalf("name differing only by case rejected: %v", err)
	}
	if _, err := store.Resolve("Ok", "", "", ""); err == nil {
		t.Fatal("resolve matched a different case")
	}
	if _, err := store.Update("missing", Profile{}); err == nil {
		t.Fatal("missing update accepted")
	}
	if err := store.Delete("missing"); err == nil {
		t.Fatal("missing delete accepted")
	}
}

func TestUpdateRenames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Profile{Name: "typoo", Backend: "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Profile{Name: "taken"}); err != nil {
		t.Fatal(err)
	}
	renamed, err := store.Update("typoo", Profile{Name: "Typo", Backend: "codex"})
	if err != nil || renamed.Name != "Typo" {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	if _, err := store.Resolve("typoo", "", "", ""); err == nil {
		t.Fatal("old name still resolves after rename")
	}
	if _, err := store.Resolve("Typo", "", "", ""); err != nil {
		t.Fatalf("new name does not resolve: %v", err)
	}
	if _, err := store.Update("Typo", Profile{Name: "taken"}); err == nil {
		t.Fatal("rename onto an existing name accepted")
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.List(); len(got) != 2 || got[0].Name != "Typo" {
		t.Fatalf("persisted = %+v", got)
	}
}
