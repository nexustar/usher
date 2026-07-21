package pi

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nexustar/usher/internal/backend"
)

func TestModelsMissingCache(t *testing.T) {
	m := Models{Path: filepath.Join(t.TempDir(), "missing.json")}
	models, err := m.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if models != nil {
		t.Fatalf("Models() = %#v, want nil", models)
	}
}

func TestModelsPersistsCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "pi-models.json")
	want := []backend.Model{{
		ID:             "openai-codex/gpt-5.4",
		DisplayName:    "GPT-5.4",
		ThinkingLevels: []string{"off", "high"},
	}}
	if err := (Models{Path: path}).write(want); err != nil {
		t.Fatal(err)
	}

	// A fresh value must recover the catalog from disk; there is no memory cache.
	got, err := (Models{Path: path}).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != want[0].ID || got[0].DisplayName != want[0].DisplayName {
		t.Fatalf("Models() = %#v, want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestModelsFromRPC(t *testing.T) {
	raw := []byte(`{"models":[
		{"provider":"openai-codex","id":"gpt-5.4","name":"GPT-5.4","reasoning":true},
		{"provider":"google","id":"gemini-flash","name":"","reasoning":false},
		{"provider":"","id":"invalid"}
	]}`)
	got, err := modelsFromRPC(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("models = %#v, want 2 entries", got)
	}
	if got[0].ID != "openai-codex/gpt-5.4" || len(got[0].ThinkingLevels) == 0 {
		t.Fatalf("reasoning model = %#v", got[0])
	}
	if got[1].ID != "google/gemini-flash" || got[1].DisplayName != "gemini-flash" {
		t.Fatalf("fallback name model = %#v", got[1])
	}
}

func TestComposerItemsFromRPC(t *testing.T) {
	raw := []byte(`{"commands":[
		{"name":"deploy","description":"Deploy the app","source":"extension"},
		{"name":"fix-tests","description":"Fix failing tests","source":"prompt"},
		{"name":"skill:brave-search","description":"Search the web","source":"skill"},
		{"name":"","description":"invalid","source":"skill"}
	]}`)
	got, err := composerItemsFromRPC(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []backend.ComposerItem{
		{Name: "deploy", Kind: "extension", Description: "Deploy the app"},
		{Name: "fix-tests", Kind: "prompt", Description: "Fix failing tests"},
		{Name: "skill:brave-search", Kind: "skill", Description: "Search the web"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("composer items = %#v, want %#v", got, want)
	}
}

func TestComposerItemsDoesNotStartColdWorker(t *testing.T) {
	r := &Runtime{workers: map[string]*worker{}}
	got, err := r.ComposerItems(context.Background(), "cold-session", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if got.Items != nil || got.Available || len(r.workers) != 0 {
		t.Fatalf("cold composer lookup = %#v, workers = %d", got, len(r.workers))
	}
}

func TestValidatePiCommandAllowsDoubleSlashPrompt(t *testing.T) {
	// Pi's harness treats // as ordinary prompt text. A nil client proves the
	// validator returns before attempting command discovery.
	if err := validatePiCommand(context.Background(), nil, "//not-a-command"); err != nil {
		t.Fatalf("double-slash prompt rejected: %v", err)
	}
}
