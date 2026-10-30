# Run usher behind an authenticating proxy

A proxy that signs people in can stand in for usher's password: usher trusts
the identity header the proxy sets and refuses requests without it. A
password, if set, is still required too. Everyone the
proxy admits drives the same sessions as the same OS user; run one usher per
group whose work must stay separate.

## Flags

```sh
usher serve --auth-header X-Forwarded-Email --logout-url /oauth2/sign_out
```

- `--auth-header NAME`: the header the proxy sets once a request is
  authenticated. A request without it gets 401; state changes are logged with
  its value as `actor`.
- `--logout-url URL`: the proxy's logout address, shown as Sign out. Unset
  hides the entry.

With `--auth-header`, usher binds a non-loopback address without a password.
Set one as well (`usher set-password`) to keep a second layer: then a
request needs the header and a signed-in cookie, and Sign out drops the
cookie before going to `--logout-url`.

## Which header

| Proxy | Header |
|---|---|
| oauth2-proxy | `X-Forwarded-Email`, or `X-Auth-Request-Email` with `--set-xauthrequest` |
| Cloudflare Access | `Cf-Access-Authenticated-User-Email` |
| Tailscale Serve | `Tailscale-User-Login` |

The header must be one the proxy sets or strips on every request it forwards,
OPTIONS included. A header the proxy only adds when the client sent none can be
supplied by the client and signs anyone in.

## Only the proxy may reach usher

usher cannot tell the proxy from any other client, so nothing else may reach
its port:

- On one machine, keep usher on `127.0.0.1` and point the proxy at it. The
  [Tailscale Serve and Cloudflare Tunnel setups](remote-access.md) already do;
  add `--auth-header`. Other OS users on that machine can still connect to
  loopback and set the header themselves, which only a password stops.
- In Kubernetes, a network policy that admits only the proxy's pods.

## Check

```sh
curl -si http://127.0.0.1:7777/api/v1/sessions                                         # 401
curl -si -H 'X-Forwarded-Email: me@example.com' http://127.0.0.1:7777/api/v1/sessions  # 200
```

Then open usher through the proxy and start a session; the log shows the
request with `actor=me@example.com`.
