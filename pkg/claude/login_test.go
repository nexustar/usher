package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeLogin(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "claude")
	body := `#!/bin/sh
printf '%s\n' "$*" > "$0.args"
printf '%s\n' "$BROWSER" > "$0.browser"
echo 'Opening browser to sign in…'
echo 'If the browser did not open, visit: https://claude.com/cai/oauth/authorize?code=true&state=x'
printf 'Paste code here if prompted > '
IFS= read -r code
if [ "$code" = good ]; then echo 'Login successful.'; exit 0; fi
echo 'OAuth error: invalid code'
exit 1
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func waitLogin(t *testing.T, m *Manager) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if l := m.LoginStatus(); l.State != "pending" {
			return l.State + ": " + l.Error
		}
	}
	t.Fatal("login still pending")
	return ""
}

func TestLogin(t *testing.T) {
	script := fakeLogin(t)
	m := New(script, "", "", nil, 1, nil, nil)
	t.Cleanup(m.Shutdown)
	for code, want := range map[string]string{"good": "done: ", "bad": "failed: OAuth error: invalid code"} {
		l, err := m.StartLogin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if l.URL != "https://claude.com/cai/oauth/authorize?code=true&state=x" || !l.NeedsCode || l.State != "pending" {
			t.Fatalf("login = %+v", l)
		}
		if err := m.SubmitLoginCode(code); err != nil {
			t.Fatal(err)
		}
		if got := waitLogin(t, m); got != want {
			t.Fatalf("code %s: %q, want %q", code, got, want)
		}
	}
	if b, _ := os.ReadFile(script + ".browser"); string(b) != "true\n" {
		t.Fatalf("BROWSER = %q", b)
	}
}
