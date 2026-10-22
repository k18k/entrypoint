import { useState } from "react";
import { api, type VPNKey } from "../api";
import { exposeSnippets, forwardSnippets, tailscaleJoin } from "../snippets";
import { Badge, Card, Copy, ErrorText, Field, Loading, Modal, Page, ago, useAction, useData } from "../ui";

type Purpose = "expose" | "forward" | "tailscale";

function KeyDialog({ purpose, version, names, onClose }: { purpose: Purpose; version: string; names: string[]; onClose: () => void }) {
  const [name, setName] = useState("");
  const [port, setPort] = useState(purpose === "expose" ? "5432" : "5432");
  const [local, setLocal] = useState("");
  const [target, setTarget] = useState(names[0] ?? "");
  const [reusable, setReusable] = useState(false);
  const [expiration, setExpiration] = useState("24h");
  const [key, setKey] = useState<VPNKey | null>(null);
  const [tab, setTab] = useState<"systemd" | "docker">("systemd");
  const a = useAction();
  const title = { expose: "Publish a service in the VPN", forward: "Use a VPN service from an app", tailscale: "Join a machine with Tailscale" }[purpose];
  if (key) {
    const sn =
      purpose === "expose"
        ? exposeSnippets(version, name, port, local)
        : purpose === "forward"
          ? forwardSnippets(version, name, target, port)
          : null;
    return (
      <Modal title={title} onClose={onClose}>
        {sn ? (
          <>
            <p>
              <strong>ENTRYPOINT_VPN</strong> ({key.reusable ? "reusable" : "single use"}, valid until {new Date(key.expiration).toLocaleString()}). Shown
              only now.
            </p>
            <Copy text={key.bundle} secret />
            <div className="tabs" role="tablist">
              {(["systemd", "docker"] as const).map((t) => (
                <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? "active" : ""} onClick={() => setTab(t)}>
                  {{ systemd: "Linux machine or VM", docker: "Docker Compose" }[t]}
                </button>
              ))}
            </div>
            <Copy text={sn[tab]} />
            {purpose === "expose" ? (
              <p className="muted">
                Other machines in the VPN reach it as <code>{name}:{port}</code>. The key is only needed for the first start; the node keeps its
                identity in its state directory.
              </p>
            ) : (
              <p className="muted">
                The app connects to <code>{target}:{port}</code> as if the service ran next to it.
              </p>
            )}
          </>
        ) : (
          <>
            <p>Run on the machine (Tailscale client installed). It can then reach every VPN service by name, e.g. <code>{names[0] ?? "vatro-postgres"}</code>.</p>
            <Copy text={tailscaleJoin(key.server, key.key)} secret />
          </>
        )}
      </Modal>
    );
  }
  return (
    <Modal title={title} onClose={onClose}>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          await a.run(async () => setKey(await api.vpnKey(reusable, expiration)));
        }}
      >
        {purpose !== "tailscale" && (
          <Field
            label={purpose === "expose" ? "Name in the VPN" : "This app's name in the VPN"}
            hint={purpose === "expose" ? "Other machines connect to <name>:<port>, e.g. vatro-postgres." : "e.g. vatro-app"}
          >
            <input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?" required autoFocus />
          </Field>
        )}
        {purpose === "forward" && (
          <Field label="Service" hint="A machine in the VPN.">
            <input list="vpn-names" value={target} onChange={(e) => setTarget(e.target.value)} required />
            <datalist id="vpn-names">
              {names.map((n) => (
                <option key={n} value={n} />
              ))}
            </datalist>
          </Field>
        )}
        {purpose !== "tailscale" && (
          <Field label="Port">
            <input value={port} onChange={(e) => setPort(e.target.value)} inputMode="numeric" pattern="[0-9]{1,5}" required />
          </Field>
        )}
        {purpose === "expose" && (
          <Field label="Local address" hint={`Where the service listens on that machine; default 127.0.0.1:${port}.`}>
            <input value={local} onChange={(e) => setLocal(e.target.value)} placeholder={`127.0.0.1:${port}`} />
          </Field>
        )}
        <div className="grid2">
          <Field label="Key valid for">
            <select value={expiration} onChange={(e) => setExpiration(e.target.value)}>
              <option value="1h">1 hour</option>
              <option value="24h">1 day</option>
              <option value="168h">1 week</option>
              <option value="720h">30 days</option>
            </select>
          </Field>
          <label className="check">
            <input type="checkbox" checked={reusable} onChange={(e) => setReusable(e.target.checked)} /> Reusable (several machines or containers without a
            state volume)
          </label>
        </div>
        <ErrorText>{a.error}</ErrorText>
        <div className="row end">
          <button type="button" className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={a.busy}>
            Issue key
          </button>
        </div>
      </form>
    </Modal>
  );
}

export function VPN() {
  const vpn = useData(api.vpn);
  const status = useData(api.status);
  const [dialog, setDialog] = useState<Purpose | null>(null);
  const a = useAction();
  const v = vpn.data;
  if (!v) return <Page title="VPN"><Loading error={vpn.error} /></Page>;
  if (!v.enabled)
    return (
      <Page title="VPN">
        <Card>
          <p>The VPN is not set up on this gateway. Point a DNS name at it and run:</p>
          <Copy text="sudo entrypoint install --vpn-name vpn.example.com" />
        </Card>
      </Page>
    );
  const names = v.nodes.map((n) => n.name).filter((n) => n !== "gateway");
  return (
    <Page
      title="VPN"
      intro={
        <>
          Machines in the VPN reach each other by name (<code>name</code> or <code>name.{v.domain}</code>); routes can point at their addresses.
          Control server: <code>{v.name}</code>.
        </>
      }
      actions={
        <>
          <button className="primary" onClick={() => setDialog("expose")}>
            Publish a service
          </button>
          <button onClick={() => setDialog("forward")}>Use from an app</button>
          <button className="ghost" onClick={() => setDialog("tailscale")}>
            Join with Tailscale
          </button>
        </>
      }
    >
      <ErrorText>{a.error}</ErrorText>
      <Card title="Machines" actions={<button className="ghost small" onClick={vpn.reload}>Refresh</button>}>
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Addresses</th>
              <th>State</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {v.nodes.map((n) => (
              <tr key={n.id}>
                <td>
                  <strong>{n.name}</strong>
                </td>
                <td>
                  {n.addresses.map((x) => (
                    <code key={x} className="addr">
                      {x}
                    </code>
                  ))}
                </td>
                <td>{n.online ? <Badge tone="ok">online</Badge> : <Badge tone="neutral">offline · {ago(n.lastSeen)}</Badge>}</td>
                <td className="end">
                  {n.name !== "gateway" && (
                    <button
                      className="danger small"
                      disabled={a.busy}
                      onClick={() => confirm(`Remove ${n.name} from the VPN?`) && a.run(async () => (await api.removeNode(n.id), vpn.reload()))}
                    >
                      Remove
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>
      {dialog && (
        <KeyDialog
          purpose={dialog}
          version={status.data?.version ?? ""}
          names={names}
          onClose={() => {
            setDialog(null);
            vpn.reload();
          }}
        />
      )}
    </Page>
  );
}
