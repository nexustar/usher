# usher — instructions for coding agents

usher is a thin web router over local coding-agent sessions (Claude Code,
Codex). It renders conversations; it does not browse the machine.

## Dependencies: stdlib-first

Default to the Go standard library. A third-party dependency is acceptable
only when it is trustworthy AND significantly simplifies the code — ask before
adding one, and prefer writing 50 more lines over adding a dep. An auditable
dependency chain is a stated project goal.

Do not introduce:

- HTTP frameworks (gin/echo/fiber/chi) — use `net/http` + Go 1.22 ServeMux
- CLI frameworks (cobra/urfave-cli) — use stdlib `flag`
- Loggers (zap/logrus/zerolog) — use `log/slog`
- Testing libs (testify) — use stdlib `testing`
- Config libs (viper) — use `encoding/json`
- SDKs for LLM/IM/etc. APIs — write a small HTTP client instead
- Utility libs (lo, samber/*) — write what you need
- Frontend frameworks or build tooling (React/Vue, npm, bundlers, Tailwind)

## Agent behavior

When backend behavior is uncertain, inspect the local upstream sources before
guessing: `../claude-agent-sdk-python`, `../codex`, and `../pi`.

Running usher to check a change is fine, but the maintainer's own instance
serves real sessions on the default port. Never disturb it:

- Give a throwaway instance a spare port and its own `--data-dir`,
  `--projects-dir`, `--codex-sessions-dir` and `--pi-sessions-dir`; any flag
  left at its default is shared with the live instance.
- Stop it by the PID you started, never `pkill`/`killall` by name — that
  matches the maintainer's `usher serve` too.

## Frontend

Priority order: maintainability > few dependencies > performance > visual
polish. The UI is a plain-JS, client-rendered, hash-routed SPA under
`internal/web/static/` — no build step, no npm. When a library is genuinely
needed, vendor a popular, self-contained, minimal one.

- Polled `innerHTML` rebuilds restart CSS animations. Cache the last rendered
  HTML and skip the write when unchanged.
- Never bump the `CACHE` const in `sw.js` as part of an edit. The service
  worker is stale-while-revalidate, so changes propagate on their own; bumping
  is a per-deploy-batch decision made by the maintainer, not per commit.
- Every focusable text input/textarea needs `font-size` ≥ 16px (iOS Safari
  zooms on focus below that). Keep one set of type sizes in base rules — no
  per-breakpoint font/padding overrides of the same selector.
- UI copy is always English.

## Product boundaries

- **usher renders conversations; it does not browse the machine.** It may
  dereference references that appear in a transcript (e.g. images shown by a
  tool), but it never offers filesystem navigation or interpretation. The only
  exception is one tmux shell per conversation, opened in its cwd and limited
  to whole-text input plus fixed control keys. It is not a filesystem API or
  security boundary. Still decided and out of scope:
  file-read or directory-listing endpoints, diff views, syntax highlighting,
  arbitrary send-keys, path linkification in prose, a native VSCode extension.
- **The main-chat agent never answers substantive questions itself.** Its only
  local job is session management (list, route, focus, permission handling,
  create). Anything else — however trivial-looking — is forwarded to a
  session; usher's value is the full-context session behind it.
- **The permission UI supervises the model; it is not a security boundary
  against the authenticated user.** The real boundary is web auth (password,
  cookie, socket isolation). Don't add in-tool gates to protect users from
  themselves; real isolation is perimeter-level (container/VM/separate user).
- Sessions are a derived view of the jsonl files on disk — no SQLite, no
  session registry.
