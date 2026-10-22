# @k18k/entrypoint-sdk

Node.js SDK for [Entrypoint](https://github.com/k18k/entrypoint): exposes a local application through a tunnel to an Entrypoint gateway, with multiple replicas and drain.

## Installation

```bash
npm install @k18k/entrypoint-sdk
```

The same package is published to GitHub Packages. GitHub requires a token with `read:packages` even for public packages: run `gh auth refresh -s read:packages` once, or use a classic personal access token with that scope.

```bash
echo "@k18k:registry=https://npm.pkg.github.com" >> .npmrc
echo "//npm.pkg.github.com/:_authToken=\${GITHUB_TOKEN}" >> .npmrc
GITHUB_TOKEN=$(gh auth token) npm install @k18k/entrypoint-sdk
```

The connector and the official frpc come in a per-platform package that npm installs next to the SDK as an optional dependency, like esbuild does: `@k18k/entrypoint-sdk-linux-x64`, `-linux-arm64`, `-darwin-x64`, `-darwin-arm64` or `-win32-x64`. Nothing is downloaded at install or run time, and no install script runs. Don't install with `--omit=optional`. Your own connector binary is selected with `ENTRYPOINT_CONNECTOR`.

## Usage

The gateway issues the `ENTRYPOINT_CONNECT` connection bundle: `sudo entrypoint token create --service api`.

`node:http` adapter: full drain (Connection: close, SSE and WebSockets closed with code 1001). Works with any `(req, res)` handler, including an Express app.

```js
import { expose } from "@k18k/entrypoint-sdk/http";

const conn = await expose((req, res) => res.end("ok"), {
  service: "api",
  healthCheck: "/health",
});
```

Base contract: a tunnel to an address where the application is already listening.

```js
import { tunnel } from "@k18k/entrypoint-sdk";

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
