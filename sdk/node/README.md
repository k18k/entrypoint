# @k18k/entrypoint

SDK Node.js dla [Entrypoint](https://github.com/k18k/entrypoint): wystawia lokalną aplikację przez tunel do bramy Entrypoint, z wieloma replikami i drainem.

## Instalacja

Z gita (bez rejestru):

```bash
npm install github:k18k/entrypoint-ts#v0.1.0
```

Z GitHub Packages:

```bash
echo "@k18k:registry=https://npm.pkg.github.com" >> .npmrc
npm install @k18k/entrypoint
```

GitHub Packages wymaga zalogowania tokenem z uprawnieniem `read:packages`, także dla pakietów publicznych.

Przy instalacji SDK pobiera z [wydania Entrypoint](https://github.com/k18k/entrypoint/releases) konektor i oficjalne frpc dla bieżącej platformy (Linux x64/arm64, macOS x64/arm64, Windows x64) i sprawdza ich sumy SHA256. Bez dostępu do sieci przy instalacji (albo z `--ignore-scripts`) pobranie nastąpi przy pierwszym `tunnel()`, albo ręcznie: `npx entrypoint-tunnel install-connector`. Własną binarkę konektora wskazuje `ENTRYPOINT_CONNECTOR`.

## Użycie

Pakiet połączenia `ENTRYPOINT_CONNECT` wydaje brama: `sudo entrypoint token create --service api`.

Adapter `node:http`: pełny drain (Connection: close, zamknięcie SSE i WebSocketów kodem 1001). Działa z każdym handlerem `(req, res)`, także z aplikacją Express.

```js
import { connectTo } from "@k18k/entrypoint/http";

const conn = await connectTo((req, res) => res.end("ok"), {
  service: "api",
  healthCheck: "/health",
});
```

Kontrakt bazowy: tunel do adresu, na którym aplikacja już słucha.

```js
import { tunnel } from "@k18k/entrypoint";

const t = await tunnel({
  service: "api",
  upstream: "http://127.0.0.1:3000",
  healthCheck: "/health",
});
process.on("SIGTERM", () => t.drain({ timeoutMs: 30_000 }).then(() => process.exit(0)));
```

CLI dla dowolnego języka:

```bash
ENTRYPOINT_CONNECT=... npx entrypoint-tunnel tunnel --upstream http://127.0.0.1:3000 --health /health
```

Wymagania: Node.js 22+.
