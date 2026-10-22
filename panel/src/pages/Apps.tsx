import { useState } from "react";
import { api, splitHost, type ChangeResult } from "../api";
import { tunnelSnippets } from "../snippets";
import { Badge, Card, Copy, ErrorText, Field, Loading, Modal, Page, useAction, useData } from "../ui";

type Created = { service: string; health: string; connect: string; tokenId: string };

/** Shows a fresh ENTRYPOINT_CONNECT once, with ways to run the connector. */
function ConnectInfo({ c, version, onClose }: { c: Created; version: string; onClose: () => void }) {
  const [tab, setTab] = useState<"systemd" | "node" | "docker">("systemd");
  const sn = tunnelSnippets(version, c.service, c.health);
  return (
    <Modal title={`Connect replicas of ${c.service}`} onClose={onClose}>
      <p>
        <strong>ENTRYPOINT_CONNECT</strong> for token <code>{c.tokenId}</code>. It is shown only now; store it like a password.
      </p>
      <Copy text={c.connect} secret />
      <div className="tabs" role="tablist">
        {(["systemd", "node", "docker"] as const).map((t) => (
          <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? "active" : ""} onClick={() => setTab(t)}>
            {{ systemd: "Linux machine", node: "Node.js SDK", docker: "Docker Compose" }[t]}
          </button>
        ))}
      </div>
      <Copy text={sn[tab]} />
      <p className="muted">
        Every machine or container running the app is a replica; the gateway spreads connections between them and withdraws a
        replica before it stops.
      </p>
    </Modal>
  );
}

function NewApp({ onClose, onCreated }: { onClose: () => void; onCreated: (c: Created) => void }) {
  const [name, setName] = useState("");
  const [hosts, setHosts] = useState("");
  const [health, setHealth] = useState("");
  const [staging, setStaging] = useState(false);
  const [step, setStep] = useState("");
  const a = useAction();
  const list = hosts
    .split(/[\s,]+/)
    .map((h) => h.trim())
    .filter(Boolean);
  return (
    <Modal title="New app" onClose={onClose}>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          let res: ChangeResult | undefined;
          const ok = await a.run(async () => {
            setStep("creating the service");
            await api.addService(name, health);
            for (const h of list) {
              setStep(`adding ${h}`);
              await api.addRoute({ ...splitHost(h), service: name, acme: staging ? "staging" : undefined });
            }
            setStep("issuing a token");
            res = await api.createToken(name, "panel");
          });
          setStep("");
          if (ok && res?.token) onCreated({ service: name, health, connect: res.token.connect, tokenId: res.token.id });
        }}
      >
        <Field label="Name" hint="Lowercase letters, digits and dashes, e.g. vatro-web.">
          <input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?" required autoFocus />
        </Field>
        <Field label="Hosts" hint="One or more, e.g. vatro.app www.vatro.app or app.vatro.app/api. Point their DNS A records at the gateway.">
          <textarea rows={2} value={hosts} onChange={(e) => setHosts(e.target.value)} placeholder="vatro.app" />
        </Field>
        <Field label="Health check path" hint="Optional, e.g. /health. Without it a replica counts as healthy when its port accepts connections.">
          <input value={health} onChange={(e) => setHealth(e.target.value)} placeholder="/health" />
        </Field>
        <label className="check">
          <input type="checkbox" checked={staging} onChange={(e) => setStaging(e.target.checked)} /> Let's Encrypt staging certificates (for trying things out)
        </label>
        <ErrorText>{a.error && `${a.error}${step ? ` (while ${step})` : ""}`}</ErrorText>
        <div className="row end">
          <button type="button" className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={a.busy || !name}>
            {a.busy ? `${step}…` : "Create app"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

export function Apps() {
  const services = useData(api.services);
  const status = useData(api.status);
  const tokens = useData(api.tokens);
  const routes = useData(api.routes);
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<Created | null>(null);
  const a = useAction();
  const version = status.data?.version ?? "";
  const reload = () => (services.reload(), tokens.reload(), routes.reload());
  return (
    <Page
      title="Apps"
      intro="An app is published through tunnels: its replicas connect out to the gateway, which serves its hosts with automatic HTTPS."
      actions={
        <button className="primary" onClick={() => setCreating(true)}>
          New app
        </button>
      }
    >
      <ErrorText>{a.error}</ErrorText>
      {!services.data ? (
        <Loading error={services.error} />
      ) : services.data.length === 0 ? (
        <Card>
          <p className="muted">No apps yet.</p>
        </Card>
      ) : (
        services.data.map((s) => {
          const live = tokens.data?.filter((t) => t.service === s.name && !t.revoked) ?? [];
          return (
            <Card
              key={s.name}
              title={
                <>
                  {s.name} <span className="muted small">port {s.port}</span>
                </>
              }
              actions={
                <>
                  <button
                    disabled={a.busy}
                    onClick={() =>
                      a.run(async () => {
                        const r = await api.createToken(s.name, "panel");
                        if (r.token) setCreated({ service: s.name, health: s.health ?? "", connect: r.token.connect, tokenId: r.token.id });
                        tokens.reload();
                      })
                    }
                  >
                    New token
                  </button>
                  <button
                    className="danger"
                    disabled={a.busy}
                    onClick={() => {
                      if (!confirm(`Delete ${s.name} with its routes? Its port stays reserved.`)) return;
                      a.run(async () => {
                        for (const r of routes.data?.filter((r) => r.service === s.name) ?? []) await api.removeRoute(r.id);
                        await api.removeService(s.name);
                        reload();
                      });
                    }}
                  >
                    Delete
                  </button>
                </>
              }
            >
              <dl className="facts">
                <dt>Hosts</dt>
                <dd>
                  {s.routes?.length ? (
                    s.routes.map((r) => (
                      <a key={r} href={`https://${r}`} target="_blank" rel="noreferrer" className="pill">
                        {r}
                      </a>
                    ))
                  ) : (
                    <span className="muted">none, add one under Routes</span>
                  )}
                </dd>
                <dt>Replicas</dt>
                <dd>
                  {s.replicas.length === 0 ? (
                    <span className="muted">no replica connected</span>
                  ) : (
                    s.replicas.map((r) => (
                      <Badge key={r.name} tone={r.status === "online" ? "ok" : "warn"}>
                        {r.name.split(".").slice(2).join(".") || r.name} · {r.curConns} conn
                      </Badge>
                    ))
                  )}
                </dd>
                <dt>Health check</dt>
                <dd>{s.health || <span className="muted">TCP</span>}</dd>
                <dt>Tokens</dt>
                <dd>{live.length ? live.map((t) => <code key={t.id}>{t.id}</code>) : <span className="muted">none active</span>}</dd>
              </dl>
            </Card>
          );
        })
      )}
      {creating && (
        <NewApp
          onClose={() => setCreating(false)}
          onCreated={(c) => {
            setCreating(false);
            setCreated(c);
            reload();
          }}
        />
      )}
      {created && <ConnectInfo c={created} version={version} onClose={() => setCreated(null)} />}
    </Page>
  );
}
