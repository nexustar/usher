package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/agentprofile"
)

func TestAgentCRUDHandlers(t *testing.T) {
	store, err := agentprofile.Load(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{agents: store}

	create := httptest.NewRequest(http.MethodPost, "/api/agents",
		strings.NewReader(`{"name":"dev","backend":"codex"}`))
	created := httptest.NewRecorder()
	s.handleCreateAgent(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}

	// A different name in the body renames the agent, so the response — not the
	// request path — is what identifies it afterwards.
	update := httptest.NewRequest(http.MethodPut, "/api/agents/dev",
		strings.NewReader(`{"name":"dev-renamed","model":"test-model"}`))
	update.SetPathValue("name", "dev")
	updated := httptest.NewRecorder()
	s.handleUpdateAgent(updated, update)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"name":"dev-renamed"`) {
		t.Fatalf("update status = %d, body = %s", updated.Code, updated.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/agents/dev-renamed", nil)
	deleteReq.SetPathValue("name", "dev-renamed")
	deleted := httptest.NewRecorder()
	s.handleDeleteAgent(deleted, deleteReq)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", deleted.Code, deleted.Body.String())
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("agents after delete = %+v", got)
	}
}
