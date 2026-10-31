package agentupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	claudeReleasesURL = "https://downloads.claude.ai/claude-code-releases/"
	codexReleaseURL   = "https://api.github.com/repos/openai/codex/releases/latest"
	piReleaseURL      = "https://pi.dev/api/latest-version"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Claude follows the release channel in configDir's settings.json, as
// `claude update` does.
func Claude(cmd, configDir string) Spec {
	return Spec{Name: "claude", Cmd: cmd, UpdateArgs: []string{"update"},
		Latest: func(ctx context.Context) (string, error) {
			body, err := fetch(ctx, claudeReleasesURL+claudeChannel(configDir))
			if err != nil {
				return "", err
			}
			return parseVersion(string(body))
		}}
}

func claudeChannel(configDir string) string {
	data, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		return "latest"
	}
	var settings struct {
		Channel string `json:"autoUpdatesChannel"`
	}
	if json.Unmarshal(data, &settings) != nil || settings.Channel != "stable" {
		return "latest"
	}
	return "stable"
}

func Codex(cmd string) Spec {
	return Spec{Name: "codex", Cmd: cmd, UpdateArgs: []string{"update"},
		Latest: func(ctx context.Context) (string, error) {
			return latestField(ctx, codexReleaseURL, "tag_name")
		}}
}

// Pi updates only itself; installed pi packages stay on `pi update --extensions`.
func Pi(cmd string) Spec {
	return Spec{Name: "pi", Cmd: cmd, UpdateArgs: []string{"update", "--self"},
		Latest: func(ctx context.Context) (string, error) {
			return latestField(ctx, piReleaseURL, "version")
		}}
}

func latestField(ctx context.Context, url, field string) (string, error) {
	body, err := fetch(ctx, url)
	if err != nil {
		return "", err
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	value, _ := fields[field].(string)
	return parseVersion(value)
}

func parseVersion(s string) (string, error) {
	version := versionRe.FindString(s)
	if version == "" {
		return "", fmt.Errorf("no version in %q", s)
	}
	return version, nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "usher")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}
