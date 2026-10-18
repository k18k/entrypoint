#!/usr/bin/env node
// npx @k18k/entrypoint tunnel --service api --upstream http://127.0.0.1:3000
import { parseArgs } from "node:util";
import { ensureConnector, tunnel, TunnelError } from "./index.js";

const USAGE = `użycie: entrypoint-tunnel tunnel --upstream http://127.0.0.1:3000 [--service <nazwa>]
       [--health /health] [--drain-timeout 30s] [--replica <nazwa>]
       entrypoint-tunnel install-connector   (pobiera konektor i frpc dla tej platformy)
Pakiet połączenia: zmienna ENTRYPOINT_CONNECT.`;

function usage(): never {
  console.error(USAGE);
  process.exit(2);
}

function parseDuration(s: string): number {
  const m = /^(\d+)(ms|s|m)?$/.exec(s);
  if (!m?.[1]) usage();
  const n = Number(m[1]);
  return m[2] === "ms" ? n : m[2] === "m" ? n * 60_000 : n * 1000;
}

async function main(): Promise<void> {
  const argv = process.argv.slice(2);
  if (argv[0] === "install-connector") {
    console.error(`entrypoint: konektor w ${await ensureConnector()}`);
    return;
  }
  if (argv[0] === "tunnel") argv.shift();
  let values;
  try {
    ({ values } = parseArgs({
      args: argv,
      options: {
        upstream: { type: "string" },
        service: { type: "string" },
        health: { type: "string" },
        replica: { type: "string" },
        "drain-timeout": { type: "string" },
      },
    }));
  } catch {
    usage();
  }
  if (!values.upstream) usage();
  const drainTimeoutMs = values["drain-timeout"] ? parseDuration(values["drain-timeout"]) : 30_000;
  const t = await tunnel({
    upstream: values.upstream,
    service: values.service,
    healthCheck: values.health,
    replica: values.replica,
    drainTimeoutMs,
  });
  console.error(`entrypoint: tunel aktywny (${t.replica})`);
  t.on("event", (e: { event: string; reason?: string }) => {
    if (["disconnected", "connected", "unhealthy", "revoked", "tls_error"].includes(e.event)) {
      console.error(`entrypoint: ${e.event}${e.reason ? ": " + e.reason : ""}`);
    }
  });
  const stop = (): void => {
    console.error("entrypoint: drain...");
    void t.drain({ timeoutMs: drainTimeoutMs }).then((code) => process.exit(code));
  };
  process.once("SIGTERM", stop);
  process.once("SIGINT", stop);
  process.exit(await t.exited);
}

main().catch((err: unknown) => {
  console.error(`entrypoint: ${err instanceof TunnelError ? err.message : String(err)}`);
  process.exit(1);
});
