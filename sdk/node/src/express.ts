// Adapter Express: uruchamia serwer na 127.0.0.1:0 i tunel. Przy drainie przestaje
// przyjmować połączenia, odpowiada z Connection: close, zamyka strumienie SSE i
// WebSockety kodem 1001, a koniec połączeń zgłasza konektorowi.
import http from "node:http";
import type { AddressInfo, Socket } from "node:net";
import { tunnel, type Tunnel, type TunnelOptions } from "./index.js";

type RequestListener = (req: http.IncomingMessage, res: http.ServerResponse) => void;

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
  server: http.Server;
  tunnel: Tunnel;
  /** Drain: wycofanie z grupy, wygaszanie połączeń, zakończenie tunelu. */
  drain(opts?: { timeoutMs?: number }): Promise<void>;
  close(): Promise<void>;
}

// Ramka zamknięcia WebSocket z kodem 1001 (Going Away), bez maski (serwer -> klient).
const WS_CLOSE_1001 = Buffer.from([0x88, 0x02, 0x03, 0xe9]);

export async function connectTo(app: RequestListener, opts: ConnectOptions): Promise<Connection> {
  let draining = false;
  const sockets = new Set<Socket>();
  const upgraded = new Set<Socket>();
  const streams = new Set<http.ServerResponse>();

  const server = http.createServer((req, res) => {
    if (draining) res.setHeader("Connection", "close");
    streams.add(res);
    res.on("close", () => streams.delete(res));
    app(req, res);
  });
  server.on("connection", (s: Socket) => {
    sockets.add(s);
    s.on("close", () => {
      sockets.delete(s);
      upgraded.delete(s);
      checkDone();
    });
  });
  server.on("upgrade", (_req, s: Socket) => upgraded.add(s));

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const port = (server.address() as AddressInfo).port;

  const t = await tunnel({ ...opts, upstream: `http://127.0.0.1:${port}` });

  let drained: (() => void) | undefined;
  function checkDone() {
    if (draining && sockets.size === 0 && drained) {
      const d = drained;
      drained = undefined;
      d();
    }
  }

  let drainPromise: Promise<void> | undefined;
  function drain(o: { timeoutMs?: number } = {}): Promise<void> {
    if (drainPromise) return drainPromise;
    drainPromise = (async () => {
      const exited = t.drain({ timeoutMs: o.timeoutMs ?? opts.drainTimeoutMs });
      // Krok 1: konektor wycofuje replikę z grupy.
      await t.withdrawn();
      // Krok 2: wygaszanie połączeń.
      draining = true;
      server.close();
      server.closeIdleConnections();
      for (const res of streams) {
        const ct = String(res.getHeader("content-type") ?? "");
        if (ct.startsWith("text/event-stream")) res.end();
      }
      const handledByWs = new Set<unknown>();
      for (const wss of opts.websockets ?? []) {
        for (const c of wss.clients) {
          handledByWs.add(c);
          c.close(1001, "server draining");
        }
      }
      if (!opts.websockets?.length) {
        for (const s of upgraded) {
          s.end(WS_CLOSE_1001);
        }
      }
      const allClosed = new Promise<void>((resolve) => {
        drained = resolve;
        checkDone();
      });
      await Promise.race([allClosed.then(() => t.notifyDrained()), exited]);
      // Krok 3: konektor kończy frpc; pozostałe połączenia są zrywane.
      await exited;
      server.closeAllConnections();
    })();
    return drainPromise;
  }

  async function close() {
    await t.close();
    server.closeAllConnections();
    server.close();
  }

  if (opts.handleSignals !== false) {
    const onSignal = () => {
      drain().then(
        () => process.exit(0),
        () => process.exit(1),
      );
    };
    process.once("SIGTERM", onSignal);
    process.once("SIGINT", onSignal);
  }

  // Tunel zakończony z innego powodu (np. odwołanie tokenu): zamykamy serwer.
  t.exited.then(() => {
    if (!draining) server.close();
  });

  return { server, tunnel: t, drain, close };
}
