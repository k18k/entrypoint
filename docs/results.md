# Prototype experiment results (section 17)

Date: 2026-10-18. Scope: experiments 1–3 in the v0.1 part. Tests 3.4 and 3.5 belong to v0.2.0.

## Environment and versions

| Component | Version |
| --- | --- |
| Caddy | 2.11.7 (official binary, `versions.json`) |
| frp (frps, frpc) | 0.71.1 (official binary, `versions.json`) |
| Pebble | 2.10.1 |
| CoreDNS | 1.14.7 |
| Go / Node.js | 1.27.2 / 24.22.0 |
| Gateway system | Debian trixie with systemd in a container (`--privileged`, private cgroup) |

Note: frp 0.71.1 was released 4 days before the tests. Before v0.2.0 it is worth repeating the tests on a newer patch release.

The tests are run with `make e2e SUITE=Replicas|Tunnels|Revoke` (files `e2e/tests/replicas_test.go`, `tunnels_test.go`, `revoke_test.go`). The measurements come from the `RESULT:` lines in the test log. Artifacts of each test are saved in `e2e/artifacts/<test>/`.

Parameters relevant to the results:

- Caddy: keep-alive to the upstream 4 s, connection 5 s, `stream_close_delay` 1 h;
- frps: `heartbeatTimeout` 40 s;
- connector: heartbeat every 10 s, health check every 1 s, 2 attempts (in the tests).

The limits in the criteria were not changed.

## Overall decision

**Variant B (frp groups on fixed service ports) is adopted.** All tests of experiments 1–3 within the v0.1 scope meet the criteria. The SRV variant is not developed further.

Three additions were made to the architecture, within the same four processes:

1. **Session revocation by rejecting Ping** (test 3.1). The plugin handles the Login, NewProxy and Ping operations. Restarting frps remains as the `entrypoint token revoke --now` option.
2. **Plugin socket held by systemd** (`entrypointd.socket`, tests 1.6 and 3.1b). A crash or restart of entrypointd does not break sessions, because frps requests wait in the socket queue. Sessions depend on entrypointd only when it is fully unavailable. The failure table in section 3 of the specification was updated.
3. **frpc restart after a failed health check** (test 1.5). It breaks keep-alive connections pinned to the sick replica.

## Experiment 1: frp groups and drain

### 1.1 New request on an existing keep-alive after a replica is withdrawn

- **Expected:**
  - with the adapter, no new requests to the withdrawn replica after 5 s;
  - with the base contract, no new requests after `drainTimeout`.
- **Observed:** two replicas, 4 keep-alive clients, traffic every 20 ms.
  - Adapter: in the window 5–15 s after withdrawal the replica received 0 requests. Clients got no errors (0 of 3276).
  - Base contract (`drainTimeout` 10 s): last request to the withdrawn replica 9.7 s after withdrawal, frpc exited after 9.8 s, then 0 requests and 0 errors.
- **Limitations:** with the base contract, the withdrawn replica serves requests on pinned connections until the end of `drainTimeout`, if traffic on those connections does not stop.
- **Decision:** passed. The description of the guarantees in section 7 matches the measurement.

### 1.2a Graceful drain during an upload

- **Expected:** a 2xx response and complete data in the application.
- **Observed:** 8 MB upload at 1 MB/s; drain started after 2 s. Result: code 200 after 8.0 s, matching SHA-256, `uploadsOK=1`.
- **Limitations:** none.
- **Decision:** passed.

### 1.2b Immediate token revocation during an upload

- **Expected:** upload interrupted, the client gets an error, the application does not treat partial data as success.
- **Observed:**
  - upload interrupted 6.2 s after revocation, at the nearest connector heartbeat;
  - the client got 502;
  - the application recorded the interrupted upload (`uploadsAborted=1`, `uploadsOK=0`);
  - the connector exited with a `revoked` event and does not reconnect.
- **Limitations:** revocation through Ping takes effect with a delay of up to one heartbeat interval (10 s). Immediate interruption is provided by `--now` (test 3.1).
- **Decision:** passed.

### 1.3 SSE and WebSocket through a replica being withdrawn

- **Expected:**
  - the adapter closes streams with code 1001 before `drainTimeout`;
  - with the base contract, streams are cut after `drainTimeout`.
