package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/router"
	"github.com/nexustar/usher/internal/sessionmeta"
	"github.com/nexustar/usher/internal/settings"
)

// What the router runs on is an instance setting: it must name an available
// backend, and /api/config reports it together with the current router session.
func TestRouterModelSetting(t *testing.T) {
	dir := t.TempDir()
	d, err := discovery.NewMulti(slog.Default(), discovery.NewClaudeSource(filepath.Join(dir, "projects")))
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(d, map[string]backend.Backend{"claude": {}}, "claude", broker.New(), nil, sessionmeta.New("", 0), nil)
	r.SetSettings(settings.New(filepath.Join(dir, "settings.json")), filepath.Join(dir, "router"))
	s := &Server{router: r}

	put := func(body string) int {
		rec := httptest.NewRecorder()
		s.handleSetRouterModel(rec, httptest.NewRequest(http.MethodPut, "/api/router", strings.NewReader(body)))
		return rec.Code
	}
	if code := put(`{"backend":"nope","model":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown backend: status %d", code)
	}
	if code := put(`{"backend":"claude","model":"haiku"}`); code != http.StatusOK {
		t.Fatalf("known backend: status %d", code)
	}

	rec := httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var cfg struct {
		Router routerConfig `json:"router"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Router != (routerConfig{Backend: "claude", Model: "haiku"}) {
		t.Fatalf("config router = %+v", cfg.Router)
	}
}
