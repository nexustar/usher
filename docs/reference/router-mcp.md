# Router MCP prototype

`usher mcp-stdio` normally exposes only `show_image`. The experimental
`--router-tools` flag additionally exposes usher's session-routing tools:

- `list_sessions`, `resolve_session`, `send_to_session`
- `read_session_transcript`, `search_sessions`, `create_session`
- `list_pending_interactions`, `respond_to_interaction`
- `set_auto_approve`, `set_archived`

The flag is intentionally absent from the MCP configuration usher injects into
ordinary coding sessions. Only the session designated as usher's router gets a
registration with it: for Claude a second config file — `mcp-router.json`,
next to `mcp.json` in the data dir — passed as spawn arguments; for Codex the
same registration as config overrides in its thread/start and thread/resume
params; for pi a generated bridge extension (pi has no MCP) that bakes in the
tool catalog and forwards each call to a spawned
`usher mcp-stdio --router-tools`. Set the designation with a
[router agent](agents.md#router-agents); the flag stays available by hand for
driving a `claude` started outside usher.

## Replies come back asynchronously — and not through the model

`send_to_session` and `create_session` return as soon as the prompt is
accepted. They never wait for the turn: turns run for minutes or hours, and
neither a held request nor the host agent's own tool-call timeout could outlast
them.

When the turn completes, usher splits what happens next in two — the same split
the web main chat has always had:

- **The user's copy is verbatim and program-delivered.** usher renders the
  routed session's answer under the routing tool call that caused it: the
  transcript already records the call and its target id, and the answer is in
  that session's own transcript, so no new state is stored. The router model is
  never the courier, and never gets the chance to paraphrase.
- **The router's copy is a note, folded into its next prompt.** `[session <id>
  replied] <excerpt>` (`[session <id> failed] …` when the turn errored, so a
  dead turn is never silent) is prefixed to whatever the user types next, and
  the queue is cleared. Nothing is sent when the reply lands: prompting the
  router would start a turn whose only product is a restatement of text the
  user can already read.

The note is excerpted, because it exists to say *what finished*, not to carry
the reply — `read_session_transcript` is there for the rest. The queue is
in-memory: an usher restart drops pending notes, and the transcripts remain the
record.

The caller never names the inbox. usher resolves it: the designated **router
session**, recorded in `sessionmeta` and resolved inside the router's
`RouteToSession`/`RouteNewSession`.
Holding the routing tools is what makes a session the router, so asking to
relay is all the MCP has to say.

The inbox must be a session **usher runs** — running the session's process is
the only way usher can deliver into it. That is the catch for a hand-started
router:

- A `claude` you launched yourself in a terminal is not reachable. usher would
  have to spawn a second process resuming the same jsonl, which the terminal
  copy neither sees nor tolerates. With no router session designated, sends
  still land and every ack reports `relayed: false`, which the tool result
  turns into "collect it with read_session_transcript".
- A session created from a router agent is reachable: usher minted its id,
  spawned it with the routing tools, and recorded the designation, so the relay
  closes the loop with no polling anywhere.

On the control socket the whole mechanism is one optional boolean: `relay` on
`POST /v1/sessions/{id}/send` and `POST /v1/sessions`. Omitted, both keep the
fire-and-forget shape (204 / 202) the IM sidecars already use.

Because a folded prompt is no longer byte-identical to what the user typed, the
IM hubs match their own echo by that preamble (`imutil.SamePromptEcho`) — worth
knowing before adding any other server-side prompt rewriting.

## Test through usher

Configure an agent with `"router": true` and a model, then create a session from
it — in the composer's agent picker, or over the API:

```sh
curl -sS -X POST localhost:8080/api/sessions \
  -H 'Content-Type: application/json' \
  -d '{"agent":"main","initial_message":"List my active sessions."}'
```

usher spawns it with the routing tools, designates it, and relays every routed
reply back into it. Its own jsonl makes it a session like any other, so it
appears in its own `list_sessions` output — it must not route work to itself.

## Test a hand-started Claude Code

Keep `usher serve` running. Build the working tree, then create a temporary MCP
configuration using absolute paths:

```sh
go build -o /tmp/usher-router-mcp ./cmd/usher

cat >/tmp/usher-router-mcp.json <<'JSON'
{
  "mcpServers": {
    "usher-router": {
      "command": "/tmp/usher-router-mcp",
      "args": ["mcp-stdio", "--router-tools"],
      "env": {
        "USHER_HOOK_SOCK": "/home/USER/.local/share/usher/hook.sock"
      }
    }
  }
}
JSON

claude --model haiku \
  --mcp-config /tmp/usher-router-mcp.json \
  --append-system-prompt \
  'Act only as an usher session router. Never answer substantive questions yourself; route them to an usher session.' \
  'Use the usher router tools to list my active sessions.'
```

Replace `USER` and the data-directory path as needed. If `usher serve` uses
`--data-dir`, pass the `hook.sock` inside that directory.

The MCP process derives `plugin.sock` from the directory containing
`USHER_HOOK_SOCK`. Both sockets are created by the same `usher serve` instance.

Start it from a plain terminal. Inside an usher-managed session it would
inherit that session's `CLAUDE*` environment. usher cannot deliver into a
session it does not run, so this router has no inbox: every ack reports
`relayed: false` and results have to be collected with
`read_session_transcript`.
