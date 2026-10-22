import { api } from "../api";
import { Badge, Card, Loading, Page, useData } from "../ui";

export function DNS() {
  const dns = useData(api.dns);
  const d = dns.data;
  return (
    <Page
      title="DNS"
      intro="The records each host needs. The gateway never changes DNS; set them at your DNS provider."
      actions={<button className="ghost" onClick={dns.reload}>Check again</button>}
    >
      <Card>
        {!d ? (
          <Loading error={dns.error} />
        ) : (
          <>
            {d.addresses.length === 0 && <p className="warn-text">The gateway's public address is unknown; reinstall with --public-ip.</p>}
            <table>
              <thead>
                <tr>
                  <th>Host</th>
                  <th>Required record</th>
                  <th>State</th>
                  <th>Current</th>
                </tr>
              </thead>
              <tbody>
                {d.rows.map((r) => (
                  <tr key={r.host}>
                    <td>{r.host}</td>
                    <td>
                      <code>{r.record}</code>
                    </td>
                    <td>{r.ok ? <Badge tone="ok">OK</Badge> : <Badge tone="warn">{r.problem}</Badge>}</td>
                    <td className="muted">{r.current?.join(" ") || "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </>
        )}
      </Card>
    </Page>
  );
}
