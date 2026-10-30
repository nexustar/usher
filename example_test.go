package usher_test

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexustar/usher"
	"github.com/nexustar/usher/pkg/backend"
)

// A program embedding usher: its own serve adds a backend and a route;
// every other command, hook and mcp-stdio included, goes to Main.
func Example() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		os.Exit(usher.Main(os.Args[1:]))
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	o := usher.DefaultOptions()
	o.Flags(fs)
	root := fs.String("echo-sessions-dir", "", "echo session logs directory")
	_ = fs.Parse(os.Args[2:])
	o.Backends = func(usher.Env) (map[string]backend.Backend, error) {
		if *root == "" {
			return nil, nil
		}
		return map[string]backend.Backend{"echo": {
			Runtime:    echoRuntime{},
			Transcript: echoTranscript{},
			Source:     echoSource{*root},
		}}, nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv, err := usher.New(ctx, o)
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	srv.Handle("GET /echo/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		log.Fatal(err)
	}
	go srv.Serve(ln)
	<-ctx.Done()
}
