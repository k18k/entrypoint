# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately via [GitHub Security Advisories](https://github.com/k18k/entrypoint/security/advisories/new), not in public issues. You should get a reply within 7 days.

## Supported versions

Only the latest release receives security fixes.

## Scope

In scope: the gateway (`entrypointd`, `entrypoint` CLI, installer), the tunnel connector and the Node.js SDK. Caddy and frp are used as unmodified official binaries; report issues in them upstream (and here too, if Entrypoint's configuration makes them exploitable).
