package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// approveArgs trusts cwd's project-local resources: RPC mode cannot prompt and
// skips them silently. --approve beats a saved decision, so a saved "do not
// trust" or an explicit trust flag withholds it.
func approveArgs(cwd string, extra []string) []string {
	for _, arg := range extra {
		switch arg {
		case "--approve", "-a", "--no-approve", "-na":
			return nil
		}
	}
	if distrusted(filepath.Join(agentDir(), "trust.json"), cwd) {
		return nil
	}
	return []string{"--approve"}
}

// distrusted reads pi's trust store: the nearest decided ancestor wins, and
// null entries decide nothing.
func distrusted(trustFile, cwd string) bool {
	raw, err := os.ReadFile(trustFile)
	if err != nil {
		return false
	}
	var store map[string]*bool
	if json.Unmarshal(raw, &store) != nil {
		return false
	}
	dir := cwd
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		dir = resolved
	}
	for {
		if decision := store[dir]; decision != nil {
			return !*decision
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func agentDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}
