# Session agents

An agent is a set of defaults for creating a session — `cwd`, `backend`,
`model`, and an optional `append_system_prompt` — addressed by a `name` you
choose. That name is the agent's only identifier: it is what the pickers show,
what `--agent <name>` takes in the chat frontends, and what addresses the agent
in the API, so one vocabulary covers every surface.

Names are stored and matched exactly as typed — `Dev` and `dev` are two
agents — and are otherwise free, CJK and accented letters included. Three
things are rejected, each because a surface the name travels through would
silently mangle it: whitespace (chat clients cut `--agent <name>` at the first
space, with no quoting, so the rest would land in the instruction); invisible
characters such as zero-width spaces and bidi marks (two names that look
identical but never match); and `/`, `.` and `..` (the name is a path segment
in `/api/agents/{name}`, which path-normalizing proxies rewrite before usher
sees them).

Manage them from **Settings → Agents** in the Web UI. That writes
`<data-dir>/agents.json`, which is also the file to back up, keep in version
control, or edit by hand:

```json
{
  "agents": [
    {
      "name": "usher-dev",
      "cwd": "/home/dev/lc/usher",
      "backend": "codex",
      "model": "gpt-5.3-codex",
      "append_system_prompt": "Keep changes small and run the relevant tests."
    },
    {
      "name": "quick-claude",
      "backend": "claude",
      "model": "haiku"
    }
  ]
}
```

Selecting an agent fills its defaults, and anything set explicitly alongside
it — in the composer, or as a field in an API request — is an override. A
profile may omit any field: the Settings form always writes a concrete backend
and model, but the API and a hand-edited file may leave them empty, in which
case the value comes from the request or from the backend's own default. The
model sentinel `"default"` beats a configured model and hands the pick back to
the backend; the pickers offer it only when usher cannot read that backend's
catalog.

The authenticated HTTP API exposes the same configuration:

```text
GET /api/agents
POST /api/agents
PUT /api/agents/{name}
DELETE /api/agents/{name}
```

To create from an agent, send its name. Explicit `cwd`, `backend`, and
`model` fields override the configured defaults:

```json
{
  "agent": "usher-dev",
  "initial_message": "Run the tests",
  "model": "gpt-5.3-codex-high"
}
```

GUI and API changes are written atomically and take effect immediately.
Unknown agent names, unavailable backends, and invalid models cause session
creation to fail rather than falling back silently.

## The router session

usher has one **router session**: it holds the session-routing MCP tools
(`list_sessions`, `send_to_session`, `create_session`, …) on top of the ordinary
`show_image`, and every session it routes work to delivers its reply back into
it. It is not created from an agent. usher gives it a routing persona and takes
its backend's local tools away (the recipes below), so it can only route; what
is left to choose is the backend and model it runs on, under Settings → General
or

```sh
curl -sS -X PUT localhost:8080/api/router \
  -H 'Content-Type: application/json' -d '{"backend":"claude","model":"haiku"}'
```

Unset, it runs on the default backend and that backend's default model. It runs
in `router/` under the data directory, which the session lists show last; a
session it creates without naming a directory runs in the system temp
directory instead.

Starting a router session replaces the current one, which becomes an ordinary
session from its next cold start. The setting and the current router session
are kept in `settings.json`, so both survive a restart. Every backend can host
the router: Claude receives the tools as spawn arguments (`mcp-router.json`),
Codex in its thread/start config params, and pi (1.0 or later) through a
generated extension that registers the same MCP server. See [Router MCP](router-mcp.md) for
what the tools promise.

### Restricting local tools

The router cannot answer questions itself: it gets usher's tools and nothing
else — on Claude no built-in tool and no other MCP server (`--tools ""
--strict-mcp-config`), on pi an allowlist (`--tools "mcp__usher__*,show_image"`),
on Codex the recipe below — and its calls to them are approved without asking.
An agent of your own — a "read-only analyst", say — is restricted through
per-backend `extra_args`. This is supervision of the model, not a security
boundary against the authenticated user:

- **Claude**: `["--disallowedTools", "Bash,Edit,Write,NotebookEdit,Read,Glob,Grep,Task,Agent,WebFetch,WebSearch"]`
  removes tools by name; `mcp__usher__*` tools are unaffected. Drop entries to
  taste for an analyst profile.
- **Codex**: no per-tool switches. `["--sandbox", "read-only", "--ask-for-approval", "never"]`
  is the closest: the sandbox blocks writes and `never` stops the model from
  requesting escalation past it (under the default on-request policy an
  auto-approve profile would wave escalations through). The model can still
  read files.
- **pi**: `["--tools", "read,grep,find,ls"]` whitelists built-ins (this set is
  pi's own read-only preset); `["--no-builtin-tools"]` removes them all, which
  for a router agent leaves exactly usher's tools — the strictest
  router shape any backend offers. Note pi's default active set is only
  `read,bash,edit,write`, so prefer the whitelist form over `--exclude-tools`.

The resolved `append_system_prompt` is snapshotted in usher's session metadata,
so editing or removing the agent later does not change an existing session.
For Codex, each session's dedicated app-server process starts with
`-c developer_instructions=...`; cold resume rebuilds the worker with the same
saved value. Claude and pi likewise restore the saved prompt through their
native `--append-system-prompt` option whenever a cold worker is resumed.