- **Observed:**
  - Adapter: WebSocket closed 32 ms after drain started with code 1001 ("server draining"). SSE ended cleanly (EOF) after 31 ms.
  - Base contract (10 s): both streams cut after 10.0 s, the WebSocket without a close frame.
- **Limitations:** the adapter recognizes SSE by `Content-Type: text/event-stream`. WebSockets handled by the `ws` library are closed through the servers passed in. Other upgrades get a raw 1001 frame.
- **Decision:** passed.

### 1.4 Withdrawal, draining and frpc exit as separate steps

- **Expected:** each step observable and triggered separately.
- **Observed:** `drainTimeout` 8 s.
  - Step 1: the replica disappears from the status after 0.1 s (`draining` and `withdrawn` events), and frpc keeps running.
  - Step 2: a request lasting 4 s, started before the drain, finishes with code 200 after 3.5 s.
  - Step 3: frpc exits after 8.1 s.
- **Limitations:** the connector withdraws the proxy by reloading the frpc configuration (`/api/reload` without the proxy). frps then closes only the listener and leaves existing connections, which the frps code confirms (`BaseProxy.Close`).
- **Decision:** passed.

### 1.5 One replica returns errors, the other is healthy

- **Expected:** the service responds through the healthy replica. The sick one disappears from the group within interval × number of attempts + 5 s (here 7 s).
- **Observed:**
  - First version: the sick replica disappeared from the group after 1.4 s, but **errors continued**: 394 of 796 requests after the limit elapsed. Caddy keep-alive connections pinned to the sick replica never become idle under constant traffic, and frps closes only the listener.
  - After changing the connector (frpc restart after `check failed`): replica removed after 1.5 s, last error 1.2 s after the failure, after the limit 0 errors out of 796 requests.
- **Limitations:** the frpc restart also interrupts requests in progress on the sick replica. The replica returns to the group only after a successful health check. The frpc health check is a trusted-client mechanism (section 5).
- **Decision:** passed after the change. The frpc restart after a failed health check is part of the connector.

### 1.6 entrypointd crash under traffic

- **Expected:** new requests are served; new logins rejected; after a restart, logins return within 30 s.
- **Observed:**
  - First version (Ping plugin, socket opened by the daemon): with entrypointd stopped, 428 of 774 requests ended with an error. frpc closes the session after a Pong response with an error, and frps turns every plugin error into such a Pong.
  - After the change (plugin socket in `entrypointd.socket`, activation by systemd): 3× `kill -9 entrypointd` at 3 s intervals under traffic, 0 errors out of 816 requests. A new replica started during the crash logged in 9.5 s after the first crash. systemd restarts the daemon (different PID).
- **Limitations:**
  - During the crash, new logins **wait** for the daemon to restart, instead of being rejected. frps calls the plugin without a timeout, and the connection waits in the socket queue.
  - Full unavailability (the socket stopped too) is described by test 3.1b.
- **Decision:** passed after the change. The behavior during a crash is better than assumed; the table in section 3 was updated.

### 1.7 Caddy cold start with entrypointd down

- **Expected:** Caddy starts from the file and serves traffic.
- **Observed:**
  - The test found a bug: the startup file written by entrypointd had group `entrypoint` instead of `entrypoint-caddy`, so Caddy could not read it. Cause: `os.Chmod(0o2750)` in Go does not set the setgid bit; `os.ModeSetgid` is needed.
  - After the fix, Caddy serves traffic 1.0 s after the restart.
- **Limitations:** none.
- **Decision:** passed after the fix.

### 1.8 Removing the last replica

- **Expected:** a 502 or 503 response within 1 s; no connection to the removed replica lasts longer than 5 s.
- **Observed:**
  - `kill -9` of the Node process of the last replica;
  - first 502 response after 0.11 s, longest gap between responses 53 ms;
  - connector (end of the stdin pipe) and frpc exited.
- **Limitations:** none.
- **Decision:** passed.

### 1.9 frps restart

- **Expected:** replicas return to the groups on the same ports within 30 s.
- **Observed:** both replicas returned after 2.9 s, the service port unchanged.
- **Limitations:** an frps restart breaks all tunnels for a few seconds.
- **Decision:** passed.

### 1.10 New replica with a health check

