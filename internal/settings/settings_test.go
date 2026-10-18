package settings

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRouterSettingsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := New(path)
	if got := s.Router(); got != (Router{}) {
		t.Fatalf("fresh settings = %+v", got)
	}
	s.SetRouterSession("router-1")
	if got, want := New(path).Router(), (Router{SessionID: "router-1"}); got != want {
		t.Fatalf("reloaded = %+v, want %+v", got, want)
	}
}

// A legacy agents.json moves in whole and is removed.
func TestImportFileAndSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	legacy := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(legacy, []byte(`{"agents":[{"name":"dev"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	if err := s.ImportFile("agents", legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file left behind: %v", err)
	}
	if err := s.ImportFile("schedules", filepath.Join(dir, "schedules.json")); err != nil {
		t.Fatalf("missing legacy file: %v", err)
	}
	r := New(path)
	compact := func(raw json.RawMessage) string {
		var b bytes.Buffer
		_ = json.Compact(&b, raw)
		return b.String()
	}
	if got := compact(r.Agents()); got != `[{"name":"dev"}]` {
		t.Errorf("agents = %s", got)
	}
}
