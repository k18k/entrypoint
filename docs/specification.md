# Entrypoint — specification

Oct 18, 2026 · @Krystian

Complete project specification after two rounds of review. It replaces the earlier v0.1 and v0.2 versions and the separate prototype plan and test cards.

## 1. Purpose and positioning

Entrypoint is a self-hosted gateway for publishing services. It runs on a machine with a public IP, accepts HTTP(S) traffic and routes it to services reachable directly, over a VPN, or through a tunnel from machines without a public IP.

It takes over selected features of services such as Cloudflare Tunnel: a reverse proxy with automatic TLS, tunnels for applications behind NAT, and in later stages hosting of static frontends. It does not provide a global network, resilience to the loss of a location, or protection against link saturation.

**External dependencies that remain:** the DNS provider, the certificate authority (Let's Encrypt by default) and the server provider's network. All published services share one gateway, so its failure affects all of them at once. This is a deliberate trade-off.

**Design principle.** The data path is handled by proven projects: Caddy (HTTP and TLS) and frp (tunnels). Our own code is the control plane: state, permissions, replica lifecycle, revocations, operations, SDK and CLI. A custom tunnel protocol is not planned.

## 2. Versions and scope

The first usable version is v0.1, intended for personal use and controlled through the CLI. The first public release is v0.2.0, with a panel and full failure tests.

&#91;embedded content: stages · prototype, v0.1, v0.2.0 gate, later versions\]

| Version | Nature | Scope | Release condition |
| --- | --- | --- | --- |
| v0.0.x | Prototype | Variant B core controlled from the CLI, experiments 1–3 | Experiment results recorded (section 17) |
| v0.1 | For personal use | Installer, HTTP(S) routing and WebSockets, IP:port upstreams, tunnels with per-service tokens, private CA, token revocation, connector, SDK with the node:http adapter, CLI, backup and restore | v0.1 readiness list (section 19) |
| v0.2.0 | First public | Panel, Fastify and Hono adapters, update with automatic rollback, maintenance mode, full e2e and failure tests, ARM64, built-in VPN | v0.2.0 readiness list (section 19) |
| v0.3 | Extension | Static releases | Static suite tests |
| v0.4 | Extension | TCP/UDP forwarding and TURN, after separate prototypes | UDP and TURN prototype results |
| v1 | Integrations and scale | Wildcard certificates via DNS-01, resource-based load balancing, OIDC, HA | To be determined |

The update in v0.1 is manual: backup, then install the new version. Automatic update rollback arrives in v0.2.0.

## 3. Architecture

Four separate processes under systemd (two more with the built-in VPN), installed by a single installer. The replicas of a service form an frp group on one fixed port, so Caddy has one static upstream per service and entrypointd is not in the data path (variant B).

&#91;embedded content: architecture · variant B, 4 processes under systemd\]

| Process | Origin | Responsible for |
| --- | --- | --- |
| caddy | Official binary, pinned version | TLS and ACME, HTTP/1.1 and HTTP/2, WebSockets, reverse proxy to upstreams and service ports |
| frps | Official binary, pinned version | Tunnel transport; replica groups on fixed service ports, binding to 127.0.0.1 only, vhost disabled |
| entrypointd | Our code, Go | State in SQLite, revisions and applying them, frps authorization plugin, port allocation, revocation journal, certificate consent endpoint, API for the CLI and panel |
| panel | Our code, Node.js (from v0.2.0) | Interface; no state of its own, everything goes through the entrypointd API |
| headscale | Official binary, pinned version (optional, from v0.2.0) | VPN control server and its embedded DERP relay, behind Caddy |
| tailscaled | Official binary, pinned version (optional, from v0.2.0) | The gateway as a VPN node, so Caddy reaches node addresses |

Caddy and frps start from the last fully applied configuration stored on disk. The gateway therefore starts even when entrypointd and the panel are not running.

| When this is down | Public traffic | Existing tunnels | New tunnel logins | Configuration changes |
| --- | --- | --- | --- | --- |
| panel | works | work | work | via the CLI |
| entrypointd (process crash, restart by systemd) | works | work | wait for the daemon restart | impossible |
| entrypointd and its socket stopped | works except tunnels | dropped at the next heartbeat (up to 10 s) | rejected | impossible |
| frps | works except tunnels | dropped | after frps restarts | possible |
| caddy | down | no traffic | work | possible |
| headscale | works; routes to VPN nodes keep working over established node connections | work | work | possible; no new VPN nodes |

After test 3.1 the plugin handles Login, NewProxy and Ping (rejecting Ping drops the sessions of revoked tokens). The plugin socket is held by systemd (`entrypointd.socket`), so when the daemon crashes and restarts, frps requests wait in the queue and sessions continue. Active sessions depend on entrypointd only when it is fully unavailable, including the socket. The entrypointd rows were updated after test 3.1b; measurements are in `docs/results.md`.

**Built-in VPN (from v0.2.0, optional).** `entrypoint install --vpn-name vpn.example.com` adds Headscale and tailscaled, so routes can point at machines without a public IP that run the regular Tailscale client. This complements tunnels: a tunnel publishes one app with replicas and drain, the VPN reaches whole machines by address.

- **Control server:** Headscale listens on 127.0.0.1 (7600, metrics on 7601) and Caddy serves it on the VPN name with an ACME certificate, upgrading `/ts2021` and `/derp`. The name is reserved: no route may use it. It is fixed once set.
- **Relay:** only the embedded DERP server (region 999) with STUN on UDP 3478; no DERP map or log uploads go to Tailscale Inc. The gateway node listens on UDP 41641 for direct connections.
- **The gateway as a node:** tailscaled runs as root with its own TUN device (`ep-vpn0`) and logs in over loopback during install, so it does not wait for the certificate. Its key goes through a file, never the command line.
- **Joining:** `entrypoint vpn key` issues a Headscale pre-auth key (single use and 1 h by default), shown once like a tunnel token. `vpn nodes` and `vpn rm` list and remove machines. All nodes belong to one Headscale user and there is no ACL policy: every node can reach every other one.
- **Backup:** Headscale's database (a consistent copy), its keys and the gateway's node state are part of the backup, so a restored gateway keeps its VPN address and the nodes reconnect without rejoining.

**Comparison variant (SRV).** Dynamic Caddy upstreams from a local DNS server run by entrypointd. Developed only if variant B does not pass experiment 1. It has known drawbacks: new requests depend on the daemon, there are no active Caddy health checks with dynamic upstreams, and there is no confirmation that a replica has withdrawn.

**The fallback plan for frp** is to evaluate another existing tunnel project, not to write our own protocol.

## 4. Configuration state

The configuration is a series of numbered, immutable revisions in SQLite. For each component, entrypointd records the number of the last applied revision and reconciles the desired state with the applied state. Token revocations are outside the revisions (section 6).

**Revisions.** Every change from the CLI or panel creates revision N with an author, a time and a diff against N−1. A change names a base revision (`baseRev`); if a newer one has been created in the meantime, the change is rejected.

**Validation before saving:** conformance to the schema, no route conflicts (section 8), existence of the referenced services, and finally rendering the Caddy configuration and checking it without applying it.

**Applying revision N:**

1. Mark N as `applying`.
2. Render the artifacts: the Caddy configuration (JSON) and the authorization snapshot (services, ports, tokens with scopes).
3. Authorization snapshot: temporary file, fsync, atomic rename, swap in memory. On the swap, tokens from the revocation journal are always subtracted.
4. Caddy: the full configuration through the admin API (`/load`) and the same content written to Caddy's startup file.
5. Mark N as `applied` and record the number per component.

**Partial failure.** When step 4 fails after step 3 succeeded, entrypointd restores the snapshot from the last `applied` revision, still with the revocations subtracted. N gets the status `failed` with the error text and is not retried in a loop. Tunnel sessions opened while the snapshot of N was in force are checked again after the rollback against the restored state, and those that do not match are dropped.

**Restart and a crash in the middle.** After startup, entrypointd re-applies the last `applied` revision to all components (an idempotent operation). A revision interrupted in the `applying` state is retried once and, on error, rolled back as above. The authorization snapshot loaded at startup is always reduced by the revocation journal, so a revocation persisted before the crash is in force from the first request after the restart.

**Drift.** Every 30 s entrypointd compares the hash of the configuration applied in Caddy with the expected one. A mismatch ends with re-applying and a log entry.

**Replica state is not part of a revision.** The source of truth about replicas is frps, which manages group membership itself. entrypointd reads it for status and consistency checks (section 7).

## 5. Permissions and trust model

A token grants only explicitly assigned permissions, and the gateway checks them, never the SDK. Routes point to services, so a service token has no influence on routing.

| Permission | What it allows | Who gets it |
| --- | --- | --- |
| `replica:connect` service X | Connecting a replica to the group of service X | Service token |
| `routes:write` | Creating and changing routes and services | Administrator only (CLI, panel) |
| `tokens:manage` | Issuing and revoking tokens | Administrator only |
| `site:deploy` site Y | Publishing releases of site Y | Separate deploy token (v0.3) |
| `ports:open` | Opening TCP/UDP ports | Administrator only (v0.4) |

Example: a backend token for `example.com/api` only allows connecting replicas of the `api` service. It will not change the `example.com/*` route or publish a frontend.

**Trust model:**

- The holder of a service token is a trusted operator of that service. The frpc health check and the connector drain are trusted-client mechanisms; a modified frpc can disable them, but it then harms only its own service.
- The boundaries between services and token revocation are enforced by the gateway. They also hold against a modified frpc that ignores the connector.
- Local processes on the gateway server are trusted; service ports on 127.0.0.1 are not separately protected from them.

**Enforcement in the frps plugin:**

- **Login:** the token comes from the client metadata. An unknown or revoked token is rejected, and failed attempts are rate-limited per IP address.
- **NewProxy:** only the `tcp` type is allowed. The plugin overwrites the group, group key and remote port with the values of the service from the token. The remaining fields are checked against an allowlist; fields outside it, including customDomains, subdomain, locations and settings of other proxy types, are rejected. The proxy name must point to a service the token has permission for.
- **frps configuration:** HTTP and HTTPS vhost disabled, port binding to 127.0.0.1 only, allowed ports limited to the pool.

**Storage.** A token is 256 random bits, shown only once. Only its hash is stored in the database.

## 6. Tunnels

The tunnel client always verifies the server certificate, authorization works without the panel, and token revocation is permanent and enforced by the gateway.

**Server identity.** By default frpc encrypts the connection but does not verify the frps certificate; verification requires pointing to a trusted CA. The installer creates a private CA (key only on the server, permissions 0600) and a tunnel port certificate (default 55555) for a fixed DNS name of the gateway, e.g. `tunnel.example.com`. The connector always sets CA and server name verification, and refuses to start without a CA. The mode without verification exists only as an explicit developer flag.

**Connection bundle** `ENTRYPOINT_CONNECT` contains the gateway DNS name, the port, the list of trusted CAs and the token. It uses a name rather than an IP address, so moving the gateway to another IP requires only a DNS record change.

**Renewal and rotation.** The tunnel port certificate is valid for 1 year and is renewed by entrypointd 30 days before expiry, with the same CA, without affecting clients. CA rotation takes three steps: new connection bundles with both CAs, switching the server certificate to the new CA after the clients are updated, removing the old CA from the bundles. `entrypoint ca status` shows which clients still log in with the old bundle.

**Authorization without the panel.** The frps plugin is an entrypointd endpoint on 127.0.0.1. It answers from the last applied snapshot in memory, without involving the panel. When entrypointd is down, frps rejects new logins while existing tunnels keep working; systemd restarts the daemon and clients retry the login.

**Service ports.** Each service gets a fixed port from the pool (default 20000–29999) when it is created. The port of a deleted service stays permanently reserved and is never given to another service. Traffic for service A therefore cannot reach service B through a stale entry.

**Reconnect.** frpc retries the connection with exponential backoff. After returning, the replica joins its service's group on the same port.

**Token revocation:**

1. An entry goes into the revocation journal: a separate append-only table, outside the revisions. Neither a revision rollback nor an automatic update rollback reverts it.
2. The authorization snapshot is updated immediately, so new logins and registrations with this token are rejected.
3. Active sessions of the token are dropped by the gateway: the plugin rejects the Ping operation, checking every session at every heartbeat against the current snapshot. A client with a heartbeat (the connector, every 10 s) is disconnected at the next Ping; a client that sends no heartbeats or ignores the rejection is disconnected after the frps `heartbeatTimeout` (40 s) at the latest. `entrypoint token revoke --now` drops the session immediately by restarting frps (a short interruption for the remaining tunnels).

The revocation is persisted before the command returns success. There is no separate dropping step that an entrypointd crash could interrupt: after a restart the snapshot is reduced by the revocation journal, and Ping requests waiting in the systemd socket are handled by the new process. The guarantee is checked by the test `TestRevoke_CrashRightAfterRevoke` (revoke, then `kill -9` of entrypointd right after, a modified client with a long-lived connection).

**Revocations and emergency restore.** A backup taken before a revocation does not know about that revocation. Therefore an emergency restore invalidates all restored tokens before frps and the plugin start accepting logins; the administrator issues them again. An off-machine copy of the revocation journal, which would allow the tokens to be kept, is an option for later.

## 7. Replicas and drain

The replicas of a service form an frp group on the service port. The group distributes TCP connections, not HTTP requests, and all drain guarantees depend on this.

**Consequences of distributing by connection:**

- Subsequent requests on one Caddy keep-alive connection go to the same replica.
- Withdrawing a replica from the group stops new connections, but not new requests on connections that are already open.
- Caddy sees the whole group as one upstream. Its health check does not distinguish replicas, so active Caddy health checks are disabled for tunnel upstreams. Otherwise an error in one replica could disable the whole service.

**Readiness and health.** This is handled by the frpc health check set by the connector: an unhealthy replica does not accept new connections from the group. It is a trusted-client mechanism (section 5). entrypointd reads the proxy state from frps for `entrypoint status` and warns about a service without any healthy replica.

**Drain** proceeds in three separate steps:

1. **Withdraw from the group:** the connector closes the proxy, so frps does not route new connections to the replica.
2. **Draining connections:** existing connections still serve requests. The framework adapter then responds with the `Connection: close` header, stops accepting new connections and closes WebSockets with code 1001. The base contract does not do this: it only waits for the connections to expire.
3. **Termination:** after `drainTimeout` (default 30 s) the connector terminates frpc, which drops the remaining connections.

| Guarantee | Base contract | Framework adapter |
| --- | --- | --- |
| No new connections after withdrawal | Yes | Yes |
| No new requests on existing connections | Only after the connection closes or `drainTimeout` | After the current request on that connection finishes |
| A request shorter than `drainTimeout` completes correctly | Yes, if the application is still running | Yes |
| WebSockets and SSE closed in a controlled way | Dropped after `drainTimeout` | Closed with code 1001 |

The base contract does not control the application process. If the application exits before `drainTimeout`, active requests end with an error.

| Timeout | Default value | Where |
| --- | --- | --- |
| Establishing a connection to the service port | 5 s | Caddy |
| Waiting for response headers | 60 s, configurable per route | Caddy |
| Idle keep-alive connection to the upstream | 4 s | Caddy |
| Replica drain | 30 s, configurable in the SDK | Connector |
| WebSocket, SSE, gRPC stream | No limit, except for drain | Caddy |

The short keep-alive idle time (4 s) limits how long a withdrawn replica can keep receiving requests on idle connections.

**WebSockets and configuration changes.** A change of the replica list does not touch the Caddy configuration, so it does not interrupt streams. A revision change reloads Caddy, and Caddy by default closes WebSockets then; we set `stream_close_delay` to 1 h. A Caddy restart or update drops all connections. WebSocket and SSE clients must be able to reconnect.

## 8. HTTP routing

A route is a pair of host and path prefix, pointing to a tunnel service or a fixed `IP:port` upstream. The pair is unique, and conflicts are rejected when a revision is validated. Only the administrator changes routes.

- **Matching:** an exact host wins over a wildcard (`*.example.com`). Within a host, the longest prefix wins. A prefix matches on a segment boundary: `/api` covers `/api` and `/api/x`, but not `/apix`.
- **The prefix** is passed to the upstream unchanged by default; the `stripPrefix` option removes it.
- **Path normalization:** the behavior for `..`, double slashes and encoded `%2F` is determined by the routing tests. We assume nothing beyond what they show.
- **Headers:** Host is passed unchanged. Caddy sets X-Forwarded-For, X-Forwarded-Proto and X-Forwarded-Host, and values sent by the client are ignored, because there is no trusted proxy in front of Caddy. The tests confirm this.
- **Per-route limits:** maximum request body size (default 50 MB) and the response header timeout.
- **Protocols:** HTTP/1.1, HTTP/2 and WebSockets. HTTP/3 is not in the scope of v0.1.

## 9. TLS and ACME

Caddy issues certificates per host through ACME (HTTP-01 and TLS-ALPN challenges). Entrypoint does not configure DNS: `entrypoint dns check` shows the required records and their current state.

**Let's Encrypt limits:** up to 50 new certificates per registrable domain within 7 days, with the pool refilling by 1 certificate every 202 minutes. All subdomains of one domain share this limit. Renewals through ACME Renewal Info are exempt from the limits.

- **The DNS check before ordering** is a preliminary test, not protection against the limit.
- **The issued-certificate counter** in `entrypoint status` counts only the certificates of this installation and serves only for warnings. It does not see certificates issued outside Entrypoint, so it is not a control.
- **Tests and CI** use the Pebble test authority, and trials on a real server use the Let's Encrypt staging environment. Every host can be switched to test mode.

**Wildcard routes and certificates** with an `A *.example.com` record: a wildcard route by itself is not consent to issue a certificate. Certificates are given only to explicitly named names (`route add *.example.com ... --tls-name a.example.com`, ordered immediately). On-the-fly certificates for any subdomain require explicit consent on the route (`--tls-on-demand`): Caddy then asks the consent endpoint in entrypointd, which allows at most 10 new subdomains per hour. Every new name consumes a slot in the weekly limit.

**Wildcard certificates (v1)** require a DNS-01 challenge: through the DNS provider's API or by delegating `_acme-challenge` with a CNAME record to a zone we control.

The tunnel port uses the private CA (section 6), not ACME.

## 10. SDK and connector

The base contract is a tunnel to an existing local address. The application listens as usual, and the SDK exposes that address through the tunnel. Framework adapters give stronger drain guarantees (section 7).

```ts
import { tunnel } from "@k18k/entrypoint-sdk"

const t = await tunnel({
  connect: process.env.ENTRYPOINT_CONNECT, // gateway name, port, CA, token
  service: "api",
  upstream: "http://127.0.0.1:3000",
  healthCheck: "/health",
})

process.on("SIGTERM", () => t.drain({ timeoutMs: 30_000 }))
```

The same from the command line, for an application in any language:

```bash
npx @k18k/entrypoint-sdk tunnel --service api --upstream http://127.0.0.1:3000
```

| Adapter | Version | What it does |
| --- | --- | --- |
| `@k18k/entrypoint-sdk/http` | v0.1 | `expose(handler)` (any `(req, res)` handler, including an Express app) starts a server on `127.0.0.1:0` and the tunnel; on drain it stops accepting connections, sends `Connection: close` and closes WebSockets with code 1001 |
| `@k18k/entrypoint-sdk/fastify` | v0.2.0 | Waits for `ready()` and uses the Fastify server, with the same drain |
| `@k18k/entrypoint-sdk/hono` | v0.2.0 | Runs the handler through the Node adapter for Hono, with the same drain |

Each adapter has CI tests for the supported ranges of framework and Node.js versions. Bun and Deno work through the base contract until tests are added.

**The connector** `entrypoint-connector` (Go) is a small intermediary process on top of the official frpc binary:

- builds the frpc configuration from the connection bundle, always with CA and server name verification and with a health check,
- watches its standard input; when the parent process dies, the pipe closes and the connector terminates frpc and itself,
- handles reconnect and drain and sends JSON events (`connected`, `healthy`, `draining`, `disconnected`) that the SDK only observes,
- never prints the token or the connection bundle.

The connector talks only to frps; there is no separate control channel to entrypointd. Supported platforms: Linux x64 and arm64, macOS x64 and arm64, Windows x64, through per-platform `optionalDependencies` (`@k18k/entrypoint-sdk-<os>-<cpu>` with the connector and frpc); the SDK never downloads binaries.

**VPN mode (from v0.2.0).** `entrypoint-connector vpn` joins the built-in VPN as a userspace node (tsnet, no root, no TUN device and no Tailscale installation) with an `ENTRYPOINT_VPN` bundle (`epv1.`: control server URL and a pre-auth key), and keeps the node identity in a state directory so restarts need no new key.

- `--name vatro-postgres --expose 5432` publishes `127.0.0.1:5432` of that machine in the VPN as `vatro-postgres:5432`.
- `--forward vatro-postgres:5432` listens on `127.0.0.1:5432` and connects through the VPN; the node name resolves inside the connector, so the machine needs no MagicDNS.
- `scripts/install-connector.sh` installs the connector and frpc on a Linux machine (SHA256-checked) and runs a mode as a systemd service; it reads the bundle from the terminal into a 0600 environment file and, for `--forward`, adds an `/etc/hosts` entry so the application connects to `vatro-postgres:5432` unchanged. In Docker the forwarding container takes the service name instead.

## 11. CLI

The `entrypoint` CLI is the full administrative interface in v0.1 and an equivalent alternative to the panel from v0.2.0. It talks to entrypointd through a local socket, so it works without the panel.

| Command | What it does |
| --- | --- |
| `entrypoint install` | Installs the services, creates the private CA, the tunnel port certificate and the database |
| `entrypoint status` | State of the services, revisions, services, replicas and the certificate counter |
| `entrypoint service add <name> [--health <path>]` | Creates a service and assigns it a fixed port |
| `entrypoint service rm <name>` | Removes a service; its port stays permanently reserved |
| `entrypoint route add <host>[/prefix] --to <ip:port>` | A route to an upstream |
| `entrypoint route add <host>[/prefix] --service <name>` | A route to a tunnel service |
| `entrypoint route list`, `entrypoint route rm <id>` | List and remove routes |
| `entrypoint token create --service <name>` | Issues a token and prints `ENTRYPOINT_CONNECT` once |
| `entrypoint token list`, `entrypoint token revoke <id>` | List and revoke tokens |
| `entrypoint dns check` | Required DNS records and their current state |
| `entrypoint ca status`, `entrypoint ca rotate` | State and rotation of the private CA |
| `entrypoint revision list`, `entrypoint revision show <n>` | Revision history and status |
| `entrypoint backup`, `entrypoint restore <file>` | Backup and restore |
| `entrypoint logs <service>` | Logs of the selected service |
| `entrypoint update` | Update (automatic rollback from v0.2.0) |
| `entrypoint uninstall [--purge]` | Uninstall, with or without data |

Every command that changes the configuration creates a revision and prints its number and the result of applying it.

## 12. Panel (from v0.2.0)

The panel is minimal and has no state of its own. Every operation goes through the entrypointd API, so a panel failure removes only the interface.

- **Scope:** routes and services, tokens (issue, list, revoke), replica and revision status with apply errors, required DNS records and certificate state.
- **Login:** passkeys (WebAuthn) only, for a single administrator: discoverable credentials with user verification required, no passwords. `entrypoint panel register` (root) prints a one-time link valid for 60 seconds whose token is in the URL fragment, so it never reaches the server's logs or a Referer; the server stores only its hash. `entrypoint panel keys` lists the passkeys and `panel keys rm <id>` revokes one; adding or removing a key ends every session. Failed sign-ins and registrations are limited per IP address.
- **Session:** a cookie with the HttpOnly, Secure and SameSite=Strict attributes.
- **CSRF:** every state-changing operation requires a CSRF token and a matching Origin header.
- **Exposure:** the panel is publicly reachable through Caddy on its own domain; only its backend port is private. Available restrictions: an allowlist of IP addresses for the panel domain, or a local mode without a public route, accessed through an SSH tunnel. The installer recommends one of them.
- **Stack:** a Go backend (`entrypoint-panel`, its own process and system user, on 127.0.0.1:7700, talking to entrypointd through the API socket, so the gateway needs no Node.js runtime) with a React + Vite frontend embedded in the binary. Caddy serves it on `--panel-name` with an ACME certificate; the name is reserved like the VPN's. Revisions made from the panel have the author `panel`.
- **Apps:** creating an app makes the service, its routes and a token in one step and shows `ENTRYPOINT_CONNECT` once with commands for a Linux machine, the Node.js SDK and Docker Compose. Hosts are bound here, by the administrator, never claimed by a connector, so a leaked token cannot publish another domain.
- **VPN:** machines with their addresses, removal, and keys for publishing a service (`--expose`), using one from an app (`--forward`) or joining with the Tailscale client.

## 13. Installation and operations

A single installer creates separate systemd services tied together in `entrypoint.target`. Programs, data and configuration live in separate directories, so an update and an uninstall do not touch the data.

```bash
curl -fsSL <script-address>/install.sh | sudo sh
sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com
```

| Path | Contents |
| --- | --- |
| `/opt/entrypoint/releases/<version>` | The programs of a given version, including the pinned Caddy and frps binaries |
| `/opt/entrypoint/current` | Symlink to the active version |
| `/etc/entrypoint` | Startup configuration |
| `/var/lib/entrypoint` | SQLite with the revisions and the revocation journal, the private CA, the Caddy storage (ACME account, certificates), the Caddy and frps startup files |

Each process runs as a separate system user. Caddy gets only the permission for ports below 1024. Logs go to journald. Only systemd is supported.

**Update (automatic from v0.2.0):**

1. Enable maintenance mode: administrative changes are blocked except token revocations, until the end of step 8.
2. Download the new version into `releases/<version>` and verify the release signature.
3. Stop the old entrypointd, then back up.
4. Database migrations: forward only, in a transaction.
5. Switch the `current` symlink.
6. Restart in order: entrypointd, frps, caddy, panel.
7. Health check of all services for 60 s.
8. On error: restore the symlink and the database from step 3, re-apply the revocations recorded in maintenance mode, then disable maintenance mode.

A frps restart drops the tunnels and a Caddy restart drops all connections, so an update is a short maintenance window. In v0.1 the update is manual: backup, then install the new version.

**Backup.** `entrypoint backup` creates a password-encrypted archive: a copy of SQLite made without stopping the services, the private CA with its key, the Caddy storage and a version manifest; from v0.3 also the site releases. Logs are not part of the backup.

**Restore:**

1. Install the same version on a clean machine.
2. `entrypoint restore <file>`. On an emergency restore, all restored tokens are invalidated before frps starts accepting logins (section 6).
3. Reconciliation and starting the services.
4. With a new IP address: update the DNS records. Connection bundles use the gateway name, so clients need no changes other than a new token.

**Uninstall.** `entrypoint uninstall` removes the services and programs but leaves `/var/lib/entrypoint` and `/etc/entrypoint`. The `--purge` flag also removes the data, only after confirmation and with an offer to back up.

## 14. Secret protection and limits

Secrets never reach the logs, and basic limits apply from v0.1.

**Secrets:**

- Tokens, connection bundles, the CA key and passwords do not appear in logs, error messages or diagnostic events of any component. entrypointd masks the metadata from plugin requests, and the frps log level must not print client metadata (checked by e2e suite 12).
- Files with secrets have permissions 0600 and belong to the user of the service that needs them.
- The backup is encrypted with a password supplied at creation.

**Limits from v0.1:**

| Limit | Default value |
| --- | --- |
| Replicas per token | 10 |
| Service port pool | 20000–29999 |
| Failed tunnel logins per IP address | 10 per minute |
| Request body size | 50 MB per route |
| Request header size | Caddy default limit |

Per-IP connection limits on the public side need a decision before v0.2.0: a Caddy plugin or operating system firewall rules. Local limits do not protect against link saturation.

## 15. Later stages

**Static releases (v0.3).** Publishing a new release does not break browsers running the previous version, but only for assets with a content hash in the file name. Paths without a hash, e.g. `/config.json`, always return the current version; the server does not decide which version an old browser tab wants.

- **Retention:** hashed files from previous releases stay available for 7 days or for the last 5 releases, whichever is longer. `index.html` always comes from the current release.
- **Missing files:** fallback to `index.html` only for navigation (GET, Accept with `text/html`, path without an extension). A missing `/assets/app.js` gets 404, never HTML.
- **Upload:** streamed tar.gz, with limits on total size, file size and file count. Absolute paths, `..` segments, symbolic and hard links and special files are rejected. Unpacked into a temporary directory, published only after verification.
- **Parallel deploys:** a queue per site, releases numbered in ascending order, published in the order accepted. The `--expect-current <id>` option rejects a deploy if the current release has changed since the build started.
- **The `site:deploy` permission** per site, independent of tunnel tokens.

**TCP/UDP forwarding and TURN (v0.4)** come only after separate prototypes. To be checked: UDP through frp with QUIC transport under loss, jitter and congestion, and TURN/TLS sharing port 443 with HTTPS through SNI. TURN needs its own authentication model and limits so that the gateway does not become an open traffic relay.

**v1:** wildcard certificates through DNS-01, load balancing by resources reported by replicas, OIDC in front of applications and several gateways (HA).

## 16. Claim status

The claims of this specification fall into those confirmed by documentation (8), our own design decisions (9) and assumptions that require an experiment (11). An experiment can change a decision; the change then goes into this table together with the result.

| Claim | Status | Source or test |
| --- | --- | --- |
| frps calls an external HTTP plugin before the Login, NewProxy, CloseProxy, Ping, NewWorkConn and NewUserConn operations; the plugin can reject the operation or change its content | Documentation | [gofrp: Server Plugin](https://gofrp.org/en/docs/features/common/server-plugin/) |
| The global metadata of the frpc client reaches the plugin in the Login operation | Documentation | [gofrp: Server Plugin](https://gofrp.org/en/docs/features/common/server-plugin/) |
| frpc does not verify the frps certificate by default; pointing to a trusted CA enables verification | Documentation | [gofrp: TLS](https://gofrp.org/en/docs/features/common/network/network-tls/) |
| tcp proxies with the same group, group key and remote port form a group among which frps distributes connections | Documentation | [gofrp: Load Balancing](https://gofrp.org/en/docs/features/common/load-balancer/) |
| The frpc health check (tcp or http) removes the proxy from frps after consecutive failed checks | Documentation | [gofrp: Load Balancing](https://gofrp.org/en/docs/features/common/load-balancer/) |
| Caddy by default closes WebSockets on a configuration reload; `stream_close_delay` keeps them for the given time | Documentation | [Caddy forum](https://caddy.community/t/way-to-achieve-zero-downtime-for-existing-connections-when-dynamic-patch-maps/33753) |
| Let's Encrypt: 50 new certificates per registrable domain per 7 days; renewals through ARI exempt | Documentation | [Let's Encrypt: rate limits](https://letsencrypt.org/docs/rate-limits/) |
| Active Caddy health checks do not work with dynamic upstreams (applies to the SRV variant) | Documentation | [Caddy: reverse\_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy) |
| Variant B: replicas as frp groups on fixed service ports | Design decision | Section 3 |
| The port of a deleted service permanently reserved | Design decision | Section 6 |
| The plugin handles only Login and NewProxy, unless test 3.1 forces Ping | Design decision | Sections 3 and 6 |
| Separate processes under systemd, one installer | Design decision | Section 3 |
| Private CA and a connection bundle with the gateway DNS name | Design decision | Section 6 |
| Revocation journal outside the revisions; emergency restore invalidates the restored tokens | Design decision | Section 6 |
| Active Caddy health checks disabled for tunnel upstreams | Design decision | Section 7 |
| The frpc health check and the connector drain are trusted-client mechanisms | Design decision | Section 5 |
| The SDK contract on a local address; adapters with separate guarantees | Design decision | Sections 7 and 10 |
| Withdrawing a replica from the group, draining keep-alive connections and terminating frpc work as three separate steps | Experiment | Tests 1.1–1.4 |
| A proxy with a health check gets no connections before the first successful check | Experiment | Test 1.10 |
| The gateway can drop the active session of a revoked token without the client's cooperation | Experiment | Tests 3.1 and 3.1b |
| An unavailable plugin causes logins to be rejected, with no effect on existing tunnels | Experiment | Test 1.6 |
| Configuration through `/load` is applied atomically, and on error Caddy stays with the previous one | Experiment | e2e suite 6 |
| Path normalization and ignoring client X-Forwarded-\* headers | Experiment | e2e suite 3 |
| frps does not print client metadata to the logs at the chosen log level | Experiment | e2e suite 12 |
| The frpc trusted CA file can contain several certificates, which enables CA rotation | Experiment | Test 2.5 |
| entrypointd can reliably read the proxy state from frps for status and consistency checks | Experiment | Test 2.4 |
| UDP through frp with QUIC, and TURN/TLS on port 443 | Experiment | v0.4 prototypes |
| The performance target on the described reference machine, with and without a tunnel | Experiment | Benchmark before v0.2.0 |

## 17. Prototype plan

The prototype checks three architecture risks: drain in frp groups, replica identity and lifecycle, and permanent token revocation. Variant B is tested first; if it passes experiment 1, the SRV variant is not developed.

Rules:

- The Caddy and frp versions are pinned in `versions.json` and recorded in the results.
- The limits in the criterion column are preliminary. An exploratory measurement may precede them, but a change of a limit is recorded explicitly before the acceptance test. A measurement by itself is not a pass.
- The result of each test goes into `docs/results.md` in four fields: expected, observed, limitations, decision.

**Experiment 1: frp groups and drain**

| # | Test | Pass criterion |
| --- | --- | --- |
| 1.1 | A new request on an existing Caddy keep-alive after the replica is withdrawn from the group | With the adapter: no new requests to the withdrawn replica after 5 s. Base contract: none after `drainTimeout` |
| 1.2a | Graceful drain during an upload that fits within `drainTimeout` | 2xx response and complete data in the application |
| 1.2b | Immediate token revocation during an upload | Upload interrupted, the client gets an error, the application does not treat partial data as success |
| 1.3 | SSE and WebSocket through a replica being withdrawn | With the adapter: closed with code 1001 before `drainTimeout`. Base contract: dropped after `drainTimeout` |
| 1.4 | Withdraw from the group, draining connections, end of frpc | Each step observable and triggered separately |
| 1.5 | One replica returns errors, the other is healthy | The service still responds through the healthy replica; the unhealthy one is removed from the group within interval × number of attempts + 5 s |
| 1.6 | entrypointd failure under traffic | New requests served; new logins rejected; after the restart, logins return within 30 s |
| 1.7 | Cold start of Caddy while entrypointd is not running | Caddy starts from the file and serves traffic |
| 1.8 | Removal of the last replica | Response 502 or 503 within 1 s; no connection to the removed replica lasts longer than 5 s |
| 1.9 | frps restart | Replicas return to their groups on the same ports within 30 s |
| 1.10 | A new replica with a health check | No connection reaches it before the first successful check |

**Experiment 2: replica identity and lifecycle**

| # | Test | Pass criterion |
| --- | --- | --- |
| 2.1 | A modified frpc presents someone else's group, port or service | Rejected by the plugin and logged, with no effect on other services |
| 2.2 | NewProxy fields outside the allowlist | Rejected or overwritten by the plugin |
| 2.3 | Removing a service and creating a new one | The port of the removed service never goes to the new one |
| 2.4 | Restarts of entrypointd and frps in various orders | Status in entrypointd consistent with the frps state, no divergence |
| 2.5 | CA rotation | A client with a two-CA bundle connects before and after the switch; a client with only the old CA is rejected after the switch |

**Experiment 3: permanent token revocation**

| # | Test | Pass criterion |
| --- | --- | --- |
| 3.1 | Clients A (modified frpc ignoring the connector) and B on different tokens; revoking the token of A | A disconnected and does not reconnect; B uninterrupted. With the fallback frps restart, B returns within 10 s |
| 3.1b | Only if rejecting Ping was chosen: repeat test 1.6 | The effect of an entrypointd failure on active sessions measured and accepted; update the failure table in section 3 |
| 3.2 | Revocation during a partial application of a revision, then a rollback | The revocation holds; sessions opened in the meantime checked again |
| 3.3 | Backup at 10:00, revocation at 11:00, machine lost, restore from 10:00 | All restored tokens invalidated before the first login |
| 3.4 | Revocation in maintenance mode during an update ending in a rollback (v0.2.0) | Revocation preserved |
| 3.5 | A successful migration after which the new version fails the health check (v0.2.0) | Return to the previous version without losing revocations |

## 18. E2E tests

The e2e suite runs with a single command (`make e2e`) in Docker containers. The gateway is installed with the real installer on a clean container. The tests show that the described scenarios work, but they do not prove the absence of errors outside them; the suite grows with every bug found.

| Container | Role | Tool |
| --- | --- | --- |
| gateway | A clean system with systemd, on which the installer and the gateway run | Debian image with systemd |
| acme | A test certificate authority instead of Let's Encrypt | Pebble, configured for repeatable tests |
| dns | Resolves the test domains to the gateway address | CoreDNS |
| client-a, client-b | Applications with the connector on different tokens | Node.js with the SDK, frpc |
| attacker | A modified frpc trying to bypass permissions | frpc with its own configuration |
| upstream | A service reachable through IP:port | HTTP, SSE and WebSocket server |
| chaos | Delays, packet loss and network cuts for the clients | tc netem or Toxiproxy |
| runner | Runs the scenarios and collects artifacts | Go tests, Playwright for the panel |

After each test, artifacts remain: the logs of all containers, the applied Caddy and frps configurations, the database state and the result. `make e2e SUITE=<name>` runs one suite. Systemd in a container requires extra privileges; if this distorts the results, suite 1 moves to a virtual machine.

| # | Suite | Cases | From version |
| --- | --- | --- | --- |
| 1 | Installer and uninstall | Installation on a clean machine; reinstallation without damage; a port scan confirms the Caddy admin API, the frps dashboard and the panel backend are unreachable; `uninstall` leaves the data, `--purge` removes it | v0.1 |
| 2 | TLS and ACME | Cert from Pebble after the DNS record appears; no cert for a host without a route; renewal; the connector rejects a server with a foreign CA | v0.1 |
| 3 | HTTP routing | Host and prefix, longest prefix, segment boundary, `stripPrefix`, conflicts, path normalization, client X-Forwarded-\*, size limits, IP:port upstream, WebSocket, SSE, HTTP/2 | v0.1 |
| 4 | Tunnels and permissions | Tests 2.1–2.5; a token without scope; disallowed NewProxy types and fields | v0.1 |
| 5 | Replicas and drain | Tests 1.1–1.10 | v0.1 |
| 6 | Configuration state | `baseRev` conflict; Caddy error after the authorization step; killing entrypointd during apply; drift | v0.1 |
| 7 | Token revocation | Tests 3.1–3.3 in v0.1, 3.4–3.5 in v0.2.0 | v0.1 |
| 8 | Operations | Backup and restore on a fresh container; restore under a new IP with a fixed gateway name; update with a faulty migration and with a failed health check; maintenance mode | v0.1 (backup, restore), v0.2.0 (update) |
| 9 | SDK and adapters | Base contract and adapters in a Node.js version matrix; `kill -9` of the parent ends the connector and frpc | v0.1 (node:http), v0.2.0 (the others) |
| 10 | Panel | Login and attempt limit; HttpOnly, Secure, SameSite cookie; CSRF; routes, services, tokens; revocation from the panel | v0.2.0 |
| 11 | Resilience | Failure of entrypointd, frps and a stubbed API client; Caddy restart; client network cut for 5 s and 60 s; packet loss | v0.1 (basic), v0.2.0 (full) |
| 12 | Secret protection | Searching logs, errors and events for tokens and keys; permissions of files with secrets | v0.1 |
| 13 | Built-in VPN | A node joins with a CLI key and serves through a route to its VPN address; the VPN name is reserved; the key is not in logs; removing a node cuts its route; backup and restore keep the nodes and the gateway's VPN address | v0.2.0 |

**What containers will not show:** the real Let's Encrypt and its limits, the VPS provider's firewall, public DNS and IPv6, link saturation, ARM64 (needs a separate runner or emulation) or reliable performance. That is why every release also goes through a smoke test on a real server.

## 19. Release readiness

**v0.1, for personal use:**

- [ ] Results of experiments 1–3 (within the v0.1 scope) recorded in `docs/results.md`, and variant B accepted or rejected
- [ ] e2e suites 1–7, 12 and the v0.1 parts of suites 8, 9 and 11 green in Docker
- [ ] Installation on a real VPS with a real certificate
- [ ] A host to an upstream over VPN works, including a WebSocket
- [ ] A host to a tunnel with two replicas (node:http adapter) works; stopping one does not interrupt the service
- [ ] A revoked token does not reconnect, and its active session is dropped
- [ ] Backup and restore on a fresh container
- [ ] README with installation and CLI usage

**v0.2.0, first public release:**

- [ ] e2e suites 1–13 green in CI on x86\_64
- [ ] Suites 1, 4 and 5 green on ARM64
- [ ] Panel with session protection and CSRF
- [ ] Update with automatic rollback and maintenance mode
- [ ] Decision on per-IP connection limits
- [ ] Smoke test on a real VPS with Let's Encrypt staging
- [ ] Restore on a fresh machine outside the test environment
- [ ] Benchmark on the reference machine with the result recorded
- [ ] Built-in VPN: a machine behind NAT reachable through a route on a real VPS

## 20. Starting in Claude Code

**Preparation:**

- [ ] Claude Code on a computer with Docker, Go and Node.js
- [ ] A Linux VPS with SSH access
- [ ] A test domain with the ability to set records
- [ ] This specification exported to Markdown as `docs/specification.md` in a new repo

**Starting prompt** to paste as the first message, with Claude Code started in the repo directory:

```markdown
We are building entrypoint: a self-hosted gateway for publishing services.
The full specification is in docs/specification.md. Read all of it before writing code.

Session goal: version v0.1 (section 2 and the v0.1 readiness list in section 19),
installed on my VPS and controlled through the CLI.

Rules:
- Scope is v0.1 only. No panel and no features from later versions.
- Monorepo: gateway/ (Go: entrypointd, CLI, connector), sdk/node (TypeScript),
  e2e/ (Docker Compose), scripts/install.sh.
- Caddy and frp as official binaries at pinned versions, recorded in versions.json.
- Variant B (frp groups). If the experiment from section 17 shows that B does not meet
  the criteria, stop and describe the problem before changing the architecture.
- First the e2e environment from section 18, then features. Finish each with a green test.
- Order: skeleton and e2e, installer, entrypointd and Caddy routing, tunnels and plugin,
  connector and SDK, token revocation, backup and restore, installation on the VPS.
- Secrets never in logs. The tunnel client always verifies the CA.
- Record the test results from section 17 in docs/results.md:
  expected, observed, limitations, decision.
- Before installing on the VPS, ask me for access and the domain.

At the end: a README with installation and CLI usage, and a list of what did not make it in.
```

## Sources

- [gofrp: Server Plugin](https://gofrp.org/en/docs/features/common/server-plugin/)
- [gofrp: TLS](https://gofrp.org/en/docs/features/common/network/network-tls/)
- [gofrp: Load Balancing and Health Check](https://gofrp.org/en/docs/features/common/load-balancer/)
- [Caddy: reverse\_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)
- [Caddy forum: WebSockets on configuration reload](https://caddy.community/t/way-to-achieve-zero-downtime-for-existing-connections-when-dynamic-patch-maps/33753)
- [Let's Encrypt: rate limits](https://letsencrypt.org/docs/rate-limits/)
