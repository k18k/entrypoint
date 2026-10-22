import { api } from "../api";
import { Badge, Card, Loading, Page, ago, useData } from "../ui";

export function Revisions() {
  const revs = useData(api.revisions);
  return (
    <Page title="Revisions" intro="Every configuration change is a numbered revision with its author and apply result.">
      <Card>
        {!revs.data ? (
          <Loading error={revs.error} />
        ) : (
          <table>
            <thead>
              <tr>
                <th>#</th>
                <th>Change</th>
                <th>Author</th>
                <th>When</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {revs.data.map((r) => (
                <tr key={r.num}>
                  <td>{r.num}</td>
                  <td>
                    {r.summary}
                    {r.error && <div className="error small">{r.error}</div>}
                  </td>
                  <td className="muted">{r.author}</td>
                  <td>{ago(r.createdAt)}</td>
                  <td>
                    <Badge tone={r.status === "applied" ? "ok" : r.status === "failed" ? "bad" : "neutral"}>{r.status}</Badge>
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
