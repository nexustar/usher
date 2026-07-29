package mainchat

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStore_AppendAndRead(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Append("default", Message{Role: "user", Content: "hello"}))
	must(s.Append("default", Message{Role: "agent", Content: "hi back"}))

	msgs, err := s.Read("default", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != "agent" || msgs[1].Content != "hi back" {
		t.Errorf("msg1 = %+v", msgs[1])
	}
	if msgs[0].Time.IsZero() {
		t.Error("Time should be auto-populated")
	}
}

func TestStore_ReadMissing(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	msgs, err := s.Read("never", 0)
	if err != nil {
		t.Fatal(err)
	}
	if msgs != nil {
		t.Errorf("expected nil, got %v", msgs)
	}
}

func TestStore_ReadLimit(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	for i := 0; i < 5; i++ {
		_ = s.Append("c", Message{Role: "user", Content: "m"})
	}
	msgs, _ := s.Read("c", 2)
	if len(msgs) != 2 {
		t.Errorf("got %d, want 2", len(msgs))
	}
}

func TestStore_InvalidID(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if err := s.Append("../etc/passwd", Message{Role: "user"}); err == nil {
		t.Error("expected error for path-traversal id")
	}
	if err := s.Append("with spaces", Message{Role: "user"}); err == nil {
		t.Error("expected error for spaces in id")
	}
}

func TestStore_List(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	_ = s.Append("alpha", Message{Role: "user", Content: "first prompt for alpha"})
	_ = s.Append("beta", Message{Role: "user", Content: "first prompt for beta"})

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	titles := map[string]string{}
	for _, c := range got {
		titles[c.ID] = c.Title
	}
	if !strings.HasPrefix(titles["alpha"], "first prompt for alpha") {
		t.Errorf("alpha title = %q", titles["alpha"])
	}
}

func TestStore_PathSafe(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	_ = s.Append("c1", Message{Role: "user", Content: "x"})

	// Make sure we wrote to dir/c1.jsonl, not anywhere else.
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(matches) != 1 || filepath.Base(matches[0]) != "c1.jsonl" {
		t.Errorf("unexpected files: %v", matches)
	}
}
