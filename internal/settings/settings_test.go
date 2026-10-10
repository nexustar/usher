package settings

import (
	"path/filepath"
	"testing"
)

func TestRouterSettingsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := New(path)
	if got := s.Router(); got != (Router{}) {
		t.Fatalf("fresh settings = %+v", got)
	}
	s.SetRouterAgent("main")
	s.SetRouterSession("router-1")
	if got, want := New(path).Router(), (Router{Agent: "main", SessionID: "router-1"}); got != want {
		t.Fatalf("reloaded = %+v, want %+v", got, want)
	}
}
