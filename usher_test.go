package usher_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher"
	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/core"
)

// echo* is the smallest backend an extension can contribute.
type echoRuntime struct{}

func (echoRuntime) Start(context.Context, backend.StartRequest) (string, <-chan backend.Event, error) {
	return "", nil, errors.New("echo: not implemented")
}
func (echoRuntime) Send(context.Context, backend.SendRequest) (<-chan backend.Event, error) {
	return nil, errors.New("echo: not implemented")
}
func (echoRuntime) Resume(context.Context, backend.ResumeRequest) error { return nil }
func (echoRuntime) Has(string) bool                                     { return false }
func (echoRuntime) LiveSessions() []backend.LiveSession                 { return nil }
func (echoRuntime) Interrupt(string) error                              { return nil }
func (echoRuntime) Kill(string) error                                   { return nil }
func (echoRuntime) Shutdown()                                           {}

type echoTranscript struct{}

func (echoTranscript) ReadBefore(string, string, int) ([]core.Turn, bool, error) {
	return nil, false, nil
}
func (echoTranscript) ReadFrom(string, string) ([]core.Turn, error) { return nil, nil }
func (echoTranscript) NewAssembler() backend.Assembler              { return echoAssembler{} }
func (echoTranscript) TurnEnded([]byte) bool                        { return true }

type echoAssembler struct{}

func (echoAssembler) FeedLine([]byte) ([]core.Turn, *core.TurnPart) { return nil, nil }
func (echoAssembler) Flush() *core.Turn                             { return nil }
func (echoAssembler) Model() string                                 { return "" }

type echoSource struct{ root string }

func (echoSource) Backend() string                { return "echo" }
func (s echoSource) Root() string                 { return s.root }
func (echoSource) IsSessionFile(path string) bool { return strings.HasSuffix(path, ".jsonl") }
func (echoSource) SessionID(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}
func (echoSource) NewMetaScanner(string) backend.MetaScanner { return &echoScanner{} }
func (echoSource) Sparse() bool                              { return false }

type echoScanner struct{}

func (*echoScanner) Feed([]byte)            {}
func (*echoScanner) Meta() core.SessionMeta { return core.SessionMeta{} }

// testOptions is usher with no built-in backend, in a data dir short enough
// for its Unix sockets (macOS caps their paths near 100 bytes).
func testOptions(t *testing.T) usher.Options {
	t.Helper()
	dir, err := os.MkdirTemp("", "usher")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	o := usher.DefaultOptions()
	o.Addr = "127.0.0.1:0"
	o.DataDir = filepath.Join(dir, "data")
	o.ClaudeSessionsDir, o.CodexSessionsDir, o.PiSessionsDir = "", "", ""
	o.DisablePush = true
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return o
}

func echoBackends(t *testing.T) func(usher.Env) (map[string]backend.Backend, error) {
	return func(env usher.Env) (map[string]backend.Backend, error) {
		if env.Interactions == nil || env.HookSocket == "" {
			t.Errorf("Env = %+v, want interactions and a hook socket", env)
		}
		return map[string]backend.Backend{"echo": {
			Runtime: echoRuntime{}, Transcript: echoTranscript{}, Source: echoSource{t.TempDir()},
		}}, nil
	}
}

// New's Handler serves on any listener, routes added after New included.
func TestNewServesItsHandlerAnywhere(t *testing.T) {
	o := testOptions(t)
	o.Backends = echoBackends(t)
	srv, err := usher.New(context.Background(), o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	if got := srv.App().Router.Backends(); !slices.Contains(got, "echo") {
		t.Fatalf("backends = %v, want echo", got)
	}
	srv.Handle("GET /echo/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for path, want := range map[string]string{"/healthz": "ok", "/echo/ping": "pong"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != want {
			t.Errorf("GET %s = %d %q, want %q", path, resp.StatusCode, body, want)
		}
	}
}

// A data dir another usher runs on is refused until that one closes.
func TestNewRefusesABusyDataDir(t *testing.T) {
	o := testOptions(t)
	o.Backends = echoBackends(t)
	first, err := usher.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usher.New(context.Background(), o); !errors.Is(err, usher.ErrDataDirInUse) {
		t.Fatalf("second New: %v, want ErrDataDirInUse", err)
	}
	first.Close()
	again, err := usher.New(context.Background(), o)
	if err != nil {
		t.Fatalf("New after Close: %v", err)
	}
	again.Close()
}

// A backend name already in use is a startup error, not a silent override.
func TestNewRejectsDuplicateBackend(t *testing.T) {
	o := testOptions(t)
	o.ClaudeSessionsDir = t.TempDir() // enables the built-in claude
	o.Backends = func(usher.Env) (map[string]backend.Backend, error) {
		return map[string]backend.Backend{"claude": {Runtime: echoRuntime{}, Transcript: echoTranscript{}}}, nil
	}
	_, err := usher.New(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), `backend "claude" is already registered`) {
		t.Fatalf("New error = %v, want duplicate backend", err)
	}
}

// Run serves on Addr until ctx is cancelled, then returns nil.
func TestRunStopsWithItsContext(t *testing.T) {
	o := testOptions(t)
	o.Backends = echoBackends(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- usher.Run(ctx, o) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}
