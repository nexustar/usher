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

The resolved `append_system_prompt` is snapshotted in usher's session metadata,
so editing or removing the agent later does not change an existing session.
For Codex, each session's dedicated app-server process starts with
`-c developer_instructions=...`; cold resume rebuilds the worker with the same
saved value. Claude and pi likewise restore the saved prompt through their
native `--append-system-prompt` option whenever a cold worker is resumed.
