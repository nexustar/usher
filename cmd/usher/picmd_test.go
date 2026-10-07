package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPiCmdPrefersManagedLauncher(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	if got := defaultPiCmd(); got != "pi" {
		t.Fatalf("no install: got %q, want pi", got)
	}

	touch := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	legacy := touch(filepath.Join(home, ".local", "share", "pi-node", "current", "bin", "pi"))
	if got := defaultPiCmd(); got != legacy {
		t.Fatalf("npm install: got %q, want %q", got, legacy)
	}
	managed := touch(filepath.Join(home, ".pi", "agent", "bin", "pi"))
	if got := defaultPiCmd(); got != managed {
		t.Fatalf("managed install: got %q, want %q", got, managed)
	}
	custom := touch(filepath.Join(home, "elsewhere", "bin", "pi"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "elsewhere"))
	if got := defaultPiCmd(); got != custom {
		t.Fatalf("custom agent dir: got %q, want %q", got, custom)
	}
}
