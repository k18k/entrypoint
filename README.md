# Entrypoint

A self-hosted gateway for publishing services. It runs on a server with a public IP, accepts HTTP(S) traffic with automatic TLS and routes it:

- to services reachable directly or over a VPN (`IP:port`);
- through tunnels to applications on machines without a public IP (behind NAT), with multiple replicas per service.

The data path is handled by Caddy (HTTP, TLS, ACME) and frp (tunnels), as official binaries at pinned versions (`versions.json`). Our own code is the control layer (`entrypointd`), the `entrypoint` CLI, the tunnel connector and the Node.js SDK.

Version **0.1**, for your own use and driven from the CLI. Specification: [docs/specification.md](docs/specification.md). Results of the prototype experiments: [docs/results.md](docs/results.md).

## How it works

```
 internet ──443/80──▶ Caddy ──▶ IP:port (e.g. over a VPN)
                        │
                        └──▶ 127.0.0.1:<service port> ── frps ══ TLS tunnel :55555 ══ frpc ── app
                                                          │ (replica group)
                                          authorization plugin: entrypointd
```

- Each **service** gets a fixed port from the 20000–29999 pool (127.0.0.1 only). The service's replicas form a frp group on that port, and frps spreads **TCP connections** between them.
- A service **token** only allows connecting replicas of that service. Permissions are checked by the gateway, never by the client.
- The tunnel client **always verifies** the gateway certificate against the private CA stored in the connection bundle.
- Caddy and frps start from the last applied configuration on disk, so traffic keeps flowing even without entrypointd.

## Requirements

**Gateway server:**

- Linux with systemd (Debian 12/13, Ubuntu 24.04), x86_64 or arm64;
- a public IPv4 address and open TCP ports **80**, **443** and **55555** (at the provider and in the system firewall);
- a domain where you can set A records.

**Development and building from source:** Go 1.27, Node.js 22+, Python 3, Docker (for e2e tests). Packages: `make dist sdk`, full release: `make release`.

**Applications:** Node.js 22+ for the SDK; any language through the connector CLI.

## Installing on the server

