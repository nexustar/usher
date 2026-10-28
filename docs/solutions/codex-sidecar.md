# Debug a Codex app-server with usher as a sidecar

An agent built on `codex app-server` already speaks the protocol usher drives
Codex with. Point usher at that server instead of letting it spawn its own,
and the agent's threads appear in usher as live sessions: you read them as
they run, answer approvals they are waiting on, send a message into one, or
start a thread of your own on the same server.

## Start the server with a listener

usher connects over the control socket or a websocket listener; the default
stdio transport is single-client and cannot be shared.

```sh
# control socket under CODEX_HOME (same host or a shared volume)
codex app-server --listen unix://

# websocket, for a sidecar on another host or container
codex app-server --listen ws://127.0.0.1:4500 \
  --ws-auth capability-token --ws-token-file /run/secrets/codex-token
```

Codex marks the websocket listener experimental, and refuses a non-loopback
one without `--ws-auth`. The control socket is protected by file permissions
and takes no token.

## Attach usher

```sh
usher serve --codex-attach unix:// \
  --codex-sessions-dir /var/lib/agent/.codex/sessions

usher serve --codex-attach ws://127.0.0.1:4500 \
  --codex-attach-token-file /run/secrets/codex-token \
  --codex-sessions-dir /var/lib/agent/.codex/sessions
```

`unix://` alone resolves to the control socket under the CODEX_HOME that
`--codex-sessions-dir` sits in; `unix:///path/to.sock` names one explicitly.

The rollouts are still the transcript: usher reads turns from the jsonl files
under `--codex-sessions-dir` and uses the connection for live state, deltas
and control. The attached server's CODEX_HOME must therefore be visible to
usher, on the same host or through a shared volume. `codex` itself need not
be installed where usher runs.

## What changes when attached

- Every thread the server holds in memory shows as live, whichever client
  loaded it. Opening one subscribes usher to its turns.
- Approvals, permission requests and questions the agent raises reach every
  client of the thread, usher included. The first answer wins, and a question
  another client settles disappears from usher's prompts. Requests only the
  owning client can answer, such as dynamic tool calls and account token
  refreshes, are left alone.
- Sending a message, interrupting, steering or setting a goal acts on the
  production thread. This is the point of attaching for debugging, but it is
  also the reason to protect the socket: whoever reaches it drives the agent.
- usher's own MCP tools are not registered on the attached server, and its
  `--codex-args` apply only to threads usher starts or resumes cold.
- Closing a session in usher ends usher's subscription; the server keeps the
  thread for its other clients and unloads it on its own schedule. Without
  attaching, usher could still show these rollouts read-only; the connection
  adds the live view and the controls.
