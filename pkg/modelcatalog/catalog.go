// Package modelcatalog implements model metadata sources for built-in agent
// backends. Catalogs are account-level capabilities, independent of workers.
package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nexustar/usher/pkg/backend"
)

// Claude lists Claude Code's aliases, then the per-account catalog it caches
// in Dir (~/.claude/cache/model-catalog). The cache format is undocumented, so
// anything unreadable leaves just the aliases.
type Claude struct{ Dir string }

func claudeAliases() []backend.Model {
	return []backend.Model{
		{ID: "fable", DisplayName: "Fable", Alias: true},
		{ID: "opus", DisplayName: "Opus", Alias: true},
		{ID: "sonnet", DisplayName: "Sonnet", Alias: true},
		{ID: "haiku", DisplayName: "Haiku", Alias: true},
		{ID: "opusplan", DisplayName: "Opus Plan", Alias: true},
	}
}

type claudeCatalog struct {
	FetchedAt int64 `json:"fetchedAt"`
	Catalog   struct {
		Config struct {
			Models []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Runtime struct {
					EffortLevels []string `json:"effort_levels"`
				} `json:"runtime"`
			} `json:"models"`
		} `json:"config"`
	} `json:"catalog"`
}

// cached is the newest account's catalog: one file per account, named
// <account>-<hash>-cc.json.
func (c Claude) cached() []backend.Model {
	paths, _ := filepath.Glob(filepath.Join(c.Dir, "*-cc.json"))
	var newest claudeCatalog
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc claudeCatalog
		if json.Unmarshal(raw, &doc) == nil && len(doc.Catalog.Config.Models) > 0 && doc.FetchedAt > newest.FetchedAt {
			newest = doc
		}
	}
	var out []backend.Model
	for _, m := range newest.Catalog.Config.Models {
		if m.ID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, backend.Model{ID: m.ID, DisplayName: name, ThinkingLevels: m.Runtime.EffortLevels})
	}
	return out
}

// Models gives each alias the effort levels of its family's newest model,
// which the catalog lists first.
func (c Claude) Models(context.Context) ([]backend.Model, error) {
	cached := c.cached()
	aliases := claudeAliases()
	for i, a := range aliases {
		prefix := "claude-" + strings.TrimSuffix(a.ID, "plan") + "-"
		for _, m := range cached {
			if strings.HasPrefix(m.ID, prefix) {
				aliases[i].ThinkingLevels = m.ThinkingLevels
				break
			}
		}
	}
	return append(aliases, cached...), nil
}

func (Claude) DefaultEffort(context.Context, string) (string, error) { return "", nil }

func (c Claude) ValidateModel(ctx context.Context, model string) error {
	models, _ := c.Models(ctx)
	for _, candidate := range models {
		if candidate.ID == model {
			return nil
		}
	}
	if strings.HasPrefix(model, "claude-") {
		return nil
	}
	return fmt.Errorf("unknown Claude model %q", model)
}

type Codex struct{ Path string }

func codexFallbackModels() []backend.Model {
	return []backend.Model{
		{ID: "gpt-5.5", DisplayName: "GPT-5.5"},
		{ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 Mini"},
	}
}

type codexDocument struct {
	Models []struct {
		Slug                     string `json:"slug"`
		DisplayName              string `json:"display_name"`
		Visibility               string `json:"visibility"`
		Priority                 int    `json:"priority"`
		DefaultReasoningLevel    string `json:"default_reasoning_level"`
		SupportedReasoningLevels []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
	} `json:"models"`
}

func (c Codex) read() (codexDocument, error) {
	var doc codexDocument
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return doc, err
	}
	err = json.Unmarshal(raw, &doc)
	return doc, err
}

func (c Codex) Models(context.Context) ([]backend.Model, error) {
	doc, err := c.read()
	if err != nil || len(doc.Models) == 0 {
		// Keep session creation available before Codex has refreshed its cache.
		return codexFallbackModels(), nil
	}
	picks := doc.Models[:0:0]
	for _, m := range doc.Models {
		if m.Visibility == "list" && m.Slug != "" {
			picks = append(picks, m)
		}
	}
	if len(picks) == 0 {
		return codexFallbackModels(), nil
	}
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].Priority < picks[j].Priority })
	out := make([]backend.Model, 0, len(picks))
	for _, m := range picks {
		label := m.DisplayName
		if label == "" {
			label = m.Slug
		}
		var levels []string
		for _, l := range m.SupportedReasoningLevels {
			if l.Effort != "" {
				levels = append(levels, l.Effort)
			}
		}
		out = append(out, backend.Model{ID: m.Slug, DisplayName: label, ThinkingLevels: levels})
	}
	return out, nil
}

func (c Codex) DefaultEffort(_ context.Context, model string) (string, error) {
	if model == "" {
		return "", nil
	}
	doc, err := c.read()
	if err != nil {
		return "", err
	}
	for _, m := range doc.Models {
		if m.Slug == model {
			return m.DefaultReasoningLevel, nil
		}
	}
	return "", nil
}

func (c Codex) ValidateModel(_ context.Context, model string) error {
	doc, err := c.read()
	if err == nil && len(doc.Models) > 0 {
		for _, candidate := range doc.Models {
			if candidate.Slug == model {
				return nil
			}
		}
		return fmt.Errorf("unknown Codex model %q", model)
	}
	for _, candidate := range codexFallbackModels() {
		if candidate.ID == model {
			return nil
		}
	}
	return fmt.Errorf("unknown Codex model %q", model)
}
