// Aplikacja testowa e2e na natywnym node:http, wystawiona przez Entrypoint.
// MODE=adapter — adapter expose(handler) z @k18k/entrypoint-ts/http
// MODE=base    — kontrakt bazowy: server.listen + tunnel()
// Panel sterowania testów słucha na 0.0.0.0:CTL_PORT (poza tunelem).
import crypto from "node:crypto";
import http from "node:http";
import { WebSocketServer } from "ws";
import { tunnel } from "@k18k/entrypoint-ts";
import { connectTo, expose } from "@k18k/entrypoint-ts/http";

const MODE = process.env.MODE ?? "adapter";
const REPLICA = process.env.REPLICA ?? "r1";
const SERVICE = process.env.SERVICE ?? "api";
const DRAIN_MS = Number(process.env.DRAIN_MS ?? 30000);
const HEALTH = process.env.HEALTH ?? "/health";

const stats = {
  replica: REPLICA, requests: 0, connections: 0, openConnections: 0,
  uploadsOK: 0, uploadsAborted: 0, lastRequestAt: null, requestsAfterDrain: 0,
  drainStartedAt: null, events: [],
};
let healthy = process.env.START_UNHEALTHY !== "1";
let drainingFlag = false;

function json(res, code, body) {
  res.writeHead(code, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
}

function handler(req, res) {
  const url = new URL(req.url, "http://app");
  if (url.pathname === HEALTH) {
    if (healthy) return res.end("ok");
    res.statusCode = 500;
    return res.end("chory");
  }
  stats.requests++;
  stats.lastRequestAt = Date.now();
  if (drainingFlag) stats.requestsAfterDrain++;
  // Chora replika zwraca błędy dla wszystkich żądań (test 1.5).
  if (!healthy) return json(res, 500, { replica: REPLICA, error: "chora" });
  switch (url.pathname) {
    case "/whoami":
      return json(res, 200, { replica: REPLICA, pid: process.pid, headers: req.headers });
    case "/slow":
      return setTimeout(() => res.end(`ok ${REPLICA}`), Math.min(Number(url.searchParams.get("ms") ?? 1000), 60_000));
    case "/upload": {
      const h = crypto.createHash("sha256");
      let n = 0;
      let done = false;
      req.on("data", (c) => { n += c.length; h.update(c); });
      req.on("end", () => {
        done = true;
        stats.uploadsOK++;
        json(res, 200, { bytes: n, sha256: h.digest("hex"), replica: REPLICA });
      });
      req.on("close", () => { if (!done) stats.uploadsAborted++; });
      return;
    }
    case "/sse": {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      let i = 0;
      const t = setInterval(() => res.write(`data: ${REPLICA} ${i++}\n\n`), 200);
      res.on("close", () => clearInterval(t));
      return;
    }
    default:
      return json(res, 200, { replica: REPLICA, path: url.pathname });
  }
}

function attachWs(server) {
  const wss = new WebSocketServer({ server, path: "/ws" });
  wss.on("connection", (ws) => ws.on("message", (m) => ws.send(`${REPLICA}:${m}`)));
  return wss;
}

let conn; // adapter
let tun; // kontrakt bazowy
let appServer;

async function start() {
  if (MODE === "adapter") {
    let wss;
    // CONNECT_TO=1: przestarzały alias connectTo (zgodność z 0.1.0).
    conn = await (process.env.CONNECT_TO === "1" ? connectTo : expose)(handler, {
      service: SERVICE, healthCheck: HEALTH, replica: REPLICA, drainTimeoutMs: DRAIN_MS,
      healthIntervalSec: 1, healthMaxFailed: 2,
      get websockets() { return wss ? [wss] : []; },
    });
    wss = attachWs(conn.server);
    appServer = conn.server;
    tun = conn.tunnel;
  } else {
    appServer = http.createServer(handler);
    attachWs(appServer);
    await new Promise((r) => appServer.listen(Number(process.env.APP_PORT ?? 3000), "127.0.0.1", r));
    tun = await tunnel({
      service: SERVICE, upstream: `http://127.0.0.1:${appServer.address().port}`, healthCheck: HEALTH,
      replica: REPLICA, drainTimeoutMs: DRAIN_MS, healthIntervalSec: 1, healthMaxFailed: 2,
    });
    process.once("SIGTERM", () => tun.drain().then(() => process.exit(0)));
  }
  appServer.on("connection", (s) => {
    stats.connections++;
    stats.openConnections++;
    s.on("close", () => stats.openConnections--);
  });
  tun.on("event", (e) => {
    stats.events.push({ event: e.event, t: Date.now(), reason: e.reason });
    console.log(JSON.stringify({ tunnelEvent: e.event, reason: e.reason }));
  });
  console.log(JSON.stringify({ ready: true, mode: MODE, replica: tun.replica }));
}

// Panel sterowania testów.
function readBody(req) {
  return new Promise((resolve) => {
    let b = "";
    req.on("data", (c) => (b += c));
    req.on("end", () => { try { resolve(JSON.parse(b || "{}")); } catch { resolve({}); } });
  });
}

http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://ctl");
  const body = req.method === "POST" ? await readBody(req) : {};
  if (url.pathname === "/stats") return json(res, 200, { ...stats, state: tun?.state, healthy, mode: MODE });
  if (url.pathname.startsWith("/health/")) {
    healthy = url.pathname === "/health/ok";
    return json(res, 200, { healthy });
  }
  if (url.pathname === "/drain") {
    drainingFlag = true;
    stats.drainStartedAt = Date.now();
    const p = conn ? conn.drain({ timeoutMs: body.timeoutMs }) : tun.drain({ timeoutMs: body.timeoutMs });
    p.then(() => console.log(JSON.stringify({ drained: true })));
    return json(res, 200, { draining: true });
  }
  if (url.pathname === "/exit") {
    json(res, 200, { exiting: true });
    return setTimeout(() => process.exit(0), 50);
  }
  json(res, 404, { error: "brak" });
}).listen(Number(process.env.CTL_PORT ?? 9000), "0.0.0.0");

start().catch((e) => {
  console.error("start nie powiódł się:", e.message);
  process.exit(1);
});