- **Expected:** no connection reaches it before the first successful check.
- **Observed:**
  - For 8 s with a failing health check the replica got 0 requests and did not appear in the status: frpc sends NewProxy only after a successful check.
  - After it recovered, with constant traffic of 4 connections, still 0 requests. After raising the traffic to 24 connections the replica got 3989 requests.
- **Limitations:**
  - **A new replica gets traffic only on new connections.** Under constant traffic Caddy keeps using existing keep-alive connections to the old replica: distribution is per connection, not per request (section 7).
  - Traffic evens out when connections become idle (4 s) or concurrency grows.
- **Decision:** passed. The limitation goes into the README.

## Experiment 2: replica identity and lifecycle

### 2.1 Modified frpc supplies another group, port or service

- **Expected:** rejected by the plugin and logged, without affecting other services.
- **Observed:** frpc without the connector, with a token of service `other`, announced three proxies.
  - Proxy `api.<token>.x` with the name of another service: rejected.
  - Proxy with group `svc-api` and the port of service `api`: the plugin overwrote the group to `svc-other` and the port to the port of service `other`.
  - Proxy `http` with `customDomains`: rejected.
  - The attacker received 0 requests, service api had 0 errors, and the rejections are in the entrypointd log.
- **Limitations:** none.
- **Decision:** passed.

### 2.2 NewProxy fields outside the allowlist

- **Expected:** rejected or overwritten by the plugin.
- **Observed:** direct calls to the plugin.
  - Rejected: `custom_domains`, `subdomain`, `locations`, `http_user`, `sk`, `multiplexer`, types `http`, `udp`, `stcp`, the name of another service, a name without a replica.
  - Overwritten: `group`, `group_key`, `remote_port`.
  - Login rejected for the token of a deleted service, a malformed token and a valid token with a `user` field.
- **Limitations:** the allowlist covers the NewProxy fields of frp 0.71.1. New fields in later frp versions are rejected until they are added to the list.
- **Decision:** passed.

### 2.3 Deleting a service and creating a new one

- **Expected:** the port of a deleted service never goes to a new one.
- **Observed:**
  - `x`=20000; after deletion `y`=20001; new `x`=20002;
  - all reservations stay in the database, and an attempt at `DELETE FROM ports` is blocked by a trigger;
  - the token of the old `x` is rejected for the new `x`, because the token is bound to the service identifier, not to the name.
- **Limitations:** the pool is 10,000 ports; deleted services use it up permanently.
- **Decision:** passed.

### 2.4 entrypointd and frps restarts in different orders

- **Expected:** status in entrypointd consistent with the frps state, without drift.
- **Observed:** the status matched the frps API in five sequences:
  - frps restart (consistent after 2.9 s);
  - entrypointd restart (0.3 s);
  - frps, then entrypointd (0.9 s);
  - entrypointd, then frps (1.3 s);
  - `kill -9` of both (3.6 s).
- **Limitations:** entrypointd does not keep replica state; the status always reads it from frps.
- **Decision:** passed.

### 2.5 CA rotation

- **Expected:** a client with a bundle of two CAs connects before and after the switch; a client with only the old CA is rejected after the switch.
- **Observed:**
  - after `ca rotate start` the new bundle has 2 CAs;
  - `ca status` shows the client logging in with the old bundle;
  - after `ca rotate switch` frps restarts with the new certificate (`.path` unit);
  - the client with two CAs returns after 3.0 s, and the client with the old CA does not reconnect;
  - after `ca rotate finish` the bundle has 1 CA.
- **Limitations:**
  - The switch restarts frps (a short interruption of all tunnels). Renewing the tunnel port certificate once a year also restarts frps.
  - frpc reports a TLS rejection only as "session shutdown", so the connector checks the certificate itself and emits `tls_error` with the cause.
  - Existing clients get the new CA from a new token or `entrypoint-connector rebundle`.
- **Decision:** passed.

## Experiment 3: durable token revocation

### 3.1 Revoking the token of client A (without the connector), client B on a different token

- **Expected:** A disconnected and does not reconnect; B without interruption. With the fallback frps restart, B returns within 10 s.
- **Observed:**
  - **Ping, frpc A with heartbeat:** session A cut after 7.0 s, does not reconnect for 30 s, and logins are rejected with `ep:revoked`. B traffic: 0 errors out of 1406 requests, no disconnections.
  - **Ping, frpc A without heartbeat (non-cooperating client):** session A cut after 37.3 s, when frps got no Ping for `heartbeatTimeout`. B traffic: 0 errors out of 2560 requests.
  - **`--now` (frps restart):** session A cut after 0.16 s; B returns after 2.5 s (96 errors out of 1150).
