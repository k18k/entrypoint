// Adapter node:http: uruchamia serwer na 127.0.0.1:0 i tunel. Przy drainie przestaje
// przyjmować połączenia, odpowiada z Connection: close, zamyka strumienie SSE i
// WebSockety kodem 1001, a koniec połączeń zgłasza konektorowi.
// Działa z każdym handlerem (req, res), także z aplikacją Express czy Koa (app.callback()).
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo, Socket } from "node:net";
import { tunnel, type Tunnel, type TunnelOptions } from "./index.js";

export type RequestHandler = (req: IncomingMessage, res: ServerResponse) => void;

/** Minimalny interfejs serwera WebSocket (zgodny z biblioteką ws). */
export interface WebSocketServerLike {
  clients: Set<{ close(code?: number, reason?: string): void }>;
}

export interface ConnectOptions extends Omit<TunnelOptions, "upstream"> {
  /** Serwery WebSocket podpięte pod server (np. new WebSocketServer({ server })). */
  websockets?: WebSocketServerLike[];
  /** Obsługa SIGTERM/SIGINT: drain i zakończenie procesu (domyślnie true). */
  handleSignals?: boolean;
}

export interface Connection {
  server: Server;
  tunnel: Tunnel;
  /** Drain: wycofanie z grupy, wygaszanie połączeń, zakończenie tunelu. */
  drain(opts?: { timeoutMs?: number }): Promise<void>;
  close(): Promise<void>;
}

const isEventStream = (v: unknown): boolean =>
  String(Array.isArray(v) ? v[0] : (v ?? ""))
    .toLowerCase()
    .startsWith("text/event-stream");

// Nagłówki z res.writeHead(status, { ... }) nie są widoczne w res.getHeader(),
// więc odpowiedzi SSE rozpoznajemy także po argumentach writeHead.
function trackEventStream(res: ServerResponse, set: Set<ServerResponse>): void {
  const writeHead = res.writeHead.bind(res) as (...args: unknown[]) => ServerResponse;
  res.writeHead = ((...args: unknown[]): ServerResponse => {
    for (const a of args) {
      if (Array.isArray(a)) {
        // [k, v, k, v] albo [[k, v], ...]
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

// Ramka zamknięcia WebSocket z kodem 1001 (Going Away), bez maski (serwer -> klient).
const WS_CLOSE_1001 = Buffer.from([0x88, 0x02, 0x03, 0xe9]);

export async function connectTo(
  handler: RequestHandler,
  opts: ConnectOptions,
): Promise<Connection> {
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
      // Krok 1: konektor wycofuje replikę z grupy.
      await t.withdrawn();
      // Krok 2: wygaszanie połączeń.
      draining = true;
      server.close();
      server.closeIdleConnections();
      closeStreams();
      const allClosed = new Promise<void>((resolve) => {
        onAllClosed = resolve;
        checkDone();
      });
      await Promise.race([allClosed.then(() => t.notifyDrained()), exited]);
      // Krok 3: konektor kończy frpc; pozostałe połączenia są zrywane.
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

  // Tunel zakończony z innego powodu (np. odwołanie tokenu): zamykamy serwer.
  void t.exited.then(() => {
    if (!draining) server.close();
  });

  return { server, tunnel: t, drain, close };
}
