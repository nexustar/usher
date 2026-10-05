package pi

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestApproveArgsRespectsSavedDistrust(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(root, "agent")
	denied := filepath.Join(root, "denied")
	allowed := filepath.Join(denied, "allowed")
	cleared := filepath.Join(denied, "cleared")
	for _, dir := range []string{agent, allowed, cleared, filepath.Join(root, "fresh")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := `{"` + denied + `": false, "` + allowed + `": true, "` + cleared + `": null}`
	if err := os.WriteFile(filepath.Join(agent, "trust.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_CODING_AGENT_DIR", agent)

	approve := []string{"--approve"}
	tests := []struct {
		name  string
		cwd   string
		extra []string
		want  []string
	}{
		{"undecided", filepath.Join(root, "fresh"), nil, approve},
		{"saved distrust", denied, nil, nil},
		{"null entry falls through to parent", cleared, nil, nil},
		{"nearer trust wins", allowed, nil, approve},
		{"explicit flag wins", filepath.Join(root, "fresh"), []string{"-na"}, nil},
	}
	for _, tt := range tests {
		if got := approveArgs(tt.cwd, tt.extra); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: approveArgs = %v, want %v", tt.name, got, tt.want)
		}
	}
}
