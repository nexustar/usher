package codex

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectTrust(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got := projectTrust(sub, nil)
	for _, key := range []string{sub, repo} {
		if _, ok := got[key]; !ok {
			t.Errorf("undecided project: no trust for %s in %v", key, got)
		}
	}
	if got := projectTrust(sub, map[string]projectConfig{repo: {TrustLevel: "untrusted"}}); got != nil {
		t.Errorf("untrusted repo root overridden: %v", got)
	}
	if got := projectTrust(sub, map[string]projectConfig{
		root: {TrustLevel: "untrusted"}, repo: {TrustLevel: "trusted"},
	}); got == nil {
		t.Error("nearer trusted decision did not win over an untrusted ancestor")
	}
}
