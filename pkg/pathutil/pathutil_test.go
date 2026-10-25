package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveImagePath(t *testing.T) {
	cwd, extra, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(path string) string {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return real
	}
	inCwd := write(filepath.Join(cwd, "sub", "a.png"))
	inExtra := write(filepath.Join(extra, "b.png"))
	outside := write(filepath.Join(elsewhere, "c.png"))
	if err := os.Symlink(outside, filepath.Join(cwd, "link.png")); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	inHome := write(filepath.Join(home, "Desktop", "shot.png"))

	cases := []struct {
		ref, want string
	}{
		{"sub/a.png", inCwd},
		{"b.png", inExtra},
		{outside, outside},
		{"link.png", outside},
		{"../" + filepath.Base(elsewhere) + "/c.png", outside},
		{"~/Desktop/shot.png", inHome},
		{"missing.png", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, ok := ResolveImagePath(cwd, c.ref, extra)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("ResolveImagePath(%q) = %q, %v; want %q", c.ref, got, ok, c.want)
		}
	}
}
