// Hono adapter: runs app.fetch through @hono/node-server's request listener and the node:http
// adapter, so drain works the same way (Connection: close, SSE closed, WebSockets closed with 1001).
import { expose, type Connection, type ExposeOptions, type RequestHandler } from "./http.js";

/** The part of a Hono app the adapter uses. */
export interface HonoLike {
  fetch(request: Request, ...rest: unknown[]): Response | Promise<Response>;
}

/** Exposes a Hono app through the Entrypoint gateway. Needs @hono/node-server installed. */
export async function exposeHono(app: HonoLike, opts: ExposeOptions): Promise<Connection> {
  let mod: { getRequestListener(fetch: HonoLike["fetch"]): RequestHandler };
  try {
    // An optional peer dependency, resolved from the application at run time.
    const name = "@hono/node-server";
    mod = (await import(name)) as typeof mod;
  } catch {
    throw new Error("exposeHono needs @hono/node-server: npm install @hono/node-server");
  }
  return expose(mod.getRequestListener(app.fetch), opts);
}
