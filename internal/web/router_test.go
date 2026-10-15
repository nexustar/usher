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

// /internal/config reports the current router session and the directory router
// sessions run in; a router on a backend usher does not have is refused.
func TestRouterConfigAndStart(t *testing.T) {
	dir := t.TempDir()
	d, err := discovery.NewMulti(slog.Default(), discovery.NewClaudeSource(filepath.Join(dir, "projects")))
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(d, map[string]backend.Backend{"claude": {}}, "claude", broker.New(), nil, sessionmeta.New("", 0), nil)
	r.SetSettings(settings.New(filepath.Join(dir, "settings.json")), filepath.Join(dir, "router"))
	s := &Server{router: r}

	rec := httptest.NewRecorder()
	s.handleStartRouter(rec, httptest.NewRequest(http.MethodPost, "/internal/router",
		strings.NewReader(`{"initial_message":"hi","backend":"nope"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown backend: status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/internal/config", nil))
	var cfg struct {
		Router routerConfig `json:"router"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Router != (routerConfig{Dir: filepath.Join(dir, "router")}) {
		t.Fatalf("config router = %+v", cfg.Router)
	}
}
