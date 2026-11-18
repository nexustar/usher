package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Only the completion for this attempt's loginId counts.
func TestLogin(t *testing.T) {
	script := filepath.Join(t.TempDir(), "codex")
	body := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"loginId":"L1","verificationUrl":"https://auth.openai.com/codex/device","userCode":"ABCD-12345"}}'
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
	if err != nil || l.URL != "https://auth.openai.com/codex/device" || l.UserCode != "ABCD-12345" {
		t.Fatalf("login = %+v, err = %v", l, err)
	}
	for m.LoginStatus().State == "pending" {
		time.Sleep(20 * time.Millisecond)
	}
	if l := m.LoginStatus(); l.State != "done" {
		t.Fatalf("login = %+v", l)
	}
}
