# Use usher with code-server or another editor

usher can add an **Open in editor** action to each session. Configure it with an
URL template containing `{cwd}`:

```sh
usher serve --editor-url 'https://code.example.com/?folder={cwd}'
```

usher replaces `{cwd}` with the session's working directory. The editor, not
usher, is responsible for interpreting that value and enforcing access.

For a local desktop editor, a protocol URL also works:

```sh
usher serve --editor-url 'vscode://file{cwd}'
```

## Cloudflare Tunnel

Keep code-server separate from usher. Route each service to its own hostname so
they retain separate authentication boundaries:

```sh
cloudflared tunnel route dns devbox usher.example.com
cloudflared tunnel route dns devbox code.example.com
```

```yaml
tunnel: devbox
credentials-file: /home/you/.cloudflared/<TUNNEL-UUID>.json
ingress:
  - hostname: usher.example.com
    service: http://localhost:7777
  - hostname: code.example.com
    service: http://localhost:8080
  - service: http_status:404
```

Then run the three processes:

```sh
usher serve --editor-url 'https://code.example.com/?folder={cwd}'
code-server --bind-addr 127.0.0.1:8080
cloudflared tunnel run devbox
```

An exposed editor is effectively shell access. Apply a Cloudflare Access policy
to `code.example.com` as well, or at minimum keep code-server's built-in
password authentication enabled.

## Tailscale Serve

Use a separate HTTPS port for code-server while keeping both services bound to
loopback:

```sh
usher serve --editor-url 'https://<machine>.<tailnet>.ts.net:8443/?folder={cwd}'
tailscale serve --bg 7777

code-server --bind-addr 127.0.0.1:8080
tailscale serve --bg --https=8443 8080
```

This produces:

- `https://<machine>.<tailnet>.ts.net` for usher.
- `https://<machine>.<tailnet>.ts.net:8443` for code-server.

Avoid mounting either application below a URL subpath. Tailscale Serve's proxy
mode forwards the full request path without stripping the mount prefix, while
both usher and code-server expect to own the URL root.

See [Access usher remotely](remote-access.md) for tunnel setup and authentication
guidance.
