// Package modelcatalog implements model metadata sources for built-in agent
// backends. Catalogs are account-level capabilities, independent of workers.
package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nexustar/usher/internal/backend"
)

type Claude struct{}

func (Claude) Models(context.Context) ([]backend.Model, error) {
	return []backend.Model{
		{ID: "fable", DisplayName: "Fable"},
		{ID: "opus", DisplayName: "Opus"},
		{ID: "sonnet", DisplayName: "Sonnet"},
		{ID: "haiku", DisplayName: "Haiku"},
		{ID: "opusplan", DisplayName: "Opus Plan"},
		{ID: "claude-opus-4-8", DisplayName: "Opus 4.8"},
		{ID: "claude-opus-4-6", DisplayName: "Opus 4.6"},
		{ID: "claude-sonnet-4-6", DisplayName: "Sonnet 4.6"},
	}, nil
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
		Slug                  string `json:"slug"`
		DisplayName           string `json:"display_name"`
		Visibility            string `json:"visibility"`
		Priority              int    `json:"priority"`
		DefaultReasoningLevel string `json:"default_reasoning_level"`
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
		out = append(out, backend.Model{ID: m.Slug, DisplayName: label})
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
