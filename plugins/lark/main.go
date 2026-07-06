// usher-lark mirrors usher's Claude Code sessions into a Lark/Feishu group
// chat, one thread per session. It is an out-of-process usher plugin: it
// consumes the Router through usher's plugin socket and talks to Lark through
// the official SDK's websocket long connection (no public endpoint needed).
//
// Setup (Lark developer console):
//   - create a self-built app, add the bot capability
//   - permissions: im:message (send + receive), im:message:send_as_bot,
//     im:resource (image upload), im:message.reactions:write
//   - events: subscribe to im.message.receive_v1 with the "long connection"
//     delivery mode; enable card callbacks over the same connection
//   - add the bot to the target group chat and pass the chat id as --chat-id
//
// Credentials: --app-id flag + $LARK_APP_SECRET (the secret is never a flag).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nexustar/usher/internal/pluginapi"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Compile-time check: the plugin-socket client provides the full Router
// subset the hub consumes.
var _ RouterAPI = (*pluginapi.Client)(nil)

// appSecretEnv holds the app secret; it is a secret, so it's never a flag.
const appSecretEnv = "LARK_APP_SECRET"

var Version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(Version)
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "usher-lark:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("usher-lark", flag.ExitOnError)
	appID := fs.String("app-id", "", "Lark app id (cli_...)")
	chatID := fs.String("chat-id", "", "group chat id to mirror sessions into (oc_...)")
	allowedUsers := fs.String("allowed-user-ids", "",
		"comma-separated open ids (ou_...) allowed to drive sessions; empty = any member of the chat")
	domain := fs.String("domain", "feishu",
		`API domain: "feishu" (open.feishu.cn), "lark" (open.larksuite.com), or a full base URL`)
	socket := fs.String("usher-socket", defaultPluginSocket(),
		"path to usher's plugin API socket (usher serve creates it in its data dir)")
	statePath := fs.String("state", defaultStatePath(),
		"session→thread map file; threads are re-adopted across restarts")
	if err := fs.Parse(args); err != nil {
		return err
	}

	secret := os.Getenv(appSecretEnv)
	switch {
	case *appID == "":
		return fmt.Errorf("--app-id is required")
	case secret == "":
		return fmt.Errorf("$%s is required", appSecretEnv)
	case *chatID == "":
		return fmt.Errorf("--chat-id is required")
	case *socket == "":
		return fmt.Errorf("could not resolve the plugin socket; pass --usher-socket")
	}
	baseURL, err := resolveDomain(*domain)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	router := pluginapi.NewClient(*socket, logger)
	// Only the event types the hub renders; tool_result user events in
	// particular carry whole tool outputs the mirror would just discard.
	router.EventTypes = []string{"user", "assistant", "subprocess.exit"}
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	err = router.Ping(pingCtx)
	pingCancel()
	if err != nil {
		return fmt.Errorf("usher plugin socket %s unreachable (is usher serve running?): %w", *socket, err)
	}

	hub, err := NewHub(newLarkClient(*appID, secret, baseURL), router, Config{
		ChatID:         *chatID,
		StatePath:      *statePath,
		AllowedUserIDs: splitIDs(*allowedUsers),
	}, logger)
	if err != nil {
		return fmt.Errorf("init hub: %w", err)
	}

	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			hub.HandleMessage(ctx, event)
			return nil
		}).
		OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			return hub.HandleCardAction(ctx, event), nil
		})

	// The ws client blocks forever once connected and reconnects on its own;
	// a pre-connect failure (bad credentials) is fatal, so surface it.
	wsErr := make(chan error, 1)
	go func() {
		wsErr <- larkws.NewClient(*appID, secret,
			larkws.WithEventHandler(handler),
			larkws.WithDomain(baseURL),
			larkws.WithLogLevel(larkcore.LogLevelWarn),
		).Start(ctx)
	}()

	logger.Info("usher-lark started", "chat", *chatID, "domain", baseURL, "socket", *socket)

	hubErr := make(chan error, 1)
	go func() { hubErr <- hub.Run(ctx) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-wsErr:
		return fmt.Errorf("lark websocket: %w", err)
	case err := <-hubErr:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

// resolveDomain maps the --domain flag to a base URL.
func resolveDomain(d string) (string, error) {
	switch d {
	case "feishu":
		return lark.FeishuBaseUrl, nil
	case "lark":
		return lark.LarkBaseUrl, nil
	}
	if strings.HasPrefix(d, "https://") {
		return d, nil
	}
	return "", fmt.Errorf(`--domain must be "feishu", "lark", or an https:// base URL (got %q)`, d)
}

func splitIDs(s string) []string {
	var ids []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			ids = append(ids, part)
		}
	}
	return ids
}

// defaultPluginSocket derives the socket path the same way `usher serve`
// does — pluginapi owns the rendezvous, so the two binaries can't drift.
func defaultPluginSocket() string {
	if dir := pluginapi.DefaultDataDir(); dir != "" {
		return pluginapi.SocketPath(dir)
	}
	return ""
}

// defaultStatePath keeps the thread map next to usher's other state.
func defaultStatePath() string {
	if dir := pluginapi.DefaultDataDir(); dir != "" {
		return filepath.Join(dir, "lark-threads.json")
	}
	return "lark-threads.json"
}
