import { api } from "../api";
import { Badge, Card, ErrorText, Loading, Page, ago, useAction, useData } from "../ui";

export function Tokens() {
  const tokens = useData(api.tokens);
  const a = useAction();
  return (
    <Page title="Tokens" intro="Each token lets replicas of one app connect. Revoking it rejects new logins at once and ends its sessions within seconds.">
      <ErrorText>{a.error}</ErrorText>
      <Card>
        {!tokens.data ? (
          <Loading error={tokens.error} />
        ) : tokens.data.length === 0 ? (
          <p className="muted">No tokens. Create an app to get one.</p>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Token</th>
                <th>App</th>
                <th>Created</th>
                <th>Last login</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {tokens.data.map((t) => (
                <tr key={t.id} className={t.revoked ? "dim" : ""}>
                  <td>
                    <code>{t.id}</code> {t.label && <span className="muted small">{t.label}</span>}
                  </td>
                  <td>{t.service}</td>
                  <td>{ago(t.createdAt)}</td>
                  <td>{t.lastLogin ? `${ago(t.lastLogin.lastLogin)} from ${t.lastLogin.hostname || t.lastLogin.clientIp}` : <span className="muted">never</span>}</td>
                  <td className="end">
                    {t.revoked ? (
                      <Badge tone="bad">revoked</Badge>
                    ) : (
                      <button
                        className="danger small"
                        disabled={a.busy}
                        onClick={() => confirm(`Revoke ${t.id}? Replicas using it disconnect.`) && a.run(async () => (await api.revokeToken(t.id), tokens.reload()))}
                      >
                        Revoke
                      </button>
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
