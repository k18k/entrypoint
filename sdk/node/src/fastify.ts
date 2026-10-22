// Fastify adapter: waits for ready(), then serves the app through the node:http adapter, so drain
// works the same way (Connection: close, SSE closed, WebSockets closed with code 1001).
import type { IncomingMessage, ServerResponse } from "node:http";
import { expose, type Connection, type ExposeOptions } from "./http.js";

/** The parts of a Fastify instance the adapter uses (fastify 4 and 5). */
export interface FastifyLike {
  ready(): PromiseLike<unknown>;
  routing(req: IncomingMessage, res: ServerResponse): void;
  server: { emit(event: string, ...args: unknown[]): boolean };
}

/**
 * Exposes a Fastify app through the Entrypoint gateway. WebSocket upgrades are passed to the app's
 * own server, so @fastify/websocket keeps working.
 */
export async function exposeFastify(app: FastifyLike, opts: ExposeOptions): Promise<Connection> {
  await app.ready();
  const conn = await expose((req, res) => app.routing(req, res), opts);
  conn.server.on("upgrade", (req, socket, head) => app.server.emit("upgrade", req, socket, head));
  return conn;
}