- **Limitations:**
  - Revocation through Ping cuts the session within one client heartbeat interval (10 s in the connector). A client that does not send Ping or ignores the error is cut at the latest after `heartbeatTimeout` (40 s).
  - New logins and registrations are rejected immediately.
- **Decision:** **rejecting Ping chosen.** Restarting frps remains as the `--now` option for immediate cutting at the cost of a short interruption of the other tunnels. On every heartbeat, Ping checks the session against the current snapshot, so it also covers revision rollback and service deletion.

### 3.1b Repeating test 1.6 with Ping rejection

- **Expected:** the impact of an entrypointd crash on active sessions measured and accepted; update of the failure table in section 3.
- **Observed:**
  - Process crash: no impact (test 1.6). The socket is held by systemd, and frps requests wait in the queue.
  - Full unavailability for 45 s (`entrypointd.socket` and `entrypointd.service` stopped): session cut after 6.1 s, at the nearest Ping; from that moment traffic through the tunnels ends with an error. After the start the replica is healthy after 18.2 s (frpc backoff up to 20 s).
- **Limitations:** fully stopping entrypointd (the socket too) or a permanent startup failure disables the tunnels after the nearest heartbeat. `entrypointd.socket` must not be stopped on a running gateway.
- **Decision:** impact measured; table in section 3 updated. **Accepted by the specification owner (2026-10-18).**

### 3.2 Revocation during partial application of a revision, then rollback

- **Expected:** the revocation persists; sessions opened in the meantime are re-checked.
- **Observed:** revision 6 stopped in test mode after the snapshot was written (15 s). During that time:
  - token A was revoked;
  - a new session C was opened;
  - Caddy was stopped.

  Result:
  - revision 6 has status `failed` with a Caddy admin API error;
  - the snapshot went back to revision 5;
  - token A stayed revoked, and its session was cut;
  - session C passed the re-check (Ping against the restored snapshot) and works;
  - only replica C remained in the group.
- **Limitations:** the CLI accepts issuing a token only after the revision is applied, so a session with a token that exists only in revision N cannot arise. The re-check covers all sessions on every Ping.
- **Decision:** passed.

### 3.3 Backup before revocation, machine loss, restore

- **Expected:** all restored tokens invalidated before the first login.
- **Observed:**
  - backup, then revocation of token A, then removal of the gateway container;
  - new gateway at a different IP address with the same name: `install.sh`, `entrypoint install`, `entrypoint restore`;
  - both restored tokens invalidated, including B, which was not revoked before the loss; the old token B is rejected;
  - the new token works, and HTTPS at the new IP serves traffic with the certificate from the backup.
- **Limitations:** after a restore, new tokens must be issued for all services (decision from the specification, section 6).
- **Decision:** passed.

## v0.1 readiness list (section 19)

| Item | Status |
| --- | --- |
| Results of experiments 1–3 (v0.1 scope) in `docs/results.md`, variant B adopted or rejected | Done: B adopted |
| e2e suites 1–7 and 12 and the v0.1 parts of suites 8, 9 and 11 green in Docker | Done: `make e2e`, suites Install, TLS, Routing, Tunnels, Replicas, State, Revoke, Secrets, Ops, SDK, Resilience |
| Installation on a real VPS with a real certificate | Done: smoke test below |
| Host to upstream over VPN works, including WebSocket | In Docker: upstream in a separate private network, WebSocket and SSE (`TestRouting_UpstreamWebSocketSSEAndHTTP2`). On a real VPN: **open**, because the test server has no VPN |
| Host to tunnel with two Express replicas works (since 2026-10-18 the `node:http` adapter, see below); stopping one does not interrupt the service | Done: in Docker (tests 1.1, 1.5 and 1.8, SDK matrix) and on the VPS (smoke test below) |
| A revoked token does not reconnect, and its active session is cut | Done: tests 3.1 and 1.2b |
| Backup and restore on a fresh container | Done: `TestOps_BackupRestoreFreshContainer`, test 3.3 (new IP) |
| README with installation and CLI usage | Done: `README.md` |

## Smoke test on a real server (2026-10-18)