1. Set the gateway DNS record, e.g. `A tunnel.example.com -> <server IP>`. The gateway name is fixed: tunnel clients connect by name, not by IP.
2. Install the programs from the [GitHub release](https://github.com/k18k/entrypoint/releases). `install.sh` downloads the package and verifies its SHA256:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/k18k/entrypoint/master/scripts/install.sh | sudo sh
   ```

   Another version: `ENTRYPOINT_VERSION=0.1.0`; a package from disk: `ENTRYPOINT_TARBALL=./entrypoint_0.1.0_linux_amd64.tar.gz`.
3. Set up the gateway:

   ```bash
   sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com
   sudo entrypoint status
   ```

The installer:

- creates the system users `entrypoint`, `entrypoint-caddy` and `entrypoint-frps`;
- creates the private tunnel CA and the certificate for port 55555;
- sets up systemd units tied together in `entrypoint.target`.

Running `entrypoint install` again is safe: it updates the units and removes nothing.

**Practice on Let's Encrypt staging first.** A route with `--acme-staging` gets a test certificate and does not use up the limit of 50 certificates per domain per week.

| Path | Contents |
| --- | --- |
| `/opt/entrypoint/releases/<version>`, `/opt/entrypoint/current` | Programs |
| `/etc/entrypoint/entrypoint.json` | Startup configuration (no secrets) |
| `/var/lib/entrypoint` | SQLite database (revisions, revocation log), private CA, Caddy storage |

## Publishing services

All admin commands require `sudo`. Every configuration change creates a numbered revision and prints the result of applying it.

### A service reachable by IP (e.g. over a VPN)

```bash
sudo entrypoint route add app.example.com --to 10.8.0.2:3000
sudo entrypoint route add app.example.com/api --to 10.8.0.3:8080 --strip-prefix
sudo entrypoint dns check          # which DNS records are needed and whether they exist yet
```

WebSockets, SSE and HTTP/2 work without extra configuration.

### An application through a tunnel

On the gateway:

```bash
sudo entrypoint service add api --health /health
sudo entrypoint route add api.example.com --service api
sudo entrypoint token create --service api      # prints ENTRYPOINT_CONNECT only once
```

`ENTRYPOINT_CONNECT` contains the gateway name, port, trusted CA and token. Store it like a password. Each replica (or each machine) can have its own token, which you can revoke independently.

In your app, install the SDK from npm:

```bash
npm install @k18k/entrypoint-ts
```

The same package is in GitHub Packages (`@k18k:registry=https://npm.pkg.github.com` in `.npmrc`; GitHub requires a token with `read:packages` even for public packages).

The SDK downloads the connector and frpc for your platform from the matching GitHub release and verifies their SHA256. npm 11 does not run dependency install scripts by default (`npm warn install-scripts`); the SDK then downloads the connector on the first `expose()`/`tunnel()`. To download it at install time: `npm install-scripts approve @k18k/entrypoint-ts`.

**`node:http` adapter (recommended).** It gives a full drain: `Connection: close`, and SSE and WebSockets closed with code 1001. It accepts any `(req, res)` handler, so it works with plain `node:http` and with framework apps (an Express app, `app.callback()` in Koa).

```js
import { expose } from "@k18k/entrypoint-ts/http";

const handler = (req, res) => {
  if (req.url === "/health") return res.end("ok");
  res.end("Hello from the tunnel");
};

// A server on 127.0.0.1:0 and a tunnel; SIGTERM starts the drain and ends the process.
const conn = await expose(handler, { service: "api", healthCheck: "/health" });
// WebSockets: new WebSocketServer({ server: conn.server }); pass them in the websockets option.
```

**Base contract.** A tunnel to an address where the application is already listening:

```js
import { tunnel } from "@k18k/entrypoint-ts";

const t = await tunnel({ service: "api", upstream: "http://127.0.0.1:3000", healthCheck: "/health" });
process.on("SIGTERM", () => t.drain({ timeoutMs: 30_000 }).then(() => process.exit(0)));
```

**Any language (CLI):**

```bash
ENTRYPOINT_CONNECT=... npx entrypoint-tunnel tunnel --upstream http://127.0.0.1:3000 --health /health
```

Run the application on two machines with tokens of the same service, and the gateway will spread traffic between the replicas. Stopping one (SIGTERM, drain) does not interrupt the service.

**Applications in Docker (instead of `cloudflared`).** The connector runs as a separate container next to the application, using the base contract. Image `ghcr.io/k18k/entrypoint-connector` (amd64 and arm64), or build locally with `make connector-image`:

```yaml
services:
  app:
    image: my-app
  tunnel:
    image: ghcr.io/k18k/entrypoint-connector:0.1.1
    command: ["--upstream", "http://app:3000", "--health", "/health", "--drain-timeout", "20s"]
    environment:
      ENTRYPOINT_CONNECT: ${ENTRYPOINT_CONNECT}   # from a .env file kept outside the repository
    stop_grace_period: 30s                        # longer than drain-timeout
    restart: unless-stopped
```

`docker compose stop` sends SIGTERM, and the connector withdraws the replica from the group and exits after `drain-timeout`.

### Drain and replicas: what to know

- Replicas share traffic **per TCP connection**, not per request. A new replica receives traffic on new connections. Under constant traffic, existing keep-alive connections (4 s idle) stay with their current replica.
- A replica with a failing health check drops out of the group. The connector then restarts frpc to cut the connections pinned to it, and the replica comes back after a successful check.
- The base contract does not control the application's connections: on drain it waits up to `drainTimeout` (30 s by default). The `node:http` adapter finishes the drain once the connections close.
- Restarting or updating Caddy drops all connections, and restarting frps drops all tunnels. WebSocket and SSE clients must be able to reconnect.

## Tokens and revocation

```bash
sudo entrypoint token list
sudo entrypoint token revoke <id>        # new logins are rejected immediately, the session is cut at the next heartbeat (up to ~10 s)
sudo entrypoint token revoke <id> --now  # immediately: restarts frps, a short outage of all tunnels
```

Revocations go to a separate append-only log. Neither a revision rollback nor a restore undoes them.

## Operations

| Command | What it does |
| --- | --- |
| `entrypoint status` | Units, revision, services with replicas, certificate counter, warnings |
| `entrypoint service add/rm/list` | Tunnel services; the port of a removed service stays reserved forever |
| `entrypoint route add/list/rm` | Routes `host[/prefix]` to `--to IP:port` or `--service`; options `--strip-prefix`, `--max-body 50MB`, `--header-timeout 60s`, `--acme-staging`, and for wildcards `--tls-name`, `--tls-on-demand` |
| `entrypoint token create/list/revoke` | Service tokens |
| `entrypoint dns check` | Required DNS records and their current state (the gateway does not change DNS) |
| `entrypoint ca status`, `ca rotate start\|switch\|finish` | The private tunnel CA and its three-step rotation |
| `entrypoint revision list`, `revision show <n>` | Change history and apply errors |
| `entrypoint backup -o file.age` | Password-encrypted backup (database, CA, certificates, configuration) |
| `entrypoint restore file.age` | Restore onto an installed gateway of the same version |
| `entrypoint logs entrypointd\|caddy\|frps [-f]` | Logs from journald |
| `entrypoint uninstall [--purge]` | Removes the units and programs; `--purge` also removes data |

Commands with script-friendly output have `--json`. Changes accept `--base-rev <n>`: the change is rejected if a newer revision has been created in the meantime.

**Backup and restore.** Take backups regularly and keep them off the server, separate from the password (`ENTRYPOINT_BACKUP_PASSWORD`, or `--password-file` for automation). Restoring on a new machine:

```bash
sudo ENTRYPOINT_TARBALL=... sh install.sh
sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com
sudo entrypoint restore entrypoint-backup.age
```

Restore **invalidates all restored tokens**, because a backup does not know about later revocations. Issue new tokens and update DNS if the IP address changed. Clients change nothing except the token: the CA and gateway name stay the same.

**Updating in v0.1 is manual:** backup, `install.sh` with the new version (`ENTRYPOINT_VERSION=...`), `entrypoint install`.

**Tunnel CA rotation:**

1. `ca rotate start`: new tokens carry both CAs;
2. update the clients (new tokens or `entrypoint-connector rebundle --ca`);
3. `ca rotate switch`: the server switches to the new CA, and `ca status` shows clients with the old bundle;
4. `ca rotate finish`.

## Security

- The Caddy admin API runs only on a Unix socket. The frps API, the plugin and the service ports listen only on 127.0.0.1. Only ports 80, 443 and 55555 are reachable from the network (e2e suite 1).
- Tokens are 256 random bits; only a hash is stored in the database. Tokens and connection bundles never reach logs or error messages (e2e suite 12). The connector passes the token to frpc only through the process environment.
- The gateway ignores address headers from the client (`X-Forwarded-*`, `X-Real-IP`, `Forwarded`) and sets `X-Forwarded-For/Proto/Host` itself.
- The frps plugin allows only `tcp` proxies, and always overwrites the group, group key and port with the service values from the token. The remaining fields are checked against an allowlist.
- A wildcard route (`*.example.com`) does not issue certificates by itself: names are given with `--tls-name a.example.com` (repeatable). On-the-fly certificates for any subdomain are an explicit choice (`--tls-on-demand`) with a limit of 10 new names per hour, so as not to exhaust the Let's Encrypt limit.
- A token holder is a trusted operator of their own service: a modified client can disable the health check, but harms only its own service.

## SDK development

`sdk/node`: TypeScript 7 with `erasableSyntaxOnly` and `isolatedDeclarations`, `tsdown` build, `oxlint` lint, `oxfmt` formatting:

```bash
cd sdk/node
npm install
npm run check      # typecheck (tsc --noEmit), oxlint, oxfmt --check
npm run build      # tsdown -> lib/
```

## Tests

```bash
make test                     # unit tests
make e2e                      # e2e tests of our code in Docker (~4 min, 4 parallel copies of the environment)
make e2e-experiments          # experiments: Caddy and frp behavior (~3 min; after changing versions.json)
make e2e SUITE=Routing        # one suite: Smoke Install TLS Routing Tunnels Replicas State Revoke Ops SDK Secrets
make e2e RUN='^TestOps_DNSCheck$' SHARDS=1 KEEP=1   # one test, the environment is kept
```

`make e2e` checks our code and our configuration: the installer, revisions, routing, the plugin and permissions, drain, the SDK, backup, secret protection. It does not need a VPS. The gateway is installed with the real `install.sh` on a clean container with systemd. Pebble stands in for Let's Encrypt, and CoreDNS for public DNS.

`make e2e-experiments` repeats the experiments from the specification that test Caddy and frp themselves: certificate renewal, atomicity of `/load`, frp groups and heartbeat, resilience to packet loss. Results are in [docs/results.md](docs/results.md).

Each copy of the environment has its own compose project and the subnets `10.77.N.0/24` and `10.78.N.0/24`. Tests are assigned to copies based on timings from the previous run (`e2e/durations.json`, updated automatically). The artifacts of each test (logs, Caddy and frps configurations, database state) go to `e2e/artifacts/`.

## What is not in v0.1

- Web panel, Fastify and Hono adapters, automatic updates with rollback and maintenance mode (v0.2.0).
- Full failure tests, ARM64 tests, a performance benchmark (v0.2.0).
- Per-IP connection limits on the public side (decision before v0.2.0).
- Static site releases (v0.3), TCP/UDP forwarding and TURN (v0.4).
- Wildcard certificates via DNS-01, OIDC, HA, a built-in VPN (v1).
- Publishing to the npmjs.com registry: the SDK is available from git and from GitHub Packages.
- The connector for Linux arm64, macOS and Windows is in the release, but has no e2e tests.

## License

[Apache-2.0](LICENSE). Caddy and frp, used as official binaries, have their own licenses (both Apache-2.0).
