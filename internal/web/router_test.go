package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/agentprofile"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/router"
	"github.com/nexustar/usher/internal/sessionmeta"
	"github.com/nexustar/usher/internal/settings"
)

// The router agent is an instance setting: it must name a configured agent,
// and /api/config reports it together with the current router session.
func TestRouterAgentSetting(t *testing.T) {
	dir := t.TempDir()
	d, err := discovery.NewMulti(slog.Default(), discovery.NewClaudeSource(filepath.Join(dir, "projects")))
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(d, map[string]backend.Backend{}, "claude", broker.New(), nil, sessionmeta.New("", 0), nil)
	r.SetSettings(settings.New(filepath.Join(dir, "settings.json")), filepath.Join(dir, "router"))
	s := &Server{router: r, agents: agentprofile.New([]agentprofile.Profile{{Name: "main", Backend: "claude"}})}

	put := func(agent string) int {
		rec := httptest.NewRecorder()
		s.handleSetRouterAgent(rec, httptest.NewRequest(http.MethodPut, "/api/router",
			strings.NewReader(`{"agent":"`+agent+`"}`)))
		return rec.Code
	}
	if code := put("missing"); code != http.StatusBadRequest {
		t.Fatalf("unknown agent: status %d", code)
	}
	if code := put("main"); code != http.StatusOK {
		t.Fatalf("known agent: status %d", code)
	}

	rec := httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var cfg struct {
		Router routerConfig `json:"router"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Router != (routerConfig{Agent: "main"}) {
		t.Fatalf("config router = %+v", cfg.Router)
	}
}
