#!/usr/bin/env node
// npx @k18k/entrypoint tunnel --service api --upstream http://127.0.0.1:3000
import { tunnel, TunnelError } from "./index.js";

function usage(): never {
  console.error(`użycie: entrypoint-tunnel tunnel --upstream http://127.0.0.1:3000 [--service <nazwa>]
       [--health /health] [--drain-timeout 30s] [--replica <nazwa>]
Pakiet połączenia: zmienna ENTRYPOINT_CONNECT.`);
  process.exit(2);
}

function parseDuration(s: string): number {
  const m = /^(\d+)(ms|s|m)?$/.exec(s);
  if (!m) usage();
  const n = Number(m[1]);
  return m[2] === "ms" ? n : m[2] === "m" ? n * 60_000 : n * 1000;
}

async function main() {
  const argv = process.argv.slice(2);
  if (argv[0] === "tunnel") argv.shift();
  const flags: Record<string, string> = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith("--")) usage();
    const [k, v] = a.includes("=") ? a.slice(2).split("=", 2) : [a.slice(2), argv[++i]];
    if (v === undefined) usage();
    flags[k] = v;
  }
  if (!flags.upstream) usage();
  const drainTimeoutMs = flags["drain-timeout"] ? parseDuration(flags["drain-timeout"]) : 30_000;
  const t = await tunnel({
    upstream: flags.upstream,
    service: flags.service,
    healthCheck: flags.health,
    replica: flags.replica,
    drainTimeoutMs,
  });
  console.error(`entrypoint: tunel aktywny (${t.replica})`);
  t.on("event", (e) => {
    if (["disconnected", "connected", "unhealthy", "revoked"].includes(e.event)) {
      console.error(`entrypoint: ${e.event}${e.reason ? ": " + e.reason : ""}`);
    }
  });
  const stop = () => {
    console.error("entrypoint: drain...");
    t.drain({ timeoutMs: drainTimeoutMs }).then((code) => process.exit(code));
  };
  process.once("SIGTERM", stop);
  process.once("SIGINT", stop);
  const code = await t.exited;
  process.exit(code);
}

main().catch((err) => {
  console.error(`entrypoint: ${err instanceof TunnelError ? err.message : err}`);
  process.exit(1);
});
