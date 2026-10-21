---
"entrypoint": minor
---

Gateway hardening after a security review:

- Wildcard routes no longer get certificates on demand for any subdomain by default. Opt in with `--tls-name <host>` (certificates only for the listed names, obtained up front) or `--tls-on-demand` (any subdomain, at most 10 new certificates per hour). With neither, its subdomains get no certificate.
- Per-token limits on the tunnel port: 30 logins per minute, 20 concurrent sessions, 60 proxy operations per minute.
- Tokens revoked while entrypointd was down stay revoked after it restarts.
- `backup restore` rejects archives with paths outside the target directory and keeps the current host's paths and addresses, so a crafted backup cannot run code as root.
