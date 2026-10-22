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
  maintenance?: { since: string; reason: string };
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

export type Session = { authenticated: boolean; registered?: boolean; csrf?: string; gateway: string };

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

// WebAuthn: the server sends options with base64url strings; the browser API wants ArrayBuffers.
const b64u = {
  decode(s: string): ArrayBuffer {
    const b = atob(s.replace(/-/g, "+").replace(/_/g, "/"));
    const out = new Uint8Array(b.length);
    for (let i = 0; i < b.length; i++) out[i] = b.charCodeAt(i);
    return out.buffer;
  },
  encode(buf: ArrayBuffer | null): string | undefined {
    if (!buf) return undefined;
    let s = "";
    for (const c of new Uint8Array(buf)) s += String.fromCharCode(c);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  },
};

type Desc = { id: string; type: string; transports?: string[] };
const toDesc = (d: Desc) => ({ ...d, id: b64u.decode(d.id) }) as PublicKeyCredentialDescriptor;

/** Whether this browser can use passkeys at all. */
export const passkeysSupported = () => typeof window.PublicKeyCredential === "function" && window.isSecureContext;

/** Adds a passkey with the one-time token from `entrypoint panel register`; signs in on success. */
export const registerPasskey = (token: string) => createPasskey("/register/begin", { token });

/** Adds another passkey for the signed-in administrator (other sessions end). */
export const addPasskey = () => createPasskey("/passkeys/begin");

async function createPasskey(begin: string, body?: unknown): Promise<Session> {
  const { ceremony, options: o } = await call<{ ceremony: string; options: any }>("POST", begin, body);
  const cred = (await navigator.credentials.create({
    publicKey: {
      ...o,
      challenge: b64u.decode(o.challenge),
      user: { ...o.user, id: b64u.decode(o.user.id) },
      excludeCredentials: (o.excludeCredentials ?? []).map(toDesc),
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("no passkey was created");
  const r = cred.response as AuthenticatorAttestationResponse;
  const s = await call<Session>(`POST`, `/register/finish?ceremony=${encodeURIComponent(ceremony)}`, {
    id: cred.id,
    rawId: b64u.encode(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: cred.getClientExtensionResults(),
    response: {
      clientDataJSON: b64u.encode(r.clientDataJSON),
      attestationObject: b64u.encode(r.attestationObject),
      transports: r.getTransports?.() ?? [],
    },
  });
  csrf = s.csrf ?? "";
  return s;
}

/** Signs in with any passkey registered for this panel. */
export async function loginWithPasskey(): Promise<Session> {
  const { ceremony, options: o } = await call<{ ceremony: string; options: any }>("POST", "/login/begin");
  const cred = (await navigator.credentials.get({
    publicKey: { ...o, challenge: b64u.decode(o.challenge), allowCredentials: (o.allowCredentials ?? []).map(toDesc) },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("no passkey was chosen");
  const r = cred.response as AuthenticatorAssertionResponse;
  const s = await call<Session>("POST", `/login/finish?ceremony=${encodeURIComponent(ceremony)}`, {
    id: cred.id,
    rawId: b64u.encode(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: cred.getClientExtensionResults(),
    response: {
      clientDataJSON: b64u.encode(r.clientDataJSON),
      authenticatorData: b64u.encode(r.authenticatorData),
      signature: b64u.encode(r.signature),
      userHandle: b64u.encode(r.userHandle),
    },
  });
  csrf = s.csrf ?? "";
  return s;
}

export async function logout() {
  await call("POST", "/logout");
  csrf = "";
}

export type Passkey = { id: string; name: string; createdAt: string; lastUsed?: string };

export async function removePasskey(id: string) {
  const s = await call<Session>("DELETE", `/passkeys/${encodeURIComponent(id)}`);
  csrf = s.csrf ?? "";
}

export const api = {
  passkeys: () => call<Passkey[]>("GET", "/passkeys"),
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
