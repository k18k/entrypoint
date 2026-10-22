import { addPasskey, api, removePasskey } from "../api";
import { Card, Copy, ErrorText, Loading, Page, ago, useAction, useData } from "../ui";

export function Passkeys() {
  const keys = useData(api.passkeys);
  const a = useAction();
  return (
    <Page
      title="Passkeys"
      intro="Each passkey can sign in to this panel. Keep at least two, e.g. one in your password manager and one on a security key. Adding or removing one signs out every other session."
      actions={
        <button className="primary" disabled={a.busy} onClick={() => a.run(async () => (await addPasskey(), keys.reload()))}>
          {a.busy ? "Waiting for your passkey…" : "Add passkey"}
        </button>
      }
    >
      <ErrorText>{a.error}</ErrorText>
      <Card>
        {!keys.data ? (
          <Loading error={keys.error} />
        ) : (
          <table>
            <thead>
              <tr>
                <th>ID</th>
                <th>Name</th>
                <th>Added</th>
                <th>Last used</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {keys.data.map((k) => (
                <tr key={k.id}>
                  <td>
                    <code>{k.id}</code>
                  </td>
                  <td>{k.name}</td>
                  <td>{ago(k.createdAt)}</td>
                  <td>{ago(k.lastUsed)}</td>
                  <td className="end">
                    <button
                      className="danger small"
                      disabled={a.busy || keys.data!.length < 2}
                      title={keys.data!.length < 2 ? "The only passkey cannot be removed" : undefined}
                      onClick={() => confirm(`Remove passkey ${k.id}? It will no longer sign in.`) && a.run(async () => (await removePasskey(k.id), keys.reload()))}
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
      <Card title="From the gateway">
        <p className="muted">A one-time link that adds a passkey, e.g. when you have none at hand:</p>
        <Copy text="sudo entrypoint panel register" />
      </Card>
    </Page>
  );
}
