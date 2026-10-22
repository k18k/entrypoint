import { api } from "../api";
import { Badge, Card, Loading, Page, ago, useData } from "../ui";

export function Overview({ go }: { go: (p: string) => void }) {
  const st = useData(api.status);
  const vpn = useData(api.vpn);
  const s = st.data;
  if (!s) return <Page title="Overview"><Loading error={st.error} /></Page>;
  const replicas = s.services.reduce((n, x) => n + x.replicas.filter((r) => r.status === "online").length, 0);
  const online = vpn.data?.nodes.filter((n) => n.online).length ?? 0;
  return (
    <Page title="Overview" intro={`Gateway ${s.gatewayName}, version ${s.version}`} actions={<button className="ghost" onClick={() => (st.reload(), vpn.reload())}>Refresh</button>}>
      <div className="stats">
        <button className="stat" onClick={() => go("/apps")}>
          <span className="num">{s.services.length}</span>
          <span>apps</span>
          <small className="muted">{replicas} replicas online</small>
        </button>
        <button className="stat" onClick={() => go("/vpn")}>
          <span className="num">{vpn.data?.enabled ? vpn.data.nodes.length : "—"}</span>
          <span>VPN machines</span>
          <small className="muted">{vpn.data?.enabled ? `${online} online` : "VPN not set up"}</small>
        </button>
        <button className="stat" onClick={() => go("/revisions")}>
          <span className="num">{s.lastApplied?.num ?? "—"}</span>
          <span>applied revision</span>
          <small className="muted">{ago(s.lastApplied?.createdAt)}</small>
        </button>
        <div className="stat">
          <span className="row">
            <Badge tone={s.caddyOk ? "ok" : "bad"}>Caddy</Badge>
            <Badge tone={s.frpsOk ? "ok" : "bad"}>frps</Badge>
          </span>
          <span>components</span>
          <small className="muted">tunnel port {s.tunnelPort}</small>
        </div>
      </div>
      {s.warnings && s.warnings.length > 0 && (
        <Card title="Warnings">
          <ul className="list">
            {s.warnings.map((w) => (
              <li key={w} className="warn-text">
                {w}
              </li>
            ))}
          </ul>
        </Card>
      )}
      <Card title="Apps" actions={<button onClick={() => go("/apps")}>Manage</button>}>
        {s.services.length === 0 ? (
          <p className="muted">No apps yet. Create one to publish a service through a tunnel.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>App</th>
                <th>Hosts</th>
                <th>Replicas</th>
              </tr>
            </thead>
            <tbody>
              {s.services.map((x) => (
                <tr key={x.name}>
                  <td>{x.name}</td>
                  <td>{x.routes?.map((r) => r.replace(/\/$/, "")).join(", ") || <span className="muted">no route</span>}</td>
                  <td>
                    {x.replicas.length === 0 ? (
                      <Badge tone="neutral">none</Badge>
                    ) : (
                      x.replicas.map((r) => (
                        <Badge key={r.name} tone={r.status === "online" ? "ok" : "warn"}>
                          {r.name}
                        </Badge>
                      ))
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
    </Page>
  );
}
