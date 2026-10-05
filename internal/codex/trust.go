package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

type projectConfig struct {
	TrustLevel string `json:"trust_level"`
}

// threadParamsFor is threadParams plus project trust.
func (c *Client) threadParamsFor(ctx context.Context, cwd, model string) map[string]any {
	p := c.threadParams(cwd, model)
	config := p["config"].(map[string]any)
	for k := range config {
		if k == "projects" || strings.HasPrefix(k, "projects.") {
			return p
		}
	}
	var out struct {
		Config struct {
			Projects map[string]projectConfig `json:"projects"`
		} `json:"config"`
	}
	if err := c.call(ctx, "config/read", map[string]any{"cwd": cwd}, &out); err != nil {
		c.log().Warn("config/read failed; project trust left to codex", "err", err)
		return p
	}
	if trust := projectTrust(cwd, out.Config.Projects); trust != nil {
		config["projects"] = trust
	}
	return p
}

// projectTrust is the "projects" override trusting cwd's project for one
// thread: an undecided project silently loses its project-local config and
// gets a read-only sandbox. The override beats config.toml, so a saved
// "untrusted" on cwd or its nearest decided ancestor withholds it.
func projectTrust(cwd string, decided map[string]projectConfig) map[string]any {
	resolved := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = r
	}
	for _, start := range []string{cwd, resolved} {
		for dir := start; ; dir = filepath.Dir(dir) {
			if level := decided[dir].TrustLevel; level != "" {
				if level == "untrusted" {
					return nil
				}
				break
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	// Session defaults key on cwd, project-local config layers on the root.
	trusted := map[string]any{"trust_level": "trusted"}
	return map[string]any{resolved: trusted, projectRoot(resolved): trusted}
}

// projectRoot is the nearest ancestor holding a .git entry, or dir itself.
func projectRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}
