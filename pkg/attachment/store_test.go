package attachment

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSave(t *testing.T) {
	root := t.TempDir()
	first, err := Save(root, "session", "../note.txt", strings.NewReader("one"), 10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Save(root, "session", "note.txt", strings.NewReader("two"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if first != filepath.Join(root, "session", "note.txt") || second != filepath.Join(root, "session", "note_1.txt") {
		t.Fatalf("paths = %q, %q", first, second)
	}
	data, _ := os.ReadFile(second)
	if string(data) != "two" {
		t.Fatalf("second content = %q", data)
	}
	if info, _ := os.Stat(first); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestSaveRejectsOversizeAndRemovesPartial(t *testing.T) {
	root := t.TempDir()
	_, err := Save(root, "session", "large.bin", strings.NewReader("12345"), 4)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "session", "large.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file remains: %v", err)
	}
}

func TestSaveRejectsInvalidSessionID(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", ".", "..", "../escape", "nested/session"} {
		if _, err := Save(root, id, "file.txt", strings.NewReader("x"), 10); !errors.Is(err, ErrInvalidSession) {
			t.Errorf("Save session %q: err = %v", id, err)
		}
	}
}
