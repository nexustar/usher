package modelcatalog

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/nexustar/usher/internal/backend"
)

func TestCodexModels(t *testing.T) {
	ctx := context.Background()
	got, err := (Codex{Path: "/no/such/models_cache.json"}).Models(ctx)
	if err != nil || len(got) != 2 || got[0].ID != "gpt-5.5" {
		t.Fatalf("missing-cache fallback = %v, %v; want gpt-5.5 then mini", got, err)
	}

	p := filepath.Join(t.TempDir(), "models_cache.json")
	if err := os.WriteFile(p, []byte(`{"models":[
		{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","priority":2,"default_reasoning_level":"low",
		 "supported_reasoning_levels":[{"effort":"low","description":"fast"},{"effort":"xhigh","description":"deep"}]},
		{"slug":"auto-review","display_name":"x","visibility":"hide","priority":1,"default_reasoning_level":"high"},
		{"slug":"gpt-5.4-mini","display_name":"GPT-5.4 Mini","visibility":"list","priority":1}
	]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := Codex{Path: p}
	got, err = catalog.Models(ctx)
	if err != nil || len(got) != 2 || got[0].ID != "gpt-5.4-mini" || got[1].ID != "gpt-5.5" {
		t.Fatalf("catalog parse/sort = %v, %v; want mini then 5.5", got, err)
	}
	if !slices.Equal(got[1].ThinkingLevels, []string{"low", "xhigh"}) || got[0].ThinkingLevels != nil {
		t.Errorf("levels = %v / %v, want [low xhigh] / none", got[1].ThinkingLevels, got[0].ThinkingLevels)
	}
	if effort, _ := catalog.DefaultEffort(ctx, "gpt-5.5"); effort != "low" {
		t.Errorf("default effort = %q, want low", effort)
	}
	if effort, _ := catalog.DefaultEffort(ctx, "auto-review"); effort != "high" {
		t.Errorf("hidden model default effort = %q, want high", effort)
	}
	if err := catalog.ValidateModel(ctx, "auto-review"); err != nil {
		t.Errorf("hidden catalog model rejected: %v", err)
	}
	if err := catalog.ValidateModel(ctx, "not-in-catalog"); err == nil {
		t.Error("unknown Codex model accepted")
	}
}

func TestClaudeValidateModel(t *testing.T) {
	catalog := Claude{}
	for _, model := range []string{"sonnet", "claude-sonnet-4-5-20250929"} {
		if err := catalog.ValidateModel(context.Background(), model); err != nil {
			t.Errorf("valid Claude model %q rejected: %v", model, err)
		}
	}
	if err := catalog.ValidateModel(context.Background(), "arbitrary"); err == nil {
		t.Error("unknown Claude model accepted")
	}
}

// The aliases lead, then the newest account's cached catalog; an alias takes
// its family's newest levels.
func TestClaudeModelsReadTheCachedCatalog(t *testing.T) {
	ctx := context.Background()
	catalog := Claude{Dir: t.TempDir()}
	aliases := claudeAliases()
	if got, _ := catalog.Models(ctx); !reflect.DeepEqual(got, aliases) {
		t.Fatalf("without a cache = %v", got)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(catalog.Dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("old-acct-1-cc.json", `{"fetchedAt":1,"catalog":{"config":{"models":[{"id":"claude-old","name":"Old"}]}}}`)
	write("new-acct-2-cc.json", `{"fetchedAt":2,"catalog":{"config":{"models":[
		{"id":"claude-opus-5-5","name":"Opus 5.5","runtime":{"effort_levels":["low","max"]}},
		{"id":"claude-opus-4-6","name":"Opus 4.6","runtime":{"effort_levels":["low"]}},
		{"id":"claude-haiku-4-5-20251001","name":"Haiku 4.5","runtime":{}}]}}}`)
	write("published-x.json", `{"fetchedAt":3}`)
	got, err := catalog.Models(ctx)
	want := claudeAliases()
	for i := range want {
		if want[i].ID == "opus" || want[i].ID == "opusplan" {
			want[i].ThinkingLevels = []string{"low", "max"}
		}
	}
	want = append(want,
		backend.Model{ID: "claude-opus-5-5", DisplayName: "Opus 5.5", ThinkingLevels: []string{"low", "max"}},
		backend.Model{ID: "claude-opus-4-6", DisplayName: "Opus 4.6", ThinkingLevels: []string{"low"}},
		backend.Model{ID: "claude-haiku-4-5-20251001", DisplayName: "Haiku 4.5"},
	)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("cached = %+v, %v; want %+v", got, err, want)
	}
	for _, model := range []string{"opusplan", "claude-opus-5-5", "claude-opus-9"} {
		if err := catalog.ValidateModel(ctx, model); err != nil {
			t.Errorf("%s rejected: %v", model, err)
		}
	}
}

// Claude Code owns the cache format: any shape usher cannot read leaves just
// the aliases, never an error.
func TestClaudeModelsSurviveAChangedCache(t *testing.T) {
	for name, body := range map[string]string{
		"not json":     `{"fetchedAt":`,
		"models moved": `{"fetchedAt":1,"catalog":{"models":[{"id":"claude-x"}]}}`,
	} {
		catalog := Claude{Dir: t.TempDir()}
		if err := os.WriteFile(filepath.Join(catalog.Dir, "a-b-cc.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := catalog.Models(context.Background())
		if err != nil || !reflect.DeepEqual(got, claudeAliases()) {
			t.Errorf("%s: Models() = %v, %v; want the aliases", name, got, err)
		}
	}
}
