# entrypoint

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
