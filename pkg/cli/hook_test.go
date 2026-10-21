package cli

import (
	"io"
	"os"
	"testing"
)

func TestLegacyPermissionRequestContinuesWithoutSocket(t *testing.T) {
	t.Setenv("USHER_HOOK_SOCK", "")
	original := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = original })

	if err := Hook([]string{"PermissionRequest"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "{\"continue\":true}\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPermissionRequestWithSocketReachesServerDuringUpgrade(t *testing.T) {
	if continueLegacyPermissionLocally("PermissionRequest", "/tmp/old-server.sock") {
		t.Fatal("managed legacy hook must reach the old server")
	}
}
