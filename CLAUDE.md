# entrypoint

Self-hosted gateway for publishing services.
Source of truth: docs/specyfikacja.md. Experiment results: docs/wyniki.md.

## Rules
- Language: English for everything new (code comments, messages, docs, commits, issues).
  Existing Polish content is translated when touched.
- Scope of current work is the version named in the task; no features from later versions.
- Monorepo: gateway/ (Go: entrypointd, CLI, connector), sdk/node (TypeScript),
  e2e/ (Docker Compose), scripts/install.sh.
- Caddy and frp as official binaries at the versions in versions.json.
- Architecture: variant B (frp groups). Architecture changes only after describing
  the problem and getting my approval.
- Every feature ends with a green e2e test; `make e2e` must pass before a commit.
- Secrets never in logs, errors or command output. The tunnel client always verifies the CA.
- Small commits with English messages; don't push without asking.
- Ask for my approval before any operation on a VPS.
