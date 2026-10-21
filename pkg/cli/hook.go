package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// hookRetryInterval is how long `usher hook` waits between reconnect attempts
// while the usher server is unreachable (e.g. mid-restart).
const hookRetryInterval = time.Second

// hookConnectTimeout caps retrying while the server is unreachable; past it we
// fail open. (A connected request waits unbounded for a human — see Hook.)
const hookConnectTimeout = 60 * time.Second

// Hook is the worker for `usher hook <event>`: it reads the payload from
// stdin, POSTs it to the usher server over its Unix socket, and writes the
// JSON decision to stdout.
//
// USHER_HOOK_SOCK (set by usher on the sessions it spawns) marks a managed
// session. Without it the session is the user's own (terminal/IDE), so we fail
// open at once (`{}`) and let the backend prompt for itself.
//
// For a managed session the resolver is usher's web UI, so we retry an
// unreachable server for up to hookConnectTimeout to ride out a restart, then
// fail open so a dead usher can't freeze the tool. Once connected the wait is
// unbounded — usher holds the request until a human answers in the UI.
func Hook(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: usher hook <event-name>")
	}
	event := args[0]

	sockPath := os.Getenv("USHER_HOOK_SOCK")
	if sockPath == "" {
		// Not a usher-managed session — fail open at once. Be explicit for a
		// stale PermissionRequest registration so both backends continue to
		// their native approval path.
		if continueLegacyPermissionLocally(event, sockPath) {
			fmt.Println(`{"continue":true}`)
			return nil
		}
		fmt.Println("{}")
		return nil
	}

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	client := &http.Client{
		// No client-side timeout: the server holds the request until the user
		// responds via the UI (or Claude Code's hook timeout kills us).
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	// "http://hook" is a placeholder authority; the unix dialer ignores it.
	url := "http://hook/hook/" + event

	deadline := time.Now().Add(hookConnectTimeout)

	for {
		// Rebuild per attempt — the body reader is consumed on use.
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			// usher down / restarting → retry until we give up, then fail open.
			if time.Now().After(deadline) {
				fmt.Fprintln(os.Stderr, "usher hook: server unreachable for", hookConnectTimeout, "- failing open")
				fmt.Println("{}")
				return nil
			}
			time.Sleep(hookRetryInterval)
			continue
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			// Reachable but declined (shouldn't happen for an env-tagged
			// session) → fail open.
			fmt.Fprintln(os.Stderr, "usher hook: server returned", resp.StatusCode, ":", strings.TrimSpace(string(out)))
			fmt.Println("{}")
			return nil
		}
		fmt.Println(strings.TrimRight(string(out), "\n"))
		return nil
	}
}

func continueLegacyPermissionLocally(event, sockPath string) bool {
	return event == "PermissionRequest" && sockPath == ""
}
