package agentupdate

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeCLI writes a CLI reporting the version in dir/version; `update` runs body.
func fakeCLI(t *testing.T, version, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nd=" + dir + "\nif [ \"$1\" = --version ]; then echo \"agent-cli $(cat $d/version) (x)\"; exit 0; fi\n" + body + "\n"
	path := filepath.Join(dir, "agent")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func latest(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

func manager(t *testing.T, specs ...Spec) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(ctx, specs, slog.New(slog.DiscardHandler))
}

func waitIdle(t *testing.T, m *Manager) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s := m.List()[0]
		if !s.Updating && !s.CheckedAt.IsZero() {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("update did not finish")
	return Status{}
}

func TestCheckFindsNewerRelease(t *testing.T) {
	cli := fakeCLI(t, "1.2.3", "")
	m := manager(t, Spec{Name: "a", Cmd: cli, Latest: latest("1.10.0")})
	m.Check(context.Background())
	s := m.List()[0]
	if s.Current != "1.2.3" || s.Latest != "1.10.0" || !s.UpdateAvailable || s.Path != cli {
		t.Fatalf("status = %+v", s)
	}
}

func TestCheckNeverOffersADowngrade(t *testing.T) {
	m := manager(t, Spec{Name: "a", Cmd: fakeCLI(t, "2.0.0", ""), Latest: latest("1.9.9")})
	m.Check(context.Background())
	if s := m.List()[0]; s.UpdateAvailable {
		t.Fatalf("status = %+v", s)
	}
}

func TestCheckKeepsLastReleaseOnFailure(t *testing.T) {
	fail := false
	spec := Spec{Name: "a", Cmd: fakeCLI(t, "1.0.0", ""), Latest: func(context.Context) (string, error) {
		if fail {
			return "", errors.New("offline")
		}
		return "1.1.0", nil
	}}
	m := manager(t, spec)
	m.Check(context.Background())
	fail = true
	m.Check(context.Background())
	if s := m.List()[0]; s.Latest != "1.1.0" || !s.UpdateAvailable || s.CheckError != "offline" {
		t.Fatalf("status = %+v", s)
	}
}

func TestCheckMissingCLI(t *testing.T) {
	m := manager(t, Spec{Name: "a", Cmd: filepath.Join(t.TempDir(), "gone")})
	m.Check(context.Background())
	if s := m.List()[0]; s.CheckError == "" || s.Current != "" {
		t.Fatalf("status = %+v", s)
	}
}

func TestUpdateRunsCommandAndRechecks(t *testing.T) {
	cli := fakeCLI(t, "1.0.0", `echo "installing $1 $2"; echo 1.1.0 > $d/version`)
	m := manager(t, Spec{Name: "a", Cmd: cli, UpdateArgs: []string{"update", "--self"}, Latest: latest("1.1.0")})
	m.Check(context.Background())
	if err := m.Update("a"); err != nil {
		t.Fatal(err)
	}
	s := waitIdle(t, m)
	if s.Current != "1.1.0" || s.UpdateAvailable || s.UpdateError != "" {
		t.Fatalf("status = %+v", s)
	}
	if !strings.Contains(s.Output, "installing update --self") {
		t.Fatalf("output = %q", s.Output)
	}
}

func TestCLIFindsSiblingRuntime(t *testing.T) {
	cli := fakeCLI(t, "1.0.0", `sibling`)
	sibling := filepath.Join(filepath.Dir(cli), "sibling")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\necho from-sibling\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := manager(t, Spec{Name: "a", Cmd: cli})
	if err := m.Update("a"); err != nil {
		t.Fatal(err)
	}
	if s := waitIdle(t, m); !strings.Contains(s.Output, "from-sibling") {
		t.Fatalf("status = %+v", s)
	}
}

func TestUpdateFailureKeepsOutput(t *testing.T) {
	cli := fakeCLI(t, "1.0.0", `echo denied >&2; exit 3`)
	m := manager(t, Spec{Name: "a", Cmd: cli, Latest: latest("1.1.0")})
	m.Check(context.Background())
	if err := m.Update("a"); err != nil {
		t.Fatal(err)
	}
	s := waitIdle(t, m)
	if s.UpdateError == "" || !strings.Contains(s.Output, "denied") || !s.UpdateAvailable {
		t.Fatalf("status = %+v", s)
	}
}

func TestUpdateIsExclusive(t *testing.T) {
	cli := fakeCLI(t, "1.0.0", `read line; sleep 0.2`)
	m := manager(t, Spec{Name: "a", Cmd: cli})
	if err := m.Update("a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Update("a"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Update = %v", err)
	}
	waitIdle(t, m)
	if err := m.Update("nope"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown Update = %v", err)
	}
}

func TestUpdateStopsWithContext(t *testing.T) {
	cli := fakeCLI(t, "1.0.0", `sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	m := New(ctx, []Spec{{Name: "a", Cmd: cli}}, slog.New(slog.DiscardHandler))
	if err := m.Update("a"); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for m.List()[0].Updating {
		if time.Now().After(deadline) {
			t.Fatal("update survived its context")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := m.List()[0]; s.UpdateError == "" {
		t.Fatalf("status = %+v", s)
	}
}

func TestOutputKeepsTail(t *testing.T) {
	m := manager(t, Spec{Name: "a", Cmd: "x"})
	w := outputWriter{m: m, e: m.entries[0]}
	if _, err := w.Write([]byte(strings.Repeat("a", maxOutput) + "end")); err != nil {
		t.Fatal(err)
	}
	if s := m.List()[0].Output; len(s) != maxOutput || !strings.HasSuffix(s, "end") {
		t.Fatalf("len = %d", len(s))
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.10.0", "1.9.9", 1},
		{"1.2.3", "1.2.4", -1},
		{"1.2.3", "1.2.3-alpha.1", 1},
		{"1.2.3-alpha.1", "1.2.3", -1},
		{"1.2.3-beta", "1.2.3-alpha", 1},
	} {
		got := compareVersions(tc.a, tc.b)
		if (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) {
			t.Errorf("compareVersions(%s, %s) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"rust-v0.50.0":        "0.50.0",
		"2.1.316\n":           "2.1.316",
		"codex-cli 1.2.3-rc1": "1.2.3-rc1",
	} {
		if got, err := parseVersion(in); err != nil || got != want {
			t.Errorf("parseVersion(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := parseVersion(""); err == nil {
		t.Error("empty input parsed")
	}
}

func TestClaudeChannel(t *testing.T) {
	dir := t.TempDir()
	if got := claudeChannel(dir); got != "latest" {
		t.Errorf("no settings = %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"autoUpdatesChannel":"stable"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claudeChannel(dir); got != "stable" {
		t.Errorf("stable settings = %s", got)
	}
}
