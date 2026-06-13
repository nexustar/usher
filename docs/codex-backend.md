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

## CLI shape (0.139.0)

- Spawn new: `codex [PROMPT]` (interactive TUI). Trust gate: a non-git / untrusted
  cwd needs `--skip-git-repo-check` (or a trusted entry in config.toml).
- Resume: `codex resume <SESSION_ID> [PROMPT]` (subcommand, not a `--resume` flag);
  `codex resume --last`. Fork: `codex fork`.
- Config: `~/.codex/config.toml` (TOML). Self-register hooks via `hooks.json`
  (JSON, stdlib-friendly) rather than editing TOML, to keep usher stdlib-only.

## Plan / status

- [x] M0 — spike: validate schema/auth/CLI on live 0.139.0; branch; testdata
- [x] M1 — `internal/codexrollout`: rollout → `jsonl.Turn`/`SessionMeta` parser + tests
- [x] M2 — discovery: `Source` interface (Root/IsSessionFile/SessionID/ReadMeta)
      with ClaudeSource + CodexSource; Discovery is now layout-agnostic. Codex's
      date-partitioned `~/.codex/sessions` is discovered by filename shape, not depth.
- [ ] M3 — `backend.Backend` interface; move Claude spawn/tail/turn-detection behind it
- [ ] M4 — Codex sender: spawn (`codex` / `codex resume <id>`), TUI spawn/resume markers
      (live spike), turn-done via `task_complete`
- [ ] M5 — hook adapter: register `PermissionRequest` hook (hooks.json), map decision shape
- [ ] M6 — wiring: backend selection, web/router end-to-end, `usher setup` for codex

## Open items to verify empirically before M4/M5

- TUI spawn/resume on-screen strings (trust prompt, resume picker, input-ready marker)
- `PermissionRequest` hook coverage on core shell/apply_patch (issue #20204)
- hook stability while blocked for a long time (usher's hook timeout is 7-day scale)
- Codex fork mechanics vs usher's prefix-copy `ForkCopy` (Turn.UUID fork point — Codex
  uses `turn_id`/`forked_from_id`, not per-line uuids)
