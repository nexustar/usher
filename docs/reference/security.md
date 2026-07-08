# Security and trust model

usher is a single-user tool. Web authentication protects access through its
HTTP interface; it is not a security boundary against software already running
as the same operating-system user.

## Web authentication

Set or rotate the web password from a terminal:

```sh
usher set-password
```

- The password is stored as an Argon2id hash in `<data-dir>/auth.json`, with
  mode `0600`.
- A separate 32-byte HMAC secret is generated at `<data-dir>/secret` on first
  start and reused across restarts.
- The HttpOnly, SameSite=Lax login cookie is derived from the secret and current
  password hash. There is no server-side session table.
- Changing the password changes the hash and invalidates every existing login
  cookie. This is the way to sign out other devices.
- Login attempts are rate-limited per client IP. After five failures, the delay
  grows exponentially from one second to a maximum of 60 seconds. A successful
  login resets the counter.

usher refuses to bind directly to a non-loopback address unless a password is
configured. A reverse proxy or tunnel connecting to loopback does not trigger
that check, so remote-access setups must set a password explicitly.

## Agent permissions

The web permission UI supervises agent requests; it does not protect the
authenticated user from themselves.

Native backend protocols carry permission requests between usher and managed
agents. The remaining Claude hook channel uses a Unix domain socket with mode
`0600` and never traverses the web port. Approval persistence is local to the
usher data directory.

## What usher protects against

With a password configured, usher's web authentication protects against:

- Other devices on the LAN or tailnet reaching the web port.
- A compromised tailnet peer attempting to use usher.
- Accidental direct exposure through `--addr 0.0.0.0`; startup is refused until
  a password exists.
- A neighboring container sharing the host network namespace but not the
  filesystem, because it cannot read authentication state or reach the hook
  socket.

## What usher does not protect against

Code running as the same operating-system user can read usher's authentication
files, read native agent transcripts, or invoke the agent CLIs directly. The OS
user account is therefore the trust boundary.

If you need isolation from local code, use a dedicated user, container, virtual
machine, or sandbox. An editor exposed through the same host should be treated
as shell access and protected independently.

usher also is not a filesystem security API. It does not provide arbitrary file
reading or directory-listing endpoints. Its explicit exceptions — uploaded
files, transcript image references, editor deep links, and one constrained tmux
shell per conversation — are convenience features inside the same single-user
trust boundary.

For deployment examples, see [Access usher remotely](../solutions/remote-access.md).
