---
"entrypoint": minor
---

Fastify and Hono adapters: `exposeFastify(app, opts)` from `@k18k/entrypoint-sdk/fastify` (waits for `ready()`, passes WebSocket upgrades to the app's server) and `exposeHono(app, opts)` from `@k18k/entrypoint-sdk/hono` (with `@hono/node-server`). Both drain like the `node:http` adapter: `Connection: close`, SSE streams closed and WebSockets closed with code 1001. Tested on Node.js 22 and 24.
