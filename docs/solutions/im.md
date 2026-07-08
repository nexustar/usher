# Connect usher to IM

usher can mirror coding-agent sessions into a messaging app, so you can follow
work, send prompts, answer agent questions, and resolve permission requests
without keeping the web UI open.

The IM integration is another frontend to the same local usher process. It does
not run agents itself or move session ownership into the messaging service.

| Integration | Session mapping | Runs as | Best fit |
|---|---|---|---|
| Telegram | One forum topic per session | Built into `usher serve` | A private Telegram group for personal remote control |
| Lark / Feishu | Card-anchored threads | Separate `usher-lark` process | Teams already using Lark or Feishu |

Messages and agent output necessarily pass through the selected IM provider.
Treat anyone allowed to send prompts or press approval buttons as able to drive
your coding-agent sessions.

## Telegram

The Telegram integration mirrors each active session into its own topic in a
forum supergroup. Topics are created lazily on the first mirrored message or
permission request. Their mappings persist in
`<data-dir>/telegram-topics.json`, so restarts reuse existing topics.

### Create the bot and group

1. In Telegram, talk to **BotFather**, run `/newbot`, and save the bot token.
2. Create a private group and enable **Topics** to turn it into a forum
   supergroup.
3. Add the bot as an administrator with permission to manage topics and send
   messages.
4. In BotFather, use `/setprivacy` to disable privacy mode for this bot. usher
   needs to receive ordinary text typed inside a session topic, not just bot
   commands and mentions.

You need the numeric supergroup chat ID and, preferably, the numeric Telegram
user IDs allowed to control sessions. One way to find them is to send a message
that the bot can receive, inspect the Bot API `getUpdates` response before
starting usher, and read `message.chat.id` and `message.from.id`. Supergroup IDs
normally begin with `-100`.

Keep the bot token secret. Anyone who has it can act as the bot.

### Start usher with Telegram enabled

The token is accepted only through `TELEGRAM_BOT_TOKEN`; it is deliberately not
a command-line flag:

```sh
export TELEGRAM_BOT_TOKEN='replace-with-your-bot-token'
usher serve \
  --telegram-group-id -1001234567890 \
  --telegram-allowed-user-ids 123456789
```

Multiple allowed users can be supplied as a comma-separated list:

```sh
usher serve \
  --telegram-group-id -1001234567890 \
  --telegram-allowed-user-ids 123456789,987654321
```

If `--telegram-allowed-user-ids` is omitted, any non-bot member of the configured
group can send prompts and use permission buttons. A private group then becomes
the trust boundary. A user allowlist is safer and also lets blocking permission
or question messages mention the authorized users.

For a long-running installation, add `TELEGRAM_BOT_TOKEN` and the two flags to
the service environment and command defined by your launchd or systemd setup.
The [Quick start](../../README.md#quick-start) links to service examples for
both platforms.

### How the Telegram mirror behaves

- Messages typed in a bound session topic are sent directly to that session.
  The General topic and unbound topics are ignored; create, archive, and delete
  sessions in the web UI.
- Prompts sent from the web UI or main chat are echoed into the session topic.
- Assistant text and supported `show_image` output are mirrored silently;
  successful turn completion sends a normal notification.
- Tool approvals appear with **Allow**, **Deny**, and, when supported,
  **Allow always** buttons.
- Agent questions appear as buttons when possible and can also be answered by
  typing in the topic.
- Deleting a session closes its topic. Archiving a session leaves the topic and
  mapping intact.
- Images larger than Telegram's roughly 10 MB photo limit are not uploaded.

If the integration does not start, check usher's logs. A token without
`--telegram-group-id` is treated as a configuration error. Invalid tokens,
missing topic-management rights, and a still-enabled bot privacy mode are the
most common setup problems.

## Lark / Feishu

Lark and Feishu support is kept outside the main binary so their larger SDK
dependency tree does not enter usher. Build and run the `usher-lark` sidecar
alongside usher; it connects to usher's plugin API and maps sessions to
card-anchored threads.

The plugin has provider-specific application permissions, event subscription,
credentials, build instructions, and interaction details. Follow the complete
[Lark / Feishu setup guide](../../plugins/lark/README.md).

## Security notes

- Prefer a private group or workspace and configure the narrowest available
  user allowlist.
- IM approval controls supervise the agent; they are not a sandbox around an
  authorized user.
- Provider history contains whatever prompts, model output, tool details, and
  images usher mirrors. Do not enable IM mirroring for sessions whose content
  cannot be sent to that provider.
- Rotate a bot or app secret immediately if it is exposed, then restart the
  relevant usher process.

For usher's local authentication boundary, read the
[security and trust model](../reference/security.md).
