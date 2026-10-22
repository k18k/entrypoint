import { useState } from "react";
import { api, splitHost } from "../api";
import { Badge, Card, ErrorText, Field, Loading, Modal, Page, useAction, useData } from "../ui";

function NewRoute({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const services = useData(api.services);
  const vpn = useData(api.vpn);
  const [host, setHost] = useState("");
  const [kind, setKind] = useState<"app" | "vpn" | "address">("app");
  const [service, setService] = useState("");
  const [node, setNode] = useState("");
  const [port, setPort] = useState("");
  const [address, setAddress] = useState("");
  const [strip, setStrip] = useState(false);
  const [staging, setStaging] = useState(false);
  const a = useAction();
  const nodes = vpn.data?.nodes.filter((n) => n.name !== "gateway") ?? [];
  const nodeIP = nodes.find((n) => n.name === node)?.addresses.find((x) => !x.includes(":")) ?? "";
  return (
    <Modal title="New route" onClose={onClose}>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          const upstream = kind === "vpn" ? `${nodeIP}:${port}` : kind === "address" ? address : undefined;
          const ok = await a.run(() =>
            api.addRoute({ ...splitHost(host), service: kind === "app" ? service : undefined, upstream, stripPrefix: strip || undefined, acme: staging ? "staging" : undefined }),
          );
          if (ok) onDone();
        }}
      >
        <Field label="Host and optional path prefix" hint="e.g. vatro.app, app.vatro.app/api or *.vatro.app">
          <input value={host} onChange={(e) => setHost(e.target.value)} required autoFocus />
        </Field>
        <div className="tabs" role="tablist">
          {(["app", "vpn", "address"] as const).map((k) => (
            <button type="button" key={k} role="tab" aria-selected={kind === k} className={kind === k ? "active" : ""} onClick={() => setKind(k)}>
              {{ app: "App (tunnel)", vpn: "VPN machine", address: "IP address" }[k]}
            </button>
          ))}
        </div>
        {kind === "app" && (
          <Field label="App">
            <select value={service} onChange={(e) => setService(e.target.value)} required>
              <option value="">Choose…</option>
              {services.data?.map((s) => (
                <option key={s.name}>{s.name}</option>
              ))}
            </select>
          </Field>
        )}
        {kind === "vpn" && (
          <div className="grid2">
            <Field label="Machine">
              <select value={node} onChange={(e) => setNode(e.target.value)} required>
                <option value="">Choose…</option>
                {nodes.map((n) => (
                  <option key={n.id} value={n.name}>
                    {n.name} ({n.addresses[0]})
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Port">
              <input value={port} onChange={(e) => setPort(e.target.value)} inputMode="numeric" required />
            </Field>
          </div>
        )}
        {kind === "address" && (
          <Field label="Address" hint="IP:port reachable from the gateway, e.g. 10.8.0.2:3000">
            <input value={address} onChange={(e) => setAddress(e.target.value)} required />
          </Field>
        )}
        <label className="check">
          <input type="checkbox" checked={strip} onChange={(e) => setStrip(e.target.checked)} /> Strip the path prefix before forwarding
        </label>
        <label className="check">
          <input type="checkbox" checked={staging} onChange={(e) => setStaging(e.target.checked)} /> Let's Encrypt staging certificate
        </label>
        <ErrorText>{a.error}</ErrorText>
        <div className="row end">
          <button type="button" className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={a.busy}>
            Add route
          </button>
        </div>
      </form>
    </Modal>
  );
}

export function Routes() {
  const routes = useData(api.routes);
  const [adding, setAdding] = useState(false);
  const a = useAction();
  return (
    <Page
      title="Routes"
      intro="Which host and path goes where. Certificates are issued automatically once the host's DNS points at the gateway."
      actions={
        <button className="primary" onClick={() => setAdding(true)}>
          New route
        </button>
      }
    >
      <ErrorText>{a.error}</ErrorText>
      <Card>
        {!routes.data ? (
          <Loading error={routes.error} />
        ) : routes.data.length === 0 ? (
          <p className="muted">No routes.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Host</th>
                <th>Target</th>
                <th>Options</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {routes.data.map((r) => (
                <tr key={r.id}>
                  <td>
                    <a href={`https://${r.host.replace("*.", "")}${r.prefix}`} target="_blank" rel="noreferrer">
                      {r.host}
                      {r.prefix !== "/" ? r.prefix : ""}
                    </a>
                  </td>
                  <td>{r.service ? <Badge tone="neutral">app {r.service}</Badge> : <code>{r.upstream}</code>}</td>
                  <td className="muted small">
                    {[r.stripPrefix && "strip prefix", r.acme === "staging" && "staging cert"].filter(Boolean).join(", ") || "—"}
                  </td>
                  <td className="end">
                    <button
                      className="danger small"
                      disabled={a.busy}
                      onClick={() => confirm(`Remove the route ${r.host}${r.prefix}?`) && a.run(async () => (await api.removeRoute(r.id), routes.reload()))}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
      {adding && (
        <NewRoute
          onClose={() => setAdding(false)}
          onDone={() => {
            setAdding(false);
            routes.reload();
          }}
        />
      )}
    </Page>
  );
}
