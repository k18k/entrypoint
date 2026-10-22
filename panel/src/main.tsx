import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";
import { loginWithPasskey, logout, onLoggedOut, passkeysSupported, registerPasskey, session, type Session } from "./api";
import { Copy, ErrorText, usePath } from "./ui";
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

function Brand({ gateway }: { gateway: string }) {
  return (
    <>
      <div className="brand">
        <span className="logo" aria-hidden="true" />
        Entrypoint
      </div>
      <p className="muted">{gateway}</p>
    </>
  );
}

function Login({ s, onDone }: { s: Session; onDone: (s: Session) => void }) {
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <main className="login">
      <div className="card">
        <Brand gateway={s.gateway} />
        {!passkeysSupported() ? (
          <ErrorText>This browser does not support passkeys.</ErrorText>
        ) : s.registered === false ? (
          <>
            <p>No passkey is registered yet. On the gateway run:</p>
            <Copy text="sudo entrypoint panel register" />
            <p className="muted small">It prints a one-time link, valid for one minute, that adds your passkey.</p>
          </>
        ) : (
          <>
            <ErrorText>{error}</ErrorText>
            <button
              className="primary"
              disabled={busy}
              autoFocus
              onClick={async () => {
                setBusy(true);
                setError("");
                try {
                  onDone(await loginWithPasskey());
                } catch (err) {
                  setError(err instanceof DOMException && err.name === "NotAllowedError" ? "Sign-in was cancelled or timed out." : (err as Error).message);
                } finally {
                  setBusy(false);
                }
              }}
            >
              {busy ? "Waiting for your passkey…" : "Sign in with a passkey"}
            </button>
          </>
        )}
      </div>
    </main>
  );
}

// The one-time token of /register#<token>, read once at load and dropped from the address bar.
const registrationToken = location.pathname === "/register" ? location.hash.slice(1) : "";
if (registrationToken) history.replaceState(null, "", "/register");

/** /register#<token>: adds a passkey with the one-time link from `entrypoint panel register`. */
function Register({ gateway, onDone }: { gateway: string; onDone: (s: Session) => void }) {
  const token = registrationToken;
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <main className="login">
      <div className="card">
        <Brand gateway={gateway} />
        {!token ? (
          <ErrorText>This link has no registration token. Run sudo entrypoint panel register for a new one.</ErrorText>
        ) : !passkeysSupported() ? (
          <ErrorText>This browser does not support passkeys.</ErrorText>
        ) : (
          <>
            <p>
              Create a passkey for this panel. Save it in your password manager (e.g. Proton Pass, 1Password, iCloud Keychain) or on a security key
              such as a YubiKey. It is the only way to sign in.
            </p>
            <ErrorText>{error}</ErrorText>
            <button
              className="primary"
              disabled={busy}
              autoFocus
              onClick={async () => {
                setBusy(true);
                setError("");
                try {
                  onDone(await registerPasskey(token));
                } catch (err) {
                  setError(
                    err instanceof DOMException && err.name === "NotAllowedError"
                      ? "Cancelled or timed out. The link worked once; run sudo entrypoint panel register again."
                      : (err as Error).message,
                  );
                } finally {
                  setBusy(false);
                }
              }}
            >
              {busy ? "Waiting for your passkey…" : "Create passkey"}
            </button>
          </>
        )}
      </div>
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
  if (path === "/register")
    return (
      <Register
        gateway={s.gateway}
        onDone={(x) => {
          setS(x);
          go("/");
        }}
      />
    );
  if (!s.authenticated) return <Login s={s} onDone={setS} />;
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
