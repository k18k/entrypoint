import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";
import { login, logout, onLoggedOut, session, type Session } from "./api";
import { ErrorText, usePath } from "./ui";
import { Overview } from "./pages/Overview";
import { Apps } from "./pages/Apps";
import { Routes } from "./pages/Routes";
import { Tokens } from "./pages/Tokens";
import { VPN } from "./pages/VPN";
import { DNS } from "./pages/DNS";
import { Revisions } from "./pages/Revisions";

const pages = [
  { path: "/", label: "Overview", el: Overview },
  { path: "/apps", label: "Apps", el: Apps },
  { path: "/vpn", label: "VPN", el: VPN },
  { path: "/routes", label: "Routes", el: Routes },
  { path: "/tokens", label: "Tokens", el: Tokens },
  { path: "/dns", label: "DNS", el: DNS },
  { path: "/revisions", label: "Revisions", el: Revisions },
];

function Login({ gateway, onDone }: { gateway: string; onDone: (s: Session) => void }) {
  const [pw, setPw] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <main className="login">
      <form
        className="card"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            onDone(await login(pw));
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setBusy(false);
          }
        }}
      >
        <div className="brand">
          <span className="logo" aria-hidden="true" />
          Entrypoint
        </div>
        <p className="muted">{gateway}</p>
        <label className="field">
          <span>Administrator password</span>
          <input type="password" autoComplete="current-password" autoFocus value={pw} onChange={(e) => setPw(e.target.value)} />
        </label>
        <ErrorText>{error}</ErrorText>
        <button className="primary" disabled={busy || !pw}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </main>
  );
}

function App() {
  const [s, setS] = useState<Session | null>(null);
  const [path, go] = usePath();
  const [menu, setMenu] = useState(false);
  useEffect(() => {
    session().then(setS, () => setS({ authenticated: false, gateway: "" }));
    return onLoggedOut(() => setS((x) => ({ authenticated: false, gateway: x?.gateway ?? "" })));
  }, []);
  if (!s) return null;
  if (!s.authenticated) return <Login gateway={s.gateway} onDone={setS} />;
  const page = pages.find((p) => p.path === path) ?? pages.find((p) => p.path !== "/" && path.startsWith(p.path)) ?? pages[0];
  const Page = page.el;
  return (
    <div className="shell">
      <nav className={`side ${menu ? "open" : ""}`}>
        <div className="brand">
          <span className="logo" aria-hidden="true" />
          Entrypoint
          <button className="ghost small menu-toggle" onClick={() => setMenu(!menu)} aria-label="Menu">
            ☰
          </button>
        </div>
        <p className="muted gw">{s.gateway}</p>
        <ul>
          {pages.map((p) => (
            <li key={p.path}>
              <a
                href={p.path}
                className={p === page ? "active" : ""}
                onClick={(e) => {
                  e.preventDefault();
                  setMenu(false);
                  go(p.path);
                }}
              >
                {p.label}
              </a>
            </li>
          ))}
        </ul>
        <button
          className="ghost small signout"
          onClick={async () => {
            await logout().catch(() => {});
            setS({ authenticated: false, gateway: s.gateway });
          }}
        >
          Sign out
        </button>
      </nav>
      <main className="content">
        <Page go={go} />
      </main>
    </div>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
