package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nexustar/usher/pkg/backend"
)

// liveProcess is the session's process once its first init has arrived.
func (m *Manager) liveProcess(id string) (*process, error) {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return nil, backend.ErrNotLive
	}
	p.mu.Lock()
	ready := p.initSeen
	p.mu.Unlock()
	if !ready {
		return nil, backend.ErrNotLive
	}
	return p, nil
}

// StartMCPLogin asks the session's process for the server's sign-in page.
// Claude listens for the redirect itself until FinishMCPLogin.
func (m *Manager) StartMCPLogin(ctx context.Context, id, server string) (string, error) {
	p, err := m.liveProcess(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := m.controlRequest(ctx, p, map[string]any{"subtype": "mcp_authenticate", "serverName": server})
	if err != nil {
		return "", err
	}
	var r struct {
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("decode mcp_authenticate: %w", err)
	}
	if r.AuthURL == "" {
		return "", errors.New("no sign-in page offered: the server may not use OAuth")
	}
	p.logger.Info("mcp login started", "server", server)
	return r.AuthURL, nil
}

func (m *Manager) ReconnectMCP(ctx context.Context, id, server string) error {
	p, err := m.liveProcess(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err = m.controlRequest(ctx, p, map[string]any{"subtype": "mcp_reconnect", "serverName": server})
	return err
}

// FinishMCPLogin hands Claude the pasted redirect, if any, then reconnects:
// a token alone leaves the server needing auth until it is redialed.
func (m *Manager) FinishMCPLogin(ctx context.Context, id, server, callbackURL string) error {
	p, err := m.liveProcess(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if callbackURL != "" {
		if _, err := m.controlRequest(ctx, p, map[string]any{
			"subtype": "mcp_oauth_callback_url", "serverName": server, "callbackUrl": callbackURL,
		}); err != nil {
			return err
		}
	}
	if _, err := m.controlRequest(ctx, p, map[string]any{"subtype": "mcp_reconnect", "serverName": server}); err != nil {
		return err
	}
	raw, err := m.controlRequest(ctx, p, map[string]any{"subtype": "mcp_status"})
	if err != nil {
		return err
	}
	var st struct {
		MCPServers []backend.MCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("decode mcp_status: %w", err)
	}
	for _, s := range st.MCPServers {
		if s.Name == server && s.Status != "connected" {
			return fmt.Errorf("%s is still %s", server, s.Status)
		}
	}
	p.logger.Info("mcp login finished", "server", server)
	return nil
}
