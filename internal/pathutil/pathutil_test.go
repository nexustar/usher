package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveWithinDir is the shared security boundary: a path resolves only if
// it stays — after symlink evaluation — strictly inside dir.
func TestResolveWithinDir(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	outside := filepath.Join(outDir, "secret.png")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a symlink inside dir pointing outside
	if err := os.Symlink(outside, filepath.Join(dir, "escape.png")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		rel    string
		wantOK bool
	}{
		{"relative inside", "chart.png", true},
		{"absolute inside", inside, true},
		{"subdir", "sub/a.png", true},
		{"dot-dot escape", "../" + filepath.Base(outDir) + "/secret.png", false},
		{"absolute outside", outside, false},
		{"symlink escaping dir", "escape.png", false},
		{"nonexistent", "missing.png", false},
		{"empty rel", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ResolveWithinDir(dir, c.rel)
			if ok != c.wantOK {
				t.Fatalf("ResolveWithinDir(%q) ok=%v want=%v (got=%q)", c.rel, ok, c.wantOK, got)
			}
		})
	}

	if _, ok := ResolveWithinDir("", "anything.png"); ok {
		t.Fatal("empty dir must not resolve")
	}
}

func TestResolveImagePathAllowsCodexGeneratedImages(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	sessionID := "019f523e-c091-7af2-8b73-a36711148c73"
	dir := filepath.Join(codexHome, "generated_images", sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "result.png")
	if err := os.WriteFile(file, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The resolver returns the real path (/var is a symlink on macOS).
	want, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := ResolveWithinDir(CodexGeneratedImagesDir(sessionID), file)
	if !ok || got != want {
		t.Fatalf("ResolveWithinDir() = %q, %v; want %q, true", got, ok, want)
	}
	if _, ok := ResolveWithinDir(CodexGeneratedImagesDir("another-session"), file); ok {
		t.Fatal("another session must not resolve this generated image")
	}
}
