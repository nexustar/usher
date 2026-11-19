package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/nexustar/usher/pkg/backend"
)

// mcpLoginTimeout bounds how long app-server keeps the callback listener.
const mcpLoginTimeout = 15 * time.Minute

// StartMCPLogin asks app-server for the server's sign-in page. It returns the
// loopback redirect app-server listens on, the one address FinishMCPLogin
// will fetch.
func (c *Client) StartMCPLogin(ctx context.Context, threadID, server string) (authURL, redirect string, err error) {
	done := make(chan error, 1)
	c.mu.Lock()
	if c.mcpLogins == nil {
		c.mcpLogins = map[string]chan error{}
	}
	c.mcpLogins[server] = done
	c.mu.Unlock()
	var res struct {
		AuthorizationURL string `json:"authorizationUrl"`
	}
	params := map[string]any{"name": server, "threadId": threadID, "timeoutSecs": int(mcpLoginTimeout / time.Second)}
	if err := c.call(ctx, "mcpServer/oauth/login", params, &res); err != nil {
		return "", "", err
	}
	u, err := url.Parse(res.AuthorizationURL)
	if err != nil {
		return "", "", fmt.Errorf("parse sign-in page: %w", err)
	}
	return res.AuthorizationURL, u.Query().Get("redirect_uri"), nil
}

func (c *Client) finishMCPLogin(params json.RawMessage) {
	var p struct {
		Name    string  `json:"name"`
		Success bool    `json:"success"`
		Error   *string `json:"error"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	var err error
	if !p.Success {
		err = errors.New("sign-in failed")
		if p.Error != nil {
			err = errors.New(*p.Error)
		}
	}
	c.mu.Lock()
	done := c.mcpLogins[p.Name]
	c.mu.Unlock()
	if done != nil {
		select {
		case done <- err:
		default:
		}
	}
}

// WaitMCPLogin waits for the sign-in's outcome, which may already be in.
func (c *Client) WaitMCPLogin(ctx context.Context, server string) error {
	c.mu.Lock()
	done := c.mcpLogins[server]
	c.mu.Unlock()
	if done == nil {
		return errors.New("no sign-in in progress")
	}
	select {
	case <-ctx.Done():
		return errors.New("sign-in has not completed")
	case err := <-done:
		c.mu.Lock()
		if c.mcpLogins[server] == done {
			delete(c.mcpLogins, server)
		}
		c.mu.Unlock()
		return err
	}
}

// ReloadMCP makes app-server redial MCP servers, picking up new credentials.
func (c *Client) ReloadMCP(ctx context.Context) error {
	return c.call(ctx, "config/mcpServer/reload", nil, nil)
}

// deliverCallback fetches the pasted redirect on this machine, only where it
// matches the loopback redirect app-server is listening on.
func deliverCallback(ctx context.Context, redirect, pasted string) error {
	want, err := url.Parse(redirect)
	if err != nil || redirect == "" {
		return errors.New("no sign-in in progress")
	}
	got, err := url.Parse(pasted)
	if err != nil || got.Scheme != "http" || got.Host != want.Host || got.Path != want.Path || !isLoopback(got.Hostname()) {
		return errors.New("that address is not this sign-in's callback")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, got.String(), nil)
	if err != nil {
		return err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deliver callback: %w", err)
	}
	resp.Body.Close()
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// StartMCPLogin signs a server in through the session's own app-server, which
// redials it once the sign-in completes.
func (m *Manager) StartMCPLogin(ctx context.Context, id, server string) (string, error) {
	w, err := m.leaseLive(id)
	if err != nil {
		return "", err
	}
	defer m.releaseWorker(w)
	authURL, redirect, err := w.client.StartMCPLogin(ctx, id, server)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.mcpRedirects == nil {
		m.mcpRedirects = map[string]string{}
	}
	m.mcpRedirects[id+"\x00"+server] = redirect
	m.mu.Unlock()
	w.client.log().Info("mcp login started", "server", server)
	return authURL, nil
}

// FinishMCPLogin delivers the pasted redirect, if any, waits for app-server's
// outcome, then has the other live workers redial with the new credentials.
func (m *Manager) FinishMCPLogin(ctx context.Context, id, server, callbackURL string) error {
	w, err := m.leaseLive(id)
	if err != nil {
		return err
	}
	defer m.releaseWorker(w)
	key := id + "\x00" + server
	m.mu.Lock()
	redirect := m.mcpRedirects[key]
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if callbackURL != "" {
		if err := deliverCallback(ctx, redirect, callbackURL); err != nil {
			return err
		}
	}
	if err := w.client.WaitMCPLogin(ctx, server); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.mcpRedirects, key)
	var others []*Client
	for oid, o := range m.workers {
		if oid != id && o.ready == nil && o.client.Running() {
			others = append(others, o.client)
		}
	}
	m.mu.Unlock()
	for _, c := range others {
		_ = c.ReloadMCP(ctx)
	}
	w.client.log().Info("mcp login finished", "server", server)
	return nil
}

// leaseLive leases the session's running worker without starting one.
func (m *Manager) leaseLive(id string) (*worker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workers[id]
	if w == nil || w.ready != nil || w.err != nil || !w.client.Running() {
		return nil, backend.ErrNotLive
	}
	w.leases++
	w.lastUsed = time.Now()
	return w, nil
}
