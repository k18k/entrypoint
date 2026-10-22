// SDK base contract: a tunnel to an existing local address.
// The application listens as usual, and the SDK exposes that address through the Entrypoint gateway.
// Permissions are checked by the gateway, never by the SDK.
import { spawn, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { existsSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";

export interface TunnelOptions {
  /** ENTRYPOINT_CONNECT connection bundle (from process.env by default). */
  connect?: string;
  /** Service name; must match the connection bundle. */
  service?: string;
  /** Local address of the application, e.g. http://127.0.0.1:3000. */
  upstream: string;
  /** HTTP health check path, e.g. /health. Without it the health check is TCP. */
  healthCheck?: string;
  healthIntervalSec?: number;
  healthMaxFailed?: number;
  /** Default drain time (ms). */
  drainTimeoutMs?: number;
  /** Replica name (from the hostname by default). */
  replica?: string;
  /** Path to entrypoint-connector (from the platform package or ENTRYPOINT_CONNECTOR by default). */
  connectorPath?: string;
  /** Whether tunnel() waits for the first healthy registration (default true). */
  waitReady?: boolean;
  readyTimeoutMs?: number;
  /** Where connector and frpc logs go (the process stderr by default). */
  log?: "inherit" | "ignore";
}

export type TunnelEventName =
  | "starting"
  | "connected"
  | "healthy"
  | "unhealthy"
  | "rejected"
  | "disconnected"
  | "draining"
  | "withdrawn"
  | "drained"
  | "drain_timeout"
  | "revoked"
  | "auth_failed"
  | "tls_error"
  | "connect_failed"
  | "error"
  | "exit";

export interface TunnelEvent {
  event: TunnelEventName;
  replica?: string;
  reason?: string;
  timeoutMs?: number;
  code?: number;
  time: string;
}

export class TunnelError extends Error {
  readonly event: TunnelEvent | undefined;

  constructor(message: string, event?: TunnelEvent) {
    super(message);
    this.name = "TunnelError";
    this.event = event;
  }
}

/** Tunnel handle. Connector events are only observed. */
export class Tunnel extends EventEmitter {
  replica: string = "";
  state: TunnelEventName = "starting";
  readonly exited: Promise<number>;
  #child: ChildProcess;
  #draining: Promise<number> | undefined;

  constructor(child: ChildProcess) {
    super();
    this.#child = child;
    this.exited = new Promise((resolve) => {
      child.on("exit", (code, signal) => resolve(code ?? (signal ? 1 : 0)));
    });
    if (child.stdout) {
      const rl = createInterface({ input: child.stdout });
      rl.on("line", (line) => this.#onLine(line));
    }
    // The stdin pipe is a "lifeline": when the Node process dies, the connector stops frpc and itself.
    child.stdin?.on("error", () => {});
  }

  #onLine(line: string): void {
    let ev: TunnelEvent;
    try {
      ev = JSON.parse(line) as TunnelEvent;
    } catch {
      return;
    }
    if (ev.replica) this.replica = ev.replica;
    this.state = ev.event;
    this.emit("event", ev);
    this.emit(ev.event, ev);
  }

  #send(cmd: object): void {
    const stdin = this.#child.stdin;
    if (stdin && !stdin.destroyed) stdin.write(JSON.stringify(cmd) + "\n");
  }

  /**
   * Replica drain: withdraw from the group (no new connections), wind down existing
   * connections over timeoutMs, then stop frpc. The base contract only waits for the
   * connections to expire; an adapter reports their end through notifyDrained().
   */
  drain(opts: { timeoutMs?: number } = {}): Promise<number> {
    if (!this.#draining) {
      this.#send({ cmd: "drain", timeoutMs: opts.timeoutMs ?? 0 });
      this.#draining = this.exited;
    }
    return this.#draining;
  }

  /** Waits for the replica to be withdrawn from the group (drain step 1). */
  withdrawn(): Promise<void> {
    if (this.state === "withdrawn") return Promise.resolve();
    return new Promise((resolve) => {
      this.once("withdrawn", () => resolve());
      void this.exited.then(() => resolve());
    });
  }

  /** Adapter: all connections closed, the connector may stop frpc. */
  notifyDrained(): void {
    this.#send({ cmd: "drained" });
  }

  /** Immediate tunnel shutdown, without drain. */
  close(): Promise<number> {
    this.#send({ cmd: "stop" });
    this.#child.stdin?.end();
    return this.exited;
  }
}

/** The package with the connector and frpc for this platform (an optional dependency of the SDK). */
function platformPackage(): string {
  return `@k18k/entrypoint-sdk-${process.platform}-${process.arch}`;
}

function connectorExe(): string {
  return process.platform === "win32" ? "entrypoint-connector.exe" : "entrypoint-connector";
}

/** Finds the connector binary: an explicit path, ENTRYPOINT_CONNECTOR or the platform package. */
export function findConnector(explicit?: string): string {
  const exe = connectorExe();
  const candidates: string[] = [];
  if (explicit) candidates.push(explicit);
  if (process.env.ENTRYPOINT_CONNECTOR) candidates.push(process.env.ENTRYPOINT_CONNECTOR);
  try {
    const require = createRequire(import.meta.url);
    const pkg = require.resolve(`${platformPackage()}/package.json`);
    candidates.push(join(dirname(pkg), "bin", exe));
  } catch {
    // the platform package is not installed
  }
  const found = candidates.find((c) => existsSync(c));
  if (found) return found;
  throw new TunnelError(
    `${exe} not found: ${platformPackage()} is not installed (supported: linux-x64, linux-arm64, darwin-x64, darwin-arm64, win32-x64; ` +
      `do not install with --omit=optional) and ENTRYPOINT_CONNECTOR is not set`,
  );
}

function connectorArgs(opts: TunnelOptions): string[] {
  const args = ["tunnel", "--parent-pipe", "--upstream", opts.upstream];
  if (opts.service) args.push("--service", opts.service);
  if (opts.healthCheck) args.push("--health", opts.healthCheck);
  if (opts.healthIntervalSec) args.push("--health-interval", String(opts.healthIntervalSec));
  if (opts.healthMaxFailed) args.push("--health-max-failed", String(opts.healthMaxFailed));
  if (opts.drainTimeoutMs) args.push("--drain-timeout", `${opts.drainTimeoutMs}ms`);
  if (opts.replica) args.push("--replica", opts.replica);
  return args;
}

function waitReady(t: Tunnel, timeoutMs: number): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => {
      cleanup();
      reject(new TunnelError(`tunnel was not ready within ${timeoutMs} ms (state: ${t.state})`));
    }, timeoutMs);
    const onEvent = (ev: TunnelEvent): void => {
      if (ev.event === "healthy") {
        cleanup();
        resolve();
      } else if (ev.event === "revoked" || ev.event === "auth_failed") {
        cleanup();
        reject(new TunnelError(ev.reason ?? ev.event, ev));
      }
    };
    const cleanup = (): void => {
      clearTimeout(timer);
      t.off("event", onEvent);
    };
    t.on("event", onEvent);
    void t.exited.then((code) => {
      cleanup();
      reject(new TunnelError(`connector exited (code ${code}) before becoming ready`));
    });
  });
}

/** Opens a tunnel to the application's local address. */
export async function tunnel(opts: TunnelOptions): Promise<Tunnel> {
  const connect = opts.connect ?? process.env.ENTRYPOINT_CONNECT;
  if (!connect) throw new TunnelError("missing connection bundle (ENTRYPOINT_CONNECT)");
  const bin = findConnector(opts.connectorPath);
  // The connection bundle is passed only through the connector's environment, never in arguments.
  const child = spawn(bin, connectorArgs(opts), {
    env: { ...process.env, ENTRYPOINT_CONNECT: connect },
    stdio: ["pipe", "pipe", opts.log === "ignore" ? "ignore" : "inherit"],
  });
  const t = new Tunnel(child);
  if (opts.waitReady !== false) await waitReady(t, opts.readyTimeoutMs ?? 60_000);
  return t;
}
