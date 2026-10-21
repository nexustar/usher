package usher_test

import (
	"context"
	"flag"
	"net/http"
	"os"

	"github.com/nexustar/usher"
	"github.com/nexustar/usher/pkg/backend"
)

// A custom binary is cmd/usher plus the extensions it compiles in: here one
// that adds a backend behind its own flag and a route behind usher's
// authentication. Its module depends on github.com/nexustar/usher; usher's
// own dependencies stay as they are.
func Example() {
	var root string
	echo := usher.Extension{
		Name: "echo",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&root, "echo-sessions-dir", "", "echo session logs directory")
		},
		Backends: func(env usher.Env) (map[string]backend.Backend, error) {
			if root == "" {
				return nil, nil
			}
			return map[string]backend.Backend{"echo": {
				Runtime:    echoRuntime{},
				Transcript: echoTranscript{},
				Source:     echoSource{root},
			}}, nil
		},
		Start: func(ctx context.Context, app *usher.App) error {
			app.Handle("GET /echo/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("pong"))
			}))
			return nil
		},
	}
	os.Exit(usher.Main(os.Args[1:], echo))
}
