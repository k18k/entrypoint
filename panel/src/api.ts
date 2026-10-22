// Client for the panel backend. Everything under /api/v1 is the entrypointd API, proxied by the
// backend after it checks the session; changes carry the CSRF token from the login.

export type Revision = {
  num: number;
  author: string;
  createdAt: string;
  summary: string;
  diff: string[] | null;
  status: string;
  error?: string;
};

export type Replica = { name: string; tokenId: string; status: string; curConns: number; since?: string };
export type Service = { name: string; port: number; health?: string; routes?: string[]; replicas: Replica[] };

export type Route = {
  id: string;
  host: string;
  prefix: string;
  service?: string;
  upstream?: string;
  stripPrefix?: boolean;
  maxBodyBytes: number;
  responseHeaderTimeout: number;
  acme?: string;
};

export type Token = {
  id: string;
  service: string;
  scope: string;
  label?: string;
  createdAt: string;
  revoked: boolean;
  revokedAt?: string;
  lastLogin?: { lastLogin: string; clientIp: string; hostname: string; version: string };
};

export type Status = {
  version: string;
  gatewayName: string;
  tunnelPort: number;
  head: Revision | null;
  lastApplied: Revision | null;
  components: { name: string; applied: number }[];
  services: Service[];
  frpsOk: boolean;
  caddyOk: boolean;
  warnings: string[] | null;
};

export type VPNNode = { id: number; name: string; addresses: string[]; online: boolean; lastSeen?: string };
export type VPN = { enabled: boolean; name?: string; domain?: string; nodes: VPNNode[] };
export type VPNKey = { key: string; bundle: string; server: string; reusable: boolean; expiration: string };

export type DNSRow = { host: string; record: string; current: string[] | null; ok: boolean; problem?: string };
export type DNS = { rows: DNSRow[]; addresses: string[] };

export type ChangeResult = {
  revision?: Revision;
  applied: boolean;
  error?: string;
  token?: { id: string; service: string; connect: string };
};

export type Session = { authenticated: boolean; csrf?: string; gateway: string };

let csrf = "";
const listeners = new Set<() => void>();

/** Called when the backend says the session is gone. */
export function onLoggedOut(f: () => void) {
  listeners.add(f);
  return () => {
    listeners.delete(f);
  };
}

export class APIError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = csrf;
  const res = await fetch("/api" + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
  });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { error: text };
  }
  if (res.status === 401 && path.startsWith("/v1/")) listeners.forEach((f) => f());
  if (!res.ok) {
    const msg = (data as { error?: string } | null)?.error ?? `HTTP ${res.status}`;
    throw new APIError(msg, res.status);
  }
  return data as T;
}

export async function session(): Promise<Session> {
  const s = await call<Session>("GET", "/session");
  csrf = s.csrf ?? "";
  return s;
}

export async function login(password: string): Promise<Session> {
  const s = await call<Session>("POST", "/login", { password });
  csrf = s.csrf ?? "";
  return s;
}

export async function logout() {
  await call("POST", "/logout");
  csrf = "";
}

export const api = {
  status: () => call<Status>("GET", "/v1/status"),
  services: () => call<Service[]>("GET", "/v1/services"),
  addService: (name: string, health: string) => call<ChangeResult>("POST", "/v1/services", { name, health: health || undefined }),
  removeService: (name: string) => call<ChangeResult>("DELETE", `/v1/services/${encodeURIComponent(name)}`),
  routes: () => call<Route[]>("GET", "/v1/routes"),
  addRoute: (r: { host: string; prefix: string; service?: string; upstream?: string; stripPrefix?: boolean; acme?: string }) =>
    call<ChangeResult>("POST", "/v1/routes", r),
  removeRoute: (id: string) => call<ChangeResult>("DELETE", `/v1/routes/${encodeURIComponent(id)}`),
  tokens: () => call<Token[]>("GET", "/v1/tokens"),
  createToken: (service: string, label: string) => call<ChangeResult>("POST", "/v1/tokens", { service, label: label || undefined }),
  revokeToken: (id: string) => call<{ revoked: string }>("POST", `/v1/tokens/${encodeURIComponent(id)}/revoke`),
  revisions: () => call<Revision[]>("GET", "/v1/revisions?limit=100"),
  dns: () => call<DNS>("GET", "/v1/dns"),
  vpn: () => call<VPN>("GET", "/v1/vpn"),
  vpnKey: (reusable: boolean, expiration: string) => call<VPNKey>("POST", "/v1/vpn/keys", { reusable, expiration }),
  removeNode: (id: number) => call<unknown>("DELETE", `/v1/vpn/nodes/${id}`),
};

/** Splits "host/prefix" into its parts. */
export function splitHost(s: string): { host: string; prefix: string } {
  const t = s.trim().replace(/^https?:\/\//, "");
  const i = t.indexOf("/");
  return i < 0 ? { host: t.toLowerCase(), prefix: "/" } : { host: t.slice(0, i).toLowerCase(), prefix: t.slice(i) };
}
