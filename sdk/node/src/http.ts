// node:http adapter: starts a server on 127.0.0.1:0 and a tunnel. On drain it stops
// accepting connections, responds with Connection: close, closes SSE streams and
// WebSockets with code 1001, and reports the end of connections to the connector.
// Works with any (req, res) handler, including an Express or Koa app (app.callback()).
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo, Socket } from "node:net";
import { tunnel, type Tunnel, type TunnelOptions } from "./index.js";

export type RequestHandler = (req: IncomingMessage, res: ServerResponse) => void;

/** Minimal WebSocket server interface (compatible with the ws library). */
export interface WebSocketServerLike {
  clients: Set<{ close(code?: number, reason?: string): void }>;
}

export interface ExposeOptions extends Omit<TunnelOptions, "upstream"> {
  /** WebSocket servers attached to the server (e.g. new WebSocketServer({ server })). */
  websockets?: WebSocketServerLike[];
  /** SIGTERM/SIGINT handling: drain and exit the process (default true). */
  handleSignals?: boolean;
}

export interface Connection {
  server: Server;
  tunnel: Tunnel;
  /** Drain: withdraw from the group, wind down connections, end the tunnel. */
  drain(opts?: { timeoutMs?: number }): Promise<void>;
  close(): Promise<void>;
}

const isEventStream = (v: unknown): boolean =>
  String(Array.isArray(v) ? v[0] : (v ?? ""))
    .toLowerCase()
    .startsWith("text/event-stream");

// Headers from res.writeHead(status, { ... }) are not visible in res.getHeader(),
// so we also recognize SSE responses by the writeHead arguments.
function trackEventStream(res: ServerResponse, set: Set<ServerResponse>): void {
  const writeHead = res.writeHead.bind(res) as (...args: unknown[]) => ServerResponse;
  res.writeHead = ((...args: unknown[]): ServerResponse => {
    for (const a of args) {
      if (Array.isArray(a)) {
        // [k, v, k, v] or [[k, v], ...]
        const flat = a.flat() as unknown[];
        for (let i = 0; i + 1 < flat.length; i += 2) {
          if (String(flat[i]).toLowerCase() === "content-type" && isEventStream(flat[i + 1]))
            set.add(res);
        }
      } else if (a && typeof a === "object") {
        for (const [k, v] of Object.entries(a)) {
          if (k.toLowerCase() === "content-type" && isEventStream(v)) set.add(res);
        }
      }
    }
    return writeHead(...args);
  }) as ServerResponse["writeHead"];
}

// WebSocket close frame with code 1001 (Going Away), unmasked (server -> client).
const WS_CLOSE_1001 = Buffer.from([0x88, 0x02, 0x03, 0xe9]);

/**
 * Exposes an HTTP handler through the Entrypoint gateway: starts a server on 127.0.0.1 (random port,
 * not reachable from the network) and a tunnel to it. Connection bundle: opts.connect or ENTRYPOINT_CONNECT.
 */
export async function expose(handler: RequestHandler, opts: ExposeOptions): Promise<Connection> {
  let draining = false;
  const sockets = new Set<Socket>();
  const upgraded = new Set<Socket>();
  const streams = new Set<ServerResponse>();
  const eventStreams = new Set<ServerResponse>();
  let onAllClosed: (() => void) | undefined;

  const checkDone = (): void => {
    if (draining && sockets.size === 0 && onAllClosed) {
      const done = onAllClosed;
      onAllClosed = undefined;
      done();
    }
  };

  const server = createServer((req, res) => {
    if (draining) res.setHeader("Connection", "close");
    trackEventStream(res, eventStreams);
    streams.add(res);
    res.on("close", () => {
      streams.delete(res);
      eventStreams.delete(res);
    });
    handler(req, res);
  });
  server.on("connection", (s: Socket) => {
    sockets.add(s);
    s.on("close", () => {
      sockets.delete(s);
      upgraded.delete(s);
      checkDone();
    });
  });
  server.on("upgrade", (_req: IncomingMessage, s: Socket) => upgraded.add(s));

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const port = (server.address() as AddressInfo).port;

  const t = await tunnel({ ...opts, upstream: `http://127.0.0.1:${port}` });

  const closeStreams = (): void => {
    for (const res of streams) {
      if (eventStreams.has(res) || isEventStream(res.getHeader("content-type"))) res.end();
    }
    const wss = opts.websockets ?? [];
    for (const w of wss) {
      for (const c of w.clients) c.close(1001, "server draining");
    }
    if (wss.length === 0) {
      for (const s of upgraded) s.end(WS_CLOSE_1001);
    }
  };

  let drainPromise: Promise<void> | undefined;
  const drain = (o: { timeoutMs?: number } = {}): Promise<void> => {
    drainPromise ??= (async () => {
      const exited = t.drain({ timeoutMs: o.timeoutMs ?? opts.drainTimeoutMs });
      // Step 1: the connector withdraws the replica from the group.
      await t.withdrawn();
      // Step 2: wind down connections.
      draining = true;
      server.close();
      server.closeIdleConnections();
      closeStreams();
      const allClosed = new Promise<void>((resolve) => {
        onAllClosed = resolve;
        checkDone();
      });
      await Promise.race([allClosed.then(() => t.notifyDrained()), exited]);
      // Step 3: the connector stops frpc; remaining connections are dropped.
      await exited;
      server.closeAllConnections();
    })();
    return drainPromise;
  };

  const close = async (): Promise<void> => {
    await t.close();
    server.closeAllConnections();
    server.close();
  };

  if (opts.handleSignals !== false) {
    const onSignal = (): void => {
      drain().then(
        () => process.exit(0),
        () => process.exit(1),
      );
    };
    process.once("SIGTERM", onSignal);
    process.once("SIGINT", onSignal);
  }

  // Tunnel ended for another reason (e.g. token revocation): close the server.
  void t.exited.then(() => {
    if (!draining) server.close();
  });

  return { server, tunnel: t, drain, close };
}

/** @deprecated Since 0.1.1: use ExposeOptions. */
export type ConnectOptions = ExposeOptions;

/** @deprecated Since 0.1.1: use expose(). */
export const connectTo: typeof expose = expose;
