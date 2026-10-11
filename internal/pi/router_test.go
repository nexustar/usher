package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated extension must carry the instance paths, with no template
// placeholder left behind.
func TestPrepareUsherRouterExtension(t *testing.T) {
	dataDir := t.TempDir()
	path, err := PrepareUsherRouterExtension(dataDir, filepath.Join(dataDir, "hook.sock"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{`registerMcpServer("usher"`, `"--router-tools"`, "hook.sock", `exposure: "direct"`} {
		if !strings.Contains(body, want) {
			t.Errorf("generated extension missing %s", want)
		}
	}
	if strings.Contains(body, "__USHER") || strings.Contains(body, "__ROUTER") {
		t.Error("template placeholder left in generated extension")
	}
}

// TestSpawnArgsRouterSelection: only the router worker loads the routing
// extension — fresh by intent, cold resume by the designated id.
func TestSpawnArgsRouterSelection(t *testing.T) {
	r := NewRuntime("pi", t.TempDir(), []string{"--extension", "/x/base.ts"}, 1, Models{}, nil, nil)
	r.SetRouterExtension("/x/router.ts")

	got := strings.Join(r.spawnArgs(nil, true), " ")
	if got != "--extension /x/base.ts --extension /x/router.ts" {
		t.Fatalf("router spawn args = %q", got)
	}
	got = strings.Join(r.spawnArgs([]string{"-c", "x=1"}, false), " ")
	if got != "--extension /x/base.ts -c x=1" {
		t.Fatalf("ordinary spawn args = %q", got)
	}

	r.SetRouterSession("r1")
	if !r.isRouter("r1") || r.isRouter("other") || r.isRouter("") {
		t.Fatal("designation lookup wrong")
	}
}
