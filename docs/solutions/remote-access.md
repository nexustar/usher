# Access usher remotely

usher has no relay or cloud component. To reach it from another device, run a
tunnel on the same machine and point the tunnel at usher's loopback port. usher
can remain on `127.0.0.1:7777`; the tunnel handles remote access.

This guide covers two common choices:

- **Tailscale** for private access from devices in your tailnet.
- **Cloudflare Tunnel** for a public hostname, preferably protected by
  Cloudflare Access.

## Set an usher password

Set a password before exposing usher through either method:

```sh
usher set-password
```

For non-interactive setup, provide the password on standard input:

```sh
printf '%s' 'replace-with-a-strong-password' | usher set-password --password-stdin
```

Plaintext is never accepted as a flag, and an empty password is rejected. Once
configured, every web request goes through usher's login page. Avoid putting a
real password directly in shell history; the interactive command is safer for
normal use.

usher refuses a direct non-loopback bind without a password. A tunnel reaches
the loopback address, however, so that safety check cannot tell whether the
tunnel is private. Set the password yourself.

## Tailscale

Tailscale is the simplest choice when every client device belongs to your
tailnet.

1. Install Tailscale on the usher host and each client, then sign them into the
   same tailnet.

   ```sh
   tailscale up
   ```

2. Start usher and publish its loopback port through Tailscale Serve.

   ```sh
   usher serve
   tailscale serve --bg 7777
   ```

3. Run `tailscale serve status`, open the HTTPS URL it reports, and optionally
   install usher to the home screen as a PWA.

`tailscale serve reset` removes the Serve configuration. Tailscale's flag
spelling has changed between releases, so check `tailscale serve --help` if the
example does not match your installed version.

## Cloudflare Tunnel

Cloudflare Tunnel uses an outbound connection from the usher host, so it does
not require an inbound port. Install
[`cloudflared`](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/)
first.

### Named tunnel with Cloudflare Access

A named tunnel requires a Cloudflare account and a domain managed by
Cloudflare. It lets you put an Access policy in front of usher so visitors must
authenticate before reaching usher's own login page.

```sh
cloudflared tunnel login
cloudflared tunnel create usher
cloudflared tunnel route dns usher usher.example.com
```

Create `~/.cloudflared/config.yml`:

```yaml
tunnel: usher
credentials-file: /home/you/.cloudflared/<TUNNEL-UUID>.json
ingress:
  - hostname: usher.example.com
    service: http://localhost:7777
  - service: http_status:404
```

Start both services:

```sh
usher serve
cloudflared tunnel run usher
```

In the Cloudflare Zero Trust dashboard, create a self-hosted Access application
for `usher.example.com` and restrict it to the identities that should reach
usher. Keep the usher password as a second layer.

### Ephemeral tunnel for a quick test

An ephemeral tunnel needs no account or domain, but produces a public URL with
no Cloudflare Access policy. It relies entirely on the usher password:

```sh
usher serve
cloudflared tunnel --url http://localhost:7777
```

Use this only for short-lived testing.

## Running other local services

Expose editors and other local services on their own hostname or HTTPS port.
This keeps authentication and access policies independent for each service.
See [Use usher with code-server or another editor](code-server.md) for a
complete example.

For details about password storage, cookies, permission channels, and what
usher's authentication does not protect against, read the
[security and trust model](../reference/security.md).
