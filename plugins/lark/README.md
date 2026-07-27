# usher-lark

Mirrors usher's sessions into a Lark / Feishu (飞书) group chat, one thread
per session: assistant replies stream into the thread, typing in the thread
sends to that session, and permission prompts are interactive cards with
Allow / Deny buttons.

This is an out-of-process **usher plugin**. It lives in its own Go module so
the Lark SDK's dependency tree (websocket, protobuf) never enters usher's
`go.mod`, and talks to a running `usher serve` through the plugin socket
(`<data-dir>/plugin.sock`). Events arrive over the SDK's websocket long
connection — no public HTTPS endpoint is needed, matching usher's
loopback/Tailscale deployment model.

## Build

```sh
make lark          # from the repo root; builds ./usher-lark
```

## Lark app setup (once)

1. Create a **self-built app** in the [developer console](https://open.feishu.cn/app)
   (or open.larksuite.com for Lark) and enable the **bot** capability.
2. Grant permissions: read/send group messages (`im:message`), upload images
   (`im:resource`), add reactions (`im:message.reactions:write`), and message
   history read for guest thread context. `im:chat:readonly` is optional and
   improves member names; the optional **Get application information**
   permission improves names of other bots/apps in guest transcripts.
3. Under **Events & callbacks**, set the subscription mode to **long
   connection** (长连接) and subscribe to `im.message.receive_v1`; set the
   card callback mode to the long connection as well.
4. Publish the app version, then add the bot to your (private) target group.
5. Get the group's `chat_id` (`oc_...`) — e.g. from the group's bot settings
   or the chat-id lookup in the API explorer.

## Run

```sh
export LARK_APP_SECRET=...       # from the app's credentials page
./usher-lark \
  --app-id cli_xxx \
  --chat-id oc_xxx \
  --allowed-user-ids ou_xxx      # your open_id; empty = any canonical chat member, disables guest sessions
```

`--domain lark` switches to open.larksuite.com (default is feishu).
`usher serve` must already be running on the same machine; the plugin fails
fast when the socket is unreachable and reconnects automatically if usher
restarts.

## Guest sessions

When `--allowed-user-ids` is non-empty, an allowlisted user can mention the
bot in any Lark group where the bot is present:

```text
@bot [--agent agent-name] [--cwd /path] [--backend backend-name] [--model model-name] instruction
```

The first mention creates a new usher session rooted at that Lark message.
Later mentions in the same thread are turn boundaries: messages between
mentions are pulled from Lark history and included as background context, but
they are not sent as prompts by themselves.
When a thread starts from an interactive card, its visible title and content
are included in that background context; mentions inside the card do not
trigger a turn.

Attachments on the first mention are not transferred into the new session.
The session ID—and therefore its managed attachment directory—does not exist
until the initial prompt has already been sent. Rich-text resource elements in
that creation prompt remain textual placeholders. On later turns, ordinary
image/file/media messages and resources embedded in posts are downloaded into
the session attachment directory and passed to the agent by absolute path.
Feishu does not allow downloading resources inside cards or merged-forward
child messages, so those remain textual placeholders.

These flags are accepted only on the creation mention. `--agent` selects a
configured agent; `--cwd`, `--backend`, and `--model` override its defaults.
Without `--agent`, the default cwd is
`--default-cwd` (default `/tmp`). An empty allowlist disables guest
sessions entirely; the canonical mirror chat keeps its existing empty
allowlist behavior.

## Behavior notes

- Assistant text renders as rich-text (post) md paragraphs — markdown in a
  plain bubble, no card frame. Permission / question prompts are card JSON
  2.0 (buttons need cards; requires Feishu client 7.20+, older clients show
  an upgrade hint). A rejected post falls back to plain text so content is
  never dropped.

- Threads are created lazily: a session gets a thread the first time it
  produces output while the plugin runs; historical sessions are ignored.
- The session↔thread map persists in `<data-dir>/lark-threads.json`, so
  threads are re-adopted across restarts.
- Session lifecycle (create / archive / delete) stays in the web UI; Lark is
  read + send only.
- A single-select `AskUserQuestion` shows tappable option buttons; any
  single question can also be answered by typing in the thread. Multi-question
  prompts fall back to the web UI.
- If the plugin restarts while permission prompts are pending, they are
  re-posted once on reconnect (tapping a stale duplicate shows
  "already resolved").