Server: Debian 13, systemd 257, 2 vCPU, 4 GB RAM, public IPv4, no system firewall. Gateway name `tunnel.mwdentry.stream`; DNS in Cloudflare (A `@` and `*`, DNS only). Replicas: two Express applications (adapter) on a computer in WSL2 behind NAT, connected over the internet.

| Check | Result |
| --- | --- |
| `install.sh` and `entrypoint install` on a clean system | Services active. **Bug found:** entrypointd started before Caddy, and the initial revision ended with status `failed` and the gateway had no applied revision. Fix: restoring waits for the Caddy admin API, and a missing applied revision creates a new one. Regression test: `TestState_DaemonStartsBeforeCaddy`. After the update revision 2 was applied automatically |
| Let's Encrypt staging certificate (`--acme-staging`) | Issued for `test.mwdentry.stream` (issuer "(STAGING) Artificial Amaranth YE1") |
| Production certificate | Issued for `app.mwdentry.stream` (Let's Encrypt YE2), trust verification passes, HTTP/2 |
| HTTP to HTTPS redirect | 308 |
| Two replicas behind NAT | Both in the group. Sequential traffic: 20 of 20 requests to one replica (one Caddy keep-alive connection). Parallel traffic (20 at once): 44/36 |
| Drain of one replica under traffic | 300 of 300 requests ended with code 200; replica removed from the group |
| Token revocation under traffic | Session cut after 5.4 s, 400 of 400 requests ended with code 200 on the remaining replica, connector exited with a `revoked` event |
| WebSocket through the tunnel | Echo through `wss://app.mwdentry.stream/ws` |
| Ports from the internet | Open 22, 80, 443 and 55555; closed 2019, 7400, 7500, 20000 |

## Adapter change: Express to node:http (2026-10-18)

At the request of the specification owner, the adapter `@k18k/entrypoint/express` replaced the adapter `@k18k/entrypoint/http`. It accepts any `(req, res)` handler, so Express applications work unchanged, and the SDK no longer depends on Express. The drain logic is the same. The e2e tests (suites Replicas and SDK) were run again on a test application based on plain `node:http`; the experiment 1 results for the adapter apply to both versions.

## Decisions after v0.1 (2026-10-18)

- Impact of full entrypointd unavailability on sessions (test 3.1b): accepted.
- A single binary with built-in Caddy and frps and an `entrypoint run` supervisor (PID 1 in the container): rejected. The architecture stays unchanged: official Caddy and frp binaries, separate processes under systemd.

## Security issues #1–#3 (2026-10-20)

| Issue | Change | Test and result |
| --- | --- | --- |
| [#1](https://github.com/k18k/entrypoint/issues/1) Revocation after an entrypointd crash | None in code: the revocation is durable before the command returns, the snapshot after restart excludes revoked tokens, Ping re-checks every session, the systemd socket keeps queued Pings. Guarantee written into spec sections 4 and 6. | `TestRevoke_CrashRightAfterRevoke`: revoke, then immediately `kill -9 entrypointd`, with a modified client holding a streaming connection. Stream closed after 8.8 s, no reconnect, re-login refused with `ep:revoked`. |
| [#2](https://github.com/k18k/entrypoint/issues/2) Resource isolation between tokens | Per-token limits enforced by the plugin, also for valid tokens: 30 logins per minute, 20 live sessions, 60 proxy registrations per minute. frps `maxPoolCount` 10 → 2. Per-service bandwidth and connection limits are left for v0.2.0, with the decision on per-IP limits. | `TestTunnels_TokenAbuseIsolation`: 45 login and registration cycles of token A in 13.7 s, 16 rate-limited; service B 0 errors of 604, p95 2 ms (baseline 2 ms). Thresholds set before measuring: 0 errors, p95 ≤ baseline + 200 ms. |
| [#3](https://github.com/k18k/entrypoint/issues/3) ACME consent vs. wildcard routes | A wildcard route alone authorizes no certificates. Names via `--tls-name`, obtained up front. Any-subdomain on-demand only with `--tls-on-demand` (10 new names per hour). Spec section 9 updated. | `TestTLS_NoCertificateWithoutRoute` (Pebble): 10 random SNIs under the wildcard got 0 certificates, with no ACME activity in Caddy logs. The explicit name and the opted-in on-demand name got certificates. |
