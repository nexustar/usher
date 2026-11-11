package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogin(t *testing.T) {
	script := filepath.Join(t.TempDir(), "codex")
	body := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"fake/1"}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"type":"chatgptDeviceCode","loginId":"L1","verificationUrl":"https://auth.openai.com/codex/device","userCode":"ABCD-12345"}}'
sleep 0.2
printf '%s\n' '{"jsonrpc":"2.0","method":"account/login/completed","params":{"loginId":"other","success":false}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"account/login/completed","params":{"loginId":"L1","success":true}}'
cat >/dev/null
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	m := NewManager(script, nil, nil, nil, nil, 1, nil)
	t.Cleanup(m.Shutdown)
	l, err := m.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.URL != "https://auth.openai.com/codex/device" || l.UserCode != "ABCD-12345" || l.State != "pending" {
		t.Fatalf("login = %+v", l)
	}
	for deadline := time.Now().Add(5 * time.Second); m.LoginStatus().State == "pending"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("login still pending")
		}
	}
	if l := m.LoginStatus(); l.State != "done" {
		t.Fatalf("login = %+v", l)
	}
}
