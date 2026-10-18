// Kontrakt bazowy SDK: tunel do istniejącego lokalnego adresu.
// Aplikacja słucha jak zwykle, a SDK wystawia ten adres przez bramę Entrypoint.
// Uprawnienia sprawdza brama, nigdy SDK.
import { spawn, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { existsSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";

export interface TunnelOptions {
  /** Pakiet połączenia ENTRYPOINT_CONNECT (domyślnie z process.env). */
  connect?: string;
  /** Nazwa serwisu; musi zgadzać się z pakietem połączenia. */
  service?: string;
  /** Lokalny adres aplikacji, np. http://127.0.0.1:3000. */
  upstream: string;
  /** Ścieżka health checku HTTP, np. /health. Bez niej health check jest TCP. */
  healthCheck?: string;
  healthIntervalSec?: number;
  healthMaxFailed?: number;
  /** Domyślny czas drainu (ms). */
  drainTimeoutMs?: number;
  /** Nazwa repliki (domyślnie z nazwy hosta). */
  replica?: string;
  /** Ścieżka do entrypoint-connector (domyślnie z pakietu platformy albo ENTRYPOINT_CONNECTOR). */
  connectorPath?: string;
  /** Czy tunnel() ma czekać na pierwszą zdrową rejestrację (domyślnie true). */
  waitReady?: boolean;
  readyTimeoutMs?: number;
  /** Gdzie trafiają logi konektora i frpc (domyślnie stderr procesu). */
  log?: "inherit" | "ignore";
}

export type TunnelEventName =
  | "starting" | "connected" | "healthy" | "unhealthy" | "rejected" | "disconnected"
  | "draining" | "withdrawn" | "drained" | "drain_timeout" | "revoked" | "auth_failed"
  | "tls_error" | "error" | "exit";

export interface TunnelEvent {
  event: TunnelEventName;
  replica?: string;
  reason?: string;
  timeoutMs?: number;
  code?: number;
  time: string;
}

export class TunnelError extends Error {
  constructor(message: string, readonly event?: TunnelEvent) {
    super(message);
    this.name = "TunnelError";
  }
}

/** Uchwyt tunelu. Zdarzenia konektora są tylko obserwowane. */
export class Tunnel extends EventEmitter {
  replica = "";
  state: TunnelEventName = "starting";
  readonly exited: Promise<number>;
  private child: ChildProcess;
  private draining?: Promise<number>;

  constructor(child: ChildProcess) {
    super();
    this.child = child;
    this.exited = new Promise((resolve) => {
      child.on("exit", (code, signal) => resolve(code ?? (signal ? 1 : 0)));
    });
    const rl = createInterface({ input: child.stdout! });
    rl.on("line", (line) => {
      let ev: TunnelEvent;
      try {
        ev = JSON.parse(line);
      } catch {
        return;
      }
      if (ev.replica) this.replica = ev.replica;
      this.state = ev.event;
      this.emit("event", ev);
      this.emit(ev.event, ev);
    });
    // Rura stdin jest „linią życia”: gdy proces Node zginie, konektor kończy frpc i siebie.
    child.stdin!.on("error", () => {});
  }

  private send(cmd: object) {
    if (this.child.stdin && !this.child.stdin.destroyed) {
      this.child.stdin.write(JSON.stringify(cmd) + "\n");
    }
  }

  /**
   * Drain repliki: wycofanie z grupy (brak nowych połączeń), wygaszanie istniejących
   * połączeń przez timeoutMs, potem zakończenie frpc. Kontrakt bazowy czeka tylko na
   * wygaśnięcie połączeń; adapter frameworka zgłasza ich koniec przez notifyDrained().
   */
  drain(opts: { timeoutMs?: number } = {}): Promise<number> {
    if (!this.draining) {
      this.send({ cmd: "drain", timeoutMs: opts.timeoutMs ?? 0 });
      this.draining = this.exited;
    }
    return this.draining;
  }

  /** Czeka na wycofanie repliki z grupy (krok 1 drainu). */
  withdrawn(): Promise<void> {
    if (this.state === "withdrawn") return Promise.resolve();
    return new Promise((resolve) => {
      this.once("withdrawn", () => resolve());
      this.exited.then(() => resolve());
    });
  }

  /** Adapter: wszystkie połączenia zamknięte, konektor może zakończyć frpc. */
  notifyDrained() {
    this.send({ cmd: "drained" });
  }

  /** Natychmiastowe zamknięcie tunelu, bez drainu. */
  close(): Promise<number> {
    this.send({ cmd: "stop" });
    this.child.stdin?.end();
    return this.exited;
  }
}

function platformPackage(): string {
  const arch = process.arch === "x64" ? "x64" : process.arch;
  return `@k18k/entrypoint-${process.platform}-${arch}`;
}

/** Znajduje binarkę konektora. */
export function findConnector(explicit?: string): string {
  const exe = process.platform === "win32" ? "entrypoint-connector.exe" : "entrypoint-connector";
  const candidates: string[] = [];
  if (explicit) candidates.push(explicit);
  if (process.env.ENTRYPOINT_CONNECTOR) candidates.push(process.env.ENTRYPOINT_CONNECTOR);
  try {
    const require = createRequire(import.meta.url);
    const pkg = require.resolve(`${platformPackage()}/package.json`);
    candidates.push(join(dirname(pkg), "bin", exe));
  } catch {
    // pakiet platformy nie jest zainstalowany
  }
  for (const c of candidates) {
    if (existsSync(c)) return c;
  }
  throw new TunnelError(
    `nie znaleziono ${exe}; zainstaluj ${platformPackage()} albo ustaw ENTRYPOINT_CONNECTOR`,
  );
}

/** Otwiera tunel do lokalnego adresu aplikacji. */
export async function tunnel(opts: TunnelOptions): Promise<Tunnel> {
  const connect = opts.connect ?? process.env.ENTRYPOINT_CONNECT;
  if (!connect) throw new TunnelError("brak pakietu połączenia (ENTRYPOINT_CONNECT)");
  const bin = findConnector(opts.connectorPath);
  const args = ["tunnel", "--parent-pipe", "--upstream", opts.upstream];
  if (opts.service) args.push("--service", opts.service);
  if (opts.healthCheck) args.push("--health", opts.healthCheck);
  if (opts.healthIntervalSec) args.push("--health-interval", String(opts.healthIntervalSec));
  if (opts.healthMaxFailed) args.push("--health-max-failed", String(opts.healthMaxFailed));
  if (opts.drainTimeoutMs) args.push("--drain-timeout", `${opts.drainTimeoutMs}ms`);
  if (opts.replica) args.push("--replica", opts.replica);
  // Pakiet połączenia przekazujemy wyłącznie przez środowisko konektora, nigdy w argumentach.
  const env = { ...process.env, ENTRYPOINT_CONNECT: connect };
  const child = spawn(bin, args, {
    env,
    stdio: ["pipe", "pipe", opts.log === "ignore" ? "ignore" : "inherit"],
  });
  const t = new Tunnel(child);
  if (opts.waitReady === false) return t;

  const timeoutMs = opts.readyTimeoutMs ?? 60_000;
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => {
      cleanup();
      reject(new TunnelError(`tunel nie był gotowy w ${timeoutMs} ms (stan: ${t.state})`));
    }, timeoutMs);
    const onEvent = (ev: TunnelEvent) => {
      if (ev.event === "healthy") {
        cleanup();
        resolve();
      } else if (ev.event === "revoked" || ev.event === "auth_failed") {
        cleanup();
        reject(new TunnelError(ev.reason ?? ev.event, ev));
      }
    };
    const onExit = (code: number) => {
      cleanup();
      reject(new TunnelError(`konektor zakończył działanie (kod ${code}) przed gotowością`));
    };
    const cleanup = () => {
      clearTimeout(timer);
      t.off("event", onEvent);
    };
    t.on("event", onEvent);
    t.exited.then(onExit);
  });
  return t;
}
