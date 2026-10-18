// Aplikacja testowa e2e: Express wystawiony przez Entrypoint.
// MODE=express  — adapter connectTo(app)
// MODE=base     — kontrakt bazowy: app.listen + tunnel()
// Panel sterowania testów słucha na 0.0.0.0:9000 (poza tunelem).
import crypto from "node:crypto";
import http from "node:http";
import express from "express";
import { WebSocketServer } from "ws";
import { tunnel } from "@k18k/entrypoint";
import { connectTo } from "@k18k/entrypoint/express";

const MODE = process.env.MODE ?? "express";
const REPLICA = process.env.REPLICA ?? "r1";
const SERVICE = process.env.SERVICE ?? "api";
const DRAIN_MS = Number(process.env.DRAIN_MS ?? 30000);
const HEALTH = process.env.HEALTH ?? "/health";

const stats = {
  replica: REPLICA, requests: 0, connections: 0, openConnections: 0,
  uploadsOK: 0, uploadsAborted: 0, lastRequestAt: null, requestsAfterDrain: 0,
  drainStartedAt: null, wsCloseCodes: [], events: [],
};
let healthy = true;
let drainingFlag = false;

const app = express();
app.use((req, res, next) => {
  if (req.path !== HEALTH) {
    stats.requests++;
    stats.lastRequestAt = Date.now();
    if (drainingFlag) stats.requestsAfterDrain++;
  }
  next();
});
app.get(HEALTH, (req, res) => (healthy ? res.send("ok") : res.status(500).send("chory")));
app.get("/whoami", (req, res) => res.json({ replica: REPLICA, pid: process.pid, headers: req.headers }));
app.get("/slow", (req, res) => setTimeout(() => res.send(`ok ${REPLICA}`), Number(req.query.ms ?? 1000)));
app.post("/upload", (req, res) => {
  const h = crypto.createHash("sha256");
  let n = 0;
  let done = false;
  req.on("data", (c) => { n += c.length; h.update(c); });
  req.on("end", () => {
    done = true;
    stats.uploadsOK++;
    res.json({ bytes: n, sha256: h.digest("hex"), replica: REPLICA });
  });
  req.on("close", () => { if (!done) stats.uploadsAborted++; });
});
app.get("/sse", (req, res) => {
  res.setHeader("Content-Type", "text/event-stream");
  res.flushHeaders();
  let i = 0;
  const t = setInterval(() => res.write(`data: ${REPLICA} ${i++}\n\n`), 200);
  res.on("close", () => clearInterval(t));
});

function attachWs(server) {
  const wss = new WebSocketServer({ server, path: "/ws" });
  wss.on("connection", (ws) => {
    ws.on("message", (m) => ws.send(`${REPLICA}:${m}`));
  });
  return wss;
}

let conn; // adapter
let tun; // kontrakt bazowy
let appServer;

async function start() {
  if (MODE === "express") {
    let wss;
    conn = await connectTo(app, {
      service: SERVICE, healthCheck: HEALTH, replica: REPLICA, drainTimeoutMs: DRAIN_MS,
      healthIntervalSec: 1, healthMaxFailed: 2,
      get websockets() { return wss ? [wss] : []; },
    });
    wss = attachWs(conn.server);
    appServer = conn.server;
    tun = conn.tunnel;
  } else {
    appServer = http.createServer(app);
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
const ctl = express();
ctl.use(express.json());
ctl.get("/stats", (req, res) => res.json({ ...stats, state: tun?.state, healthy, mode: MODE }));
ctl.post("/health/:state", (req, res) => { healthy = req.params.state === "ok"; res.json({ healthy }); });
ctl.post("/drain", (req, res) => {
  drainingFlag = true;
  stats.drainStartedAt = Date.now();
  const timeoutMs = req.body?.timeoutMs;
  const p = conn ? conn.drain({ timeoutMs }) : tun.drain({ timeoutMs });
  p.then(() => console.log(JSON.stringify({ drained: true })));
  res.json({ draining: true });
});
ctl.post("/exit", (req, res) => { res.json({ exiting: true }); setTimeout(() => process.exit(0), 50); });
ctl.listen(Number(process.env.CTL_PORT ?? 9000), "0.0.0.0");

start().catch((e) => {
  console.error("start nie powiódł się:", e.message);
  process.exit(1);
});
