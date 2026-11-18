package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogin(t *testing.T) {
	script := filepath.Join(t.TempDir(), "claude")
	body := `#!/bin/sh
printf '%s\n' "$BROWSER" > "$0.browser"
echo 'If the browser did not open, visit: https://claude.com/cai/oauth/authorize?code=true&state=x'
printf 'Paste code here if prompted > '
IFS= read -r code
[ "$code" = good ] && exit 0
echo 'OAuth error: invalid code'
exit 1
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	m := New(script, "", "", nil, 1, nil, nil)
	t.Cleanup(m.Shutdown)
	for code, want := range map[string]string{"good": "done: ", "bad": "failed: OAuth error: invalid code"} {
		l, err := m.StartLogin(context.Background())
		if err != nil || l.URL != "https://claude.com/cai/oauth/authorize?code=true&state=x" || !l.NeedsCode {
			t.Fatalf("login = %+v, err = %v", l, err)
		}
		if err := m.SubmitLoginCode(code); err != nil {
			t.Fatal(err)
		}
		for m.LoginStatus().State == "pending" {
			time.Sleep(20 * time.Millisecond)
		}
		if l := m.LoginStatus(); l.State+": "+l.Error != want {
			t.Fatalf("code %s: %+v", code, l)
		}
	}
	if b, _ := os.ReadFile(script + ".browser"); string(b) != "true\n" {
		t.Fatalf("BROWSER = %q", b)
	}
}
