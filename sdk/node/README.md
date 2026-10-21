# @k18k/entrypoint-ts

Node.js SDK for [Entrypoint](https://github.com/k18k/entrypoint): exposes a local application through a tunnel to an Entrypoint gateway, with multiple replicas and drain.

## Installation

```bash
npm install @k18k/entrypoint-ts
```

The same package is published to GitHub Packages. GitHub requires a token with `read:packages` even for public packages: run `gh auth refresh -s read:packages` once, or use a classic personal access token with that scope.

```bash
echo "@k18k:registry=https://npm.pkg.github.com" >> .npmrc
echo "//npm.pkg.github.com/:_authToken=\${GITHUB_TOKEN}" >> .npmrc
GITHUB_TOKEN=$(gh auth token) npm install @k18k/entrypoint-ts
```

On install, the SDK downloads the connector and the official frpc for the current platform (Linux x64/arm64, macOS x64/arm64, Windows x64) from the [Entrypoint release](https://github.com/k18k/entrypoint/releases) and verifies their SHA256 sums. Without network access at install time (or with `--ignore-scripts`), the download happens on the first `tunnel()`, or manually: `npx entrypoint-tunnel install-connector`. Your own connector binary is selected with `ENTRYPOINT_CONNECTOR`.

Newer npm (11.x) does not run dependency install scripts by default and shows `npm warn install-scripts`. The SDK then downloads the connector on the first `expose()`/`tunnel()`. To download it at install time: `npm install-scripts approve @k18k/entrypoint-ts`.

## Usage

The gateway issues the `ENTRYPOINT_CONNECT` connection bundle: `sudo entrypoint token create --service api`.

`node:http` adapter: full drain (Connection: close, SSE and WebSockets closed with code 1001). Works with any `(req, res)` handler, including an Express app.

```js
import { expose } from "@k18k/entrypoint-ts/http";

const conn = await expose((req, res) => res.end("ok"), {
  service: "api",
  healthCheck: "/health",
});
```

Base contract: a tunnel to an address where the application is already listening.

```js
import { tunnel } from "@k18k/entrypoint-ts";

const t = await tunnel({
  service: "api",
  upstream: "http://127.0.0.1:3000",
  healthCheck: "/health",
});
process.on("SIGTERM", () => t.drain({ timeoutMs: 30_000 }).then(() => process.exit(0)));
```

CLI for any language:

```bash
ENTRYPOINT_CONNECT=... npx entrypoint-tunnel tunnel --upstream http://127.0.0.1:3000 --health /health
```

Requirements: Node.js 22+.
