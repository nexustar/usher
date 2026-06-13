# Codex backend (work in progress — branch `feat/codex-backend`)

Adding OpenAI **Codex CLI** as a second session backend alongside Claude Code.
This doc records the validated facts and the integration plan so the work is
resumable. Everything below was verified against a live `codex-cli 0.139.0`
logged in with a ChatGPT subscription (`auth_mode = chatgpt`, no API key), not
just docs.

## Decision: drive Codex the same way as Claude (tmux + interactive TUI)

Not headless `codex exec`. Reasons, in order:

1. **Feature fall-through.** Driving the real TUI means any slash command /
   picker / future Codex feature is usable by the human without usher
   integrating it — usher stays a thin wrapper (its whole identity). `codex
   exec` would expose only the curated subset usher wires up.
2. **Screen view.** `tmux capture-pane` mirrors the rendered TUI for the
   `/screen` endpoint for free; exec has no persistent pane.
3. **`exec` rejects interactive approvals outright.** Verified in source
   (`codex-rs/exec/src/lib.rs:1645-1746`): command/file/apply_patch/permissions
   approvals all return "not supported in exec mode".

The approval-routing that usher needs does **not** require driving the TUI: the
`PreToolUse` / `PermissionRequest` **hook** runs in Codex's shared `core` for
both interactive and exec modes, and a hook can block and return Allow/Deny
before the built-in prompt is raised. So tmux mode still gets clean, hook-based
approvals (same shape as usher's existing Claude PreToolUse hook) — TUI
scraping is only needed for the spawn/resume flow, exactly as for Claude.

Billing is **not** a constraint (unlike Claude's `-p --resume`): `codex exec`
and the TUI both run under the ChatGPT login; `enforce_login_restrictions`
(`login/src/auth/manager.rs:768`) only forces API-key when an enterprise admin
sets `forced_login_method`.

## Validated rollout (transcript) schema — codex 0.139.0

Path: `~/.codex/sessions/YYYY/MM/DD/rollout-<YYYY-MM-DDThh-mm-ss>-<uuid>.jsonl`,
one object per line, flushed after every write (tailable). The `<uuid>` in the
filename is the session id.

Every line: `{"timestamp": <rfc3339>, "type": <t>, "payload": {...}}`.

| `type`         | `payload.type`           | usher uses it for |
|----------------|--------------------------|-------------------|
| `session_meta` | — (first line)           | `id`, `cwd`, `timestamp` → session id / cwd / start time |
| `event_msg`    | `user_message`           | **real user turn** — `payload.message` (clean; no injected `<environment_context>` noise) |
| `event_msg`    | `agent_message`          | **assistant text part** — `payload.message`, plus `phase` (commentary/final_answer) |
| `event_msg`    | `task_started`           | turn start |
| `event_msg`    | `task_complete`          | **turn-done marker** (analog of Claude's `system`/`turn_duration`); has `turn_id`, `last_agent_message`, `duration_ms` |
| `response_item`| `message` (role user/dev/assistant) | skipped — duplicates the clean `event_msg` text and carries injected context |
| `response_item`| `function_call`          | **tool call** — `name`, `arguments` (JSON string, e.g. exec_command `{cmd,workdir}`), `call_id` |
| `response_item`| `function_call_output`   | **tool result** — `call_id` (pairs with the call), `output` (string) |
| `event_msg`    | `token_count`, `turn_context`, `reasoning` | skipped |

Content blocks: user/developer use `input_text`, assistant uses `output_text`.

**Why event_msg for text but response_item for tools:** `event_msg
user_message`/`agent_message` are the clean, user-visible UI strings (what the
TUI showed); injected context (`<environment_context>`, developer
instructions) only appears in `response_item` user/developer messages and has
**no** corresponding `event_msg`, so keying real turns off `event_msg` filters
the noise for free. Tool calls only exist in the `response_item` stream. The
assembler walks all lines in file order and interleaves the two.

## Validated TUI spawn/resume flow (live spike, codex 0.139.0 in tmux)

Driven in a tmux pane exactly as usher's pool drives claude. **The headline: Codex's
flow is far simpler and less race-prone than Claude's** — there is no resume chooser.

- **Trust prompt** (first time only, untrusted cwd): pane shows `Do you trust the
  contents of this directory?` with `› 1. Yes, continue` / `2. No, quit`; default is
  option 1 → accept with **Enter**. Accepting persists the cwd to config.toml, so a
  later resume of the same dir skips it entirely.
- **`codex resume <ID>` with an explicit id resumes straight into the input box —
  NO picker, NO "full session vs summary" chooser.** Prior turns are replayed and the
  composer is ready. This eliminates the entire `❯`-arrow-row / Down+Enter keystroke
  dance (and its lost-Enter / resume-into-compact races) that dominates the Claude
  sender. The only gate on spawn is the one-time trust prompt.
- **Input-ready marker:** the footer line `<model> <mode> · <cwd>` (e.g.
  `gpt-5.5 default · /tmp/x`) — bottom-anchored, always visible when the composer is
  ready (the top banner `OpenAI Codex (v…` can scroll off on a long resumed session).
  Match `· ` + the known cwd. (Claude's analog is `? for shortcuts`.)
- Codex's selection arrow is `›` (U+203A), not Claude's `❯` — matters if any chooser
  ever needs arrow-row matching, but resume currently needs none.

## CLI shape (0.139.0)

- Spawn new: `codex [PROMPT]` (interactive TUI). Trust gate: a non-git / untrusted
  cwd needs `--skip-git-repo-check` (or a trusted entry in config.toml).
- Resume: `codex resume <SESSION_ID> [PROMPT]` (subcommand, not a `--resume` flag);
  `codex resume --last`. Fork: `codex fork`.
- Config: `~/.codex/config.toml` (TOML). Self-register hooks via `hooks.json`
  (JSON, stdlib-friendly) rather than editing TOML, to keep usher stdlib-only.

## Product decision: coexistence (user, confirmed)

usher runs **both backends at once** — one dashboard lists Claude *and* Codex
sessions together. Consequences for the design:

- **Discovery is multi-source**: it scans `~/.claude/projects` and
  `~/.codex/sessions` simultaneously and tags each session with its backend
  (`core.Session.Backend`), determined by which Source found it.
- **A session belongs to one backend for life** — there is NO cross-tool
  migration/handoff. The router routes a send to the matching sender by the
  session's `Backend` tag.
- **New-session backend is chosen via the model picker.** Model names
  disambiguate the backend (claude-* → Claude, gpt-*/o3-* → Codex); only the
  literal "default" collides, so that one entry needs an explicit backend choice
  in the UI. No separate "pick a backend" control.

## Plan / status

- [x] M0 — spike: validate schema/auth/CLI on live 0.139.0; branch; testdata
- [x] M1 — `internal/codexrollout`: rollout → `jsonl.Turn`/`SessionMeta` parser + tests
- [x] M2 — discovery: `Source` interface (Backend/Root/IsSessionFile/SessionID/
      ReadMeta) with ClaudeSource + CodexSource; Discovery is layout-agnostic and now
      **multi-source** (`NewMulti`) — it scans `~/.claude/projects` and
      `~/.codex/sessions` at once, merges them, and tags each `core.Session.Backend`.
      Codex's date-partitioned tree is matched by filename shape, not depth.
- [x] M3 — backend seam in the sender: tailer turn-completion is a
      `tailConfig.turnComplete` func, and the spawn-command / env-scrub / TUI-prep seams
      are the `backend` interface below (claudeBackend now holds the Claude logic).
- [~] M4 — Codex sender. **Spike + backend logic done** (`internal/sender/backend.go`):
      a `backend` interface (spawnCommand / preAssignsID / locate / discoverNewID /
      turnComplete / waitReady) with the `codexBackend` impl, all pure parts unit-tested:
      - spawn new: `env -u CODEX_* codex [-c model=…]` (no id — preAssignsID=false;
        discoverNewID finds the newest rollout in cwd after spawn)
      - resume: `codex resume <id>` (straight in, no chooser)
      - env-scrub: nestedCodexEnv = CODEX_THREAD_ID (+ CI/SANDBOX defensively)
      - locate: `<sessionsDir>/*/*/*/rollout-*-<id>.jsonl`
      - waitReady: trust-accept (Enter) then footer marker
      Wiring done: claudeBackend holds the existing Claude logic (deduped via
      claudeSpawnCommand); Sender delegates waitReady/locate/turnComplete to s.backend;
      pool gained an optional spawnOverride (nil = Claude default → all tests unchanged).
      `NewCodex` done (sets pool.spawnOverride + codex turnComplete); Codex **resume**
      works end-to-end through Send (spawn `codex resume <id>` → composer → inject →
      tail rollout → task_complete), covered by a sender test on the fake tmux.
      TODO: the Codex new-session id handoff (see "Design decision" below) + main.go.
- [ ] M5 — hook adapter: register `PermissionRequest` hook (hooks.json), map decision shape
- [ ] M6 — wiring: backend selection, web/router end-to-end, `usher setup` for codex

## Design decision: new-session identity for Codex

Claude lets usher pick a new session's id up front (`claude --session-id <uuid>`);
the router generates the UUID, registers it in `creating`/`activeSend`, and keys
broker events on it before the jsonl even exists. **Codex has no such flag — it
generates its own UUIDv7** — so that contract can't hold for a new Codex session.

Resume is unaffected (the id is already known; implemented + tested). For *new*
sessions the plan (Option B — backend id is canonical, matching discovery):

1. Spawn `codex` in a window named by a temporary placeholder id; inject the prompt.
2. `codexBackend.discoverNewID(cwd, known)` finds the rollout Codex just created
   (newest under the sessions tree, matching cwd, id ∉ the pre-spawn known set).
3. Rename the tmux window placeholder→real id; re-key the pool LRU/busy.
4. Hand the real id back to the router so it can re-key `creating`/`activeSend`/
   broker (e.g. a synthesized `session.id_resolved` StreamEvent that run() emits
   and runStart/CreateSession act on).

This is the one place the router (not just the sender) changes, so it lands with
the M6 wiring rather than being rushed. Until then, NewCodex drives resume only.

## Open items to verify empirically before M4/M5

- TUI spawn/resume on-screen strings (trust prompt, resume picker, input-ready marker)
- `PermissionRequest` hook coverage on core shell/apply_patch (issue #20204)
- hook stability while blocked for a long time (usher's hook timeout is 7-day scale)
- Codex fork mechanics vs usher's prefix-copy `ForkCopy` (Turn.UUID fork point — Codex
  uses `turn_id`/`forked_from_id`, not per-line uuids)
