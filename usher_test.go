package usher_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

// Run brings up an extension's backend with no built-in one present, hands
// Start the assembled app, and stops cleanly when ctx is cancelled.
func TestRunServesExtensionBackend(t *testing.T) {
	o := usher.DefaultOptions()
	o.Addr = "127.0.0.1:0"
	o.DataDir = filepath.Join(t.TempDir(), "data")
	o.ClaudeSessionsDir, o.CodexSessionsDir, o.PiSessionsDir = "", "", ""
	o.DisablePush = true
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var backends []string
	ext := usher.Extension{
		Name: "echo",
		Backends: func(env usher.Env) (map[string]backend.Backend, error) {
			if env.Interactions == nil || env.HookSocket == "" {
				t.Errorf("Env = %+v, want interactions and a hook socket", env)
			}
			return map[string]backend.Backend{"echo": {
				Runtime: echoRuntime{}, Transcript: echoTranscript{}, Source: echoSource{t.TempDir()},
			}}, nil
		},
		Start: func(ctx context.Context, app *usher.App) error {
			backends = app.Router.Backends()
			app.Handle("GET /echo/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("pong"))
			}))
			cancel()
			return nil
		},
	}
	if err := usher.Run(ctx, o, ext); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains(backends, "echo") {
		t.Fatalf("backends at Start = %v, want echo", backends)
	}
}

// A backend name two providers claim is a startup error, not a silent
// override.
func TestRunRejectsDuplicateBackend(t *testing.T) {
	o := usher.DefaultOptions()
	o.Addr = "127.0.0.1:0"
	o.DataDir = filepath.Join(t.TempDir(), "data")
	o.ClaudeSessionsDir, o.CodexSessionsDir, o.PiSessionsDir = "", "", ""
	o.DisablePush = true
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	provide := func(env usher.Env) (map[string]backend.Backend, error) {
		return map[string]backend.Backend{"echo": {Runtime: echoRuntime{}, Transcript: echoTranscript{}}}, nil
	}
	err := usher.Run(context.Background(), o, usher.Extension{Name: "a", Backends: provide}, usher.Extension{Name: "b", Backends: provide})
	if err == nil || !strings.Contains(err.Error(), `backend "echo" is already registered`) {
		t.Fatalf("Run error = %v, want duplicate backend", err)
	}
}
