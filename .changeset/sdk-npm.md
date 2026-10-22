---
"entrypoint": minor
---

The SDK is now `@k18k/entrypoint-sdk` on npm (with provenance) and on GitHub Packages; install it with `npm install @k18k/entrypoint-sdk`. Update imports from `@k18k/entrypoint` to `@k18k/entrypoint-sdk` (and `@k18k/entrypoint/http` to `@k18k/entrypoint-sdk/http`). The connector and frpc now come in per-platform optional dependencies (`@k18k/entrypoint-sdk-linux-x64` and so on), like esbuild: no install script, no download at run time, and `npx entrypoint-tunnel install-connector` and `ensureConnector()` are gone. The `.tgz` file is no longer attached to GitHub releases.
