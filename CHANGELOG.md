# entrypoint

## 0.2.0

### Minor Changes

- 1eeb3fe: Built-in VPN: `entrypoint install --vpn-name vpn.example.com` runs Headscale behind Caddy, with its embedded DERP relay, and joins the gateway as a node. Routes can then point at any machine in the VPN (`--to 100.64.0.2:3000`). `entrypoint vpn key` issues join keys for the regular Tailscale client, and `vpn nodes` and `vpn rm` manage machines. Backups include the VPN state. With the VPN the gateway also needs UDP 3478 and 41641 open.
- 0c4d787: `entrypoint-connector vpn` joins the gateway's VPN without Tailscale or root: `--name vatro-postgres --expose 5432` publishes a local port as `vatro-postgres:5432`, and `--forward vatro-postgres:5432` makes it reachable on `127.0.0.1:5432` of another machine. The bundle (`ENTRYPOINT_VPN`) comes from the panel or `entrypoint vpn key --bundle`. `install-connector.sh` (a release file) installs the connector on a Linux machine and runs it as a systemd service, and the connector image now takes the mode as its command (`tunnel` stays the default). VPN machines resolve each other by name (MagicDNS, `<name>.ep.internal`).
- 5abbea0: Fastify and Hono adapters: `exposeFastify(app, opts)` from `@k18k/entrypoint-sdk/fastify` (waits for `ready()`, passes WebSocket upgrades to the app's server) and `exposeHono(app, opts)` from `@k18k/entrypoint-sdk/hono` (with `@hono/node-server`). Both drain like the `node:http` adapter: `Connection: close`, SSE streams closed and WebSockets closed with code 1001. Tested on Node.js 22 and 24.
- 6c75c36: Per-IP connection limits on the public side, as kernel rules (nftables) that the installer manages in a table of its own: at most 256 concurrent connections per client address on 80/443 (IPv6 per /64) and 100 new connections per second on 80/443 and the tunnel port. Each service is capped at 1024 concurrent requests in Caddy, so one service cannot take all of frps. Set them with `--per-ip-connections`, `--per-ip-rate` and `--service-connections` (0 turns a limit off). `install.sh` installs nftables when it is missing and apt is available; otherwise the installer warns and the per-IP limits are off.
- a633c6c: The SDK is now `@k18k/entrypoint-sdk` on npm (with provenance) and on GitHub Packages; install it with `npm install @k18k/entrypoint-sdk`. Update imports from `@k18k/entrypoint` to `@k18k/entrypoint-sdk` (and `@k18k/entrypoint/http` to `@k18k/entrypoint-sdk/http`). The connector and frpc now come in per-platform optional dependencies (`@k18k/entrypoint-sdk-linux-x64` and so on), like esbuild: no install script, no download at run time, and `npx entrypoint-tunnel install-connector` and `ensureConnector()` are gone. The `.tgz` file is no longer attached to GitHub releases.
- a9f6909: `entrypoint update` updates the gateway in place: it checks the release's Ed25519 signature, turns on maintenance mode (configuration changes are refused, token revocations still work), copies the database, runs the new version's migrations, switches versions, restarts the services in order and watches them for 60 seconds. If anything fails it rolls back to the previous version and database and keeps every revocation made meanwhile. `entrypoint maintenance on|off` controls maintenance mode by hand. Releases now carry a signed `SHA256SUMS`, which `install.sh` verifies too (it needs openssl).
- 0c4d787: Web panel: `entrypoint install --panel-name panel.example.com` serves it through Caddy with an automatic certificate. Sign-in is with passkeys only (WebAuthn: a password manager such as Proton Pass, or a security key such as a YubiKey): `entrypoint panel register` prints a one-time link valid for 60 seconds that adds one, and `entrypoint panel keys` lists and revokes them. You can keep several passkeys (a password manager and a security key as a backup) and add or remove them on the panel's Passkeys page. From the panel you create an app with its hosts in one step and get its connection bundle with ready commands for a Linux machine, the Node.js SDK or Docker Compose; manage routes, tokens and VPN machines; issue VPN keys; and check DNS records and revisions. Sessions use an HttpOnly, Secure, SameSite=Strict cookie, changes need a CSRF token and the panel's Origin, and failed sign-ins are limited per IP address.
- a633c6c: Gateway hardening after a security review:
  
  - Wildcard routes no longer get certificates on demand for any subdomain by default. Opt in with `--tls-name <host>` (certificates only for the listed names, obtained up front) or `--tls-on-demand` (any subdomain, at most 10 new certificates per hour). With neither, its subdomains get no certificate.
  - Per-token limits on the tunnel port: 30 logins per minute, 20 concurrent sessions, 60 proxy operations per minute.
  - Tokens revoked while entrypointd was down stay revoked after it restarts.
  - `backup restore` rejects archives with paths outside the target directory and keeps the current host's paths and addresses, so a crafted backup cannot run code as root.

### Patch Changes

- 1fd17fc: All CLI, daemon, connector and SDK messages, logs and docs are now in English.
- 893688b: More fixes from the Strix security review:
  
  - Revoked tokens stay rejected when a new configuration snapshot is applied.
  - A rollback with no previously applied revision applies an empty authorization snapshot instead of keeping the failed one.
  - Route prefixes conflict case-insensitively (`/API` and `/api` on one host are rejected).
  - `entrypoint backup` requires a password of at least 12 characters from every source (flag, file, prompt).
  - The connector rejects `{{` and `}}` in `--health`, `--upstream`, `--service` and the gateway name from the bundle.

## 0.1.1

A naming fix in the SDK.

### Changes

- `@k18k/entrypoint/http` adapter: `expose(handler, opts)` replaces `connectTo(handler, opts)`. `connectTo` and the `ConnectOptions` type stay as deprecated aliases, so code written for 0.1.0 keeps working.

```js
import { expose } from "@k18k/entrypoint/http";

await expose(app, { service: "api", healthCheck: "/health" });
```

Connector image: `ghcr.io/k18k/entrypoint-connector:0.1.1`. The gateway and the connector have no functional changes compared to 0.1.0.

## 0.1.0

The first usable version of Entrypoint, for personal use and driven from the CLI: a self-hosted gateway for publishing services (HTTP(S) with automatic TLS, tunnels to apps behind NAT, multiple replicas per service).

### Gateway installation

```bash
curl -fsSL https://raw.githubusercontent.com/k18k/entrypoint/master/scripts/install.sh | sudo sh
sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com
```

Requirements: Linux x86_64 with systemd, TCP ports 80, 443 and 55555.

Connector image for apps in Docker: `ghcr.io/k18k/entrypoint-connector:0.1.0`.

### Contents

- Host+prefix routing to `IP:port` upstreams (e.g. over a VPN) and to tunnel services; WebSockets, SSE, HTTP/2.
- frp tunnels with per-service tokens and a private CA (the client always verifies the gateway), token revocation and session teardown.
- Replica drain: base contract, `node:http` adapter, CLI and connector image.
- Configuration revisions with validation, rollback and drift control; encrypted backup and restore.
- Versions: Caddy 2.11.7, frp 0.71.1 (official binaries).

### Limitations

- Gateway: linux amd64 only (ARM64 in v0.2.0). The connector builds for Linux arm64, macOS and Windows, but has no e2e tests there.
- Replicas share traffic per TCP connection, not per request.
- No dashboard and no automatic updates (v0.2.0).

### File verification

SHA256 sums of all files are in `SHA256SUMS`; `install.sh` and the SDK check them before installing.
