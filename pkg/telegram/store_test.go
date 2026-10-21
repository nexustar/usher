package telegram

import (
	"path/filepath"
	"testing"
)

func TestTopicStoreReadopt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topics.json")

	s, err := newTopicStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.thread("sess-a"); ok {
		t.Fatal("empty store should have no binding")
	}
	if err := s.put("sess-a", 99); err != nil {
		t.Fatal(err)
	}
	if err := s.put("sess-b", 100); err != nil {
		t.Fatal(err)
	}

	// Reopen: bindings must survive (re-adopt across restart).
	s2, err := newTopicStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := s2.thread("sess-a"); !ok || id != 99 {
		t.Fatalf("sess-a = %d,%v want 99,true", id, ok)
	}
	if id, ok := s2.thread("sess-b"); !ok || id != 100 {
		t.Fatalf("sess-b = %d,%v want 100,true", id, ok)
	}

	if err := s2.delete("sess-a"); err != nil {
		t.Fatal(err)
	}
	s3, _ := newTopicStore(path)
	if _, ok := s3.thread("sess-a"); ok {
		t.Fatal("sess-a should be gone after delete")
	}
	if _, ok := s3.thread("sess-b"); !ok {
		t.Fatal("sess-b should remain")
	}
}
