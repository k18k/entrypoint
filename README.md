# Entrypoint

Samodzielnie hostowana brama do publikowania usług. Działa na serwerze z publicznym IP, przyjmuje ruch HTTP(S) z automatycznym TLS i kieruje go:

- do usług osiągalnych bezpośrednio albo przez VPN (`IP:port`);
- przez tunele do aplikacji na maszynach bez publicznego IP (za NAT-em), z wieloma replikami na serwis.

Ścieżkę danych obsługują Caddy (HTTP, TLS, ACME) i frp (tunele) w oficjalnych, przypiętych wersjach (`versions.json`). Własny kod to warstwa sterowania (`entrypointd`), CLI `entrypoint`, konektor tunelu i SDK dla Node.js.

Wersja **0.1**, do własnego użytku i sterowana z CLI. Specyfikacja: [docs/specyfikacja.md](docs/specyfikacja.md). Wyniki eksperymentów prototypu: [docs/wyniki.md](docs/wyniki.md).

## Jak to działa

```
 internet ──443/80──▶ Caddy ──▶ IP:port (np. przez VPN)
                        │
                        └──▶ 127.0.0.1:<port serwisu> ── frps ══ tunel TLS :55555 ══ frpc ── aplikacja
                                                          │ (grupa replik)
                                          plugin autoryzacji: entrypointd
```

- Każdy **serwis** dostaje stały port z puli 20000–29999 (tylko 127.0.0.1). Repliki serwisu tworzą grupę frp na tym porcie, a frps rozkłada między nie **połączenia TCP**.
- **Token** serwisu pozwala wyłącznie podłączać repliki tego serwisu. Uprawnienia sprawdza brama, nigdy klient.
- Klient tunelu **zawsze weryfikuje** certyfikat bramy z prywatnego CA zapisanego w pakiecie połączenia.
- Caddy i frps startują z ostatniej zastosowanej konfiguracji na dysku, więc ruch działa także bez entrypointd.

## Wymagania

**Serwer bramy:**

- Linux z systemd (Debian 12/13, Ubuntu 24.04), x86_64 albo arm64;
- publiczny IPv4 i otwarte porty TCP **80**, **443** i **55555** (u dostawcy i w firewallu systemu);
- domena z możliwością ustawienia rekordów A.

**Rozwój i budowanie ze źródeł:** Go 1.27, Node.js 22+, Python 3, Docker (do testów e2e). Paczki: `make dist sdk`, pełne wydanie: `make release`.

**Aplikacje:** Node.js 22+ dla SDK; dowolny język przez CLI konektora.

## Instalacja na serwerze

1. Ustaw rekord DNS bramy, np. `A tunnel.example.com -> <IP serwera>`. Nazwa bramy jest stała: klienci tuneli łączą się po nazwie, nie po IP.
2. Zainstaluj programy z [wydania na GitHubie](https://github.com/k18k/entrypoint/releases). `install.sh` pobiera paczkę i sprawdza jej SHA256:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/k18k/entrypoint/master/scripts/install.sh | sudo sh
   ```

   Inna wersja: `ENTRYPOINT_VERSION=0.1.0`; paczka z dysku: `ENTRYPOINT_TARBALL=./entrypoint_0.1.0_linux_amd64.tar.gz`.
3. Załóż bramę:

   ```bash
   sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com
   sudo entrypoint status
   ```

Instalator:

- tworzy użytkowników systemowych `entrypoint`, `entrypoint-caddy` i `entrypoint-frps`;
- tworzy prywatne CA tunelu i certyfikat portu 55555;
- zakłada usługi systemd spięte w `entrypoint.target`.

Ponowne `entrypoint install` jest bezpieczne: aktualizuje usługi i niczego nie usuwa.

**Najpierw ćwicz na stagingu Let's Encrypt.** Trasa z `--acme-staging` dostaje certyfikat testowy i nie zużywa limitu 50 certyfikatów na domenę na tydzień.

| Ścieżka | Zawartość |
| --- | --- |
| `/opt/entrypoint/releases/<wersja>`, `/opt/entrypoint/current` | Programy |
| `/etc/entrypoint/entrypoint.json` | Konfiguracja startowa (bez sekretów) |
| `/var/lib/entrypoint` | Baza SQLite (rewizje, dziennik odwołań), prywatne CA, magazyn Caddy |

## Publikowanie usług

Wszystkie polecenia administracyjne wymagają `sudo`. Każda zmiana konfiguracji tworzy numerowaną rewizję i wypisuje wynik jej zastosowania.

### Usługa osiągalna po IP (np. przez VPN)

```bash
sudo entrypoint route add app.example.com --to 10.8.0.2:3000
sudo entrypoint route add app.example.com/api --to 10.8.0.3:8080 --strip-prefix
sudo entrypoint dns check          # jakie rekordy DNS są potrzebne i czy już są
```

WebSockety, SSE i HTTP/2 działają bez dodatkowej konfiguracji.

### Aplikacja przez tunel

Na bramie:

```bash
sudo entrypoint service add api --health /health
sudo entrypoint route add api.example.com --service api
sudo entrypoint token create --service api      # wypisuje ENTRYPOINT_CONNECT tylko raz
```

`ENTRYPOINT_CONNECT` zawiera nazwę bramy, port, zaufane CA i token. Przechowuj go jak hasło. Każda replika (albo każda maszyna) może mieć osobny token, który odwołasz niezależnie.

W aplikacji zainstaluj SDK z gita. Przy instalacji SDK pobiera konektor i frpc dla Twojej platformy z wydania i sprawdza ich SHA256:

```bash
npm install github:k18k/entrypoint-ts#v0.1.1
```

Nowsze npm (11.x) domyślnie nie uruchamia skryptów instalacyjnych zależności i pokazuje `npm warn install-scripts`. Wtedy SDK pobierze konektor przy pierwszym `expose()`/`tunnel()`. Żeby pobierał się już przy instalacji: `npm install-scripts approve @k18k/entrypoint`.

SDK jest też w GitHub Packages jako `@k18k/entrypoint` (`@k18k:registry=https://npm.pkg.github.com` w `.npmrc`; GitHub wymaga tokenu `read:packages` także dla pakietów publicznych).

**Adapter `node:http` (zalecany).** Daje pełny drain: `Connection: close`, zamknięcie SSE i WebSocketów kodem 1001. Przyjmuje dowolny handler `(req, res)`, więc działa z czystym `node:http` i z aplikacjami frameworków (aplikacja Express, `app.callback()` w Koa).

```js
import { expose } from "@k18k/entrypoint/http";

const handler = (req, res) => {
  if (req.url === "/health") return res.end("ok");
  res.end("Cześć z tunelu");
};

// Serwer na 127.0.0.1:0 i tunel; SIGTERM uruchamia drain i kończy proces.
const conn = await expose(handler, { service: "api", healthCheck: "/health" });
// WebSockety: new WebSocketServer({ server: conn.server }); przekaż je w opcji websockets.
```

**Kontrakt bazowy.** Tunel do adresu, na którym aplikacja już słucha:

```js
import { tunnel } from "@k18k/entrypoint";

const t = await tunnel({ service: "api", upstream: "http://127.0.0.1:3000", healthCheck: "/health" });
process.on("SIGTERM", () => t.drain({ timeoutMs: 30_000 }).then(() => process.exit(0)));
```

**Dowolny język (CLI):**

```bash
ENTRYPOINT_CONNECT=... npx entrypoint-tunnel tunnel --upstream http://127.0.0.1:3000 --health /health
```

Uruchom aplikację na dwóch maszynach z tokenami tego samego serwisu, a brama rozłoży ruch między repliki. Zatrzymanie jednej (SIGTERM, drain) nie przerywa serwisu.

**Aplikacje w Dockerze (zamiast `cloudflared`).** Konektor działa jako osobny kontener obok aplikacji, w kontrakcie bazowym. Obraz `ghcr.io/k18k/entrypoint-connector` (amd64 i arm64) albo lokalnie `make connector-image`:

```yaml
services:
  app:
    image: moja-aplikacja
  tunnel:
    image: ghcr.io/k18k/entrypoint-connector:0.1.1
    command: ["--upstream", "http://app:3000", "--health", "/health", "--drain-timeout", "20s"]
    environment:
      ENTRYPOINT_CONNECT: ${ENTRYPOINT_CONNECT}   # z pliku .env poza repozytorium
    stop_grace_period: 30s                        # dłuższy niż drain-timeout
    restart: unless-stopped
```

`docker compose stop` wysyła SIGTERM, a konektor wycofuje replikę z grupy i kończy się po `drain-timeout`.

### Drain i repliki: co warto wiedzieć

- Repliki dzielą ruch **po połączeniach TCP**, nie po żądaniach. Nowa replika dostaje ruch na nowych połączeniach. Przy stałym ruchu stare połączenia keep-alive (bezczynność 4 s) zostają przy dotychczasowej replice.
- Replika z nieudanym health checkiem wypada z grupy. Konektor restartuje wtedy frpc, żeby zerwać przypięte do niej połączenia, i replika wraca po udanym sprawdzeniu.
- Kontrakt bazowy nie kontroluje połączeń aplikacji: przy drainie czeka do `drainTimeout` (domyślnie 30 s). Adapter `node:http` kończy drain, gdy połączenia się zamkną.
- Restart lub aktualizacja Caddy zrywa wszystkie połączenia, a restart frps wszystkie tunele. Klienci WebSocket i SSE muszą umieć połączyć się ponownie.

## Tokeny i odwołania

```bash
sudo entrypoint token list
sudo entrypoint token revoke <id>        # nowe logowania odrzucane od razu, sesja zrywana przy heartbeat (do ~10 s)
sudo entrypoint token revoke <id> --now  # natychmiast: restart frps, krótka przerwa wszystkich tuneli
```

Odwołania trafiają do osobnego dziennika tylko do dopisywania. Nie cofa ich rollback rewizji ani restore.

## Operacje

| Polecenie | Co robi |
| --- | --- |
| `entrypoint status` | Usługi, rewizja, serwisy z replikami, licznik certyfikatów, ostrzeżenia |
| `entrypoint service add/rm/list` | Serwisy tuneli; port usuniętego serwisu zostaje zarezerwowany na zawsze |
| `entrypoint route add/list/rm` | Trasy `host[/prefiks]` do `--to IP:port` albo `--service`; opcje `--strip-prefix`, `--max-body 50MB`, `--header-timeout 60s`, `--acme-staging` |
| `entrypoint token create/list/revoke` | Tokeny serwisów |
| `entrypoint dns check` | Wymagane rekordy DNS i ich bieżący stan (brama nie zmienia DNS) |
| `entrypoint ca status`, `ca rotate start\|switch\|finish` | Prywatne CA tunelu i jego rotacja w trzech krokach |
| `entrypoint revision list`, `revision show <n>` | Historia zmian i błędy stosowania |
| `entrypoint backup -o plik.age` | Zaszyfrowany hasłem backup (baza, CA, certyfikaty, konfiguracja) |
| `entrypoint restore plik.age` | Odtworzenie na zainstalowanej bramie tej samej wersji |
| `entrypoint logs entrypointd\|caddy\|frps [-f]` | Logi z journald |
| `entrypoint uninstall [--purge]` | Usuwa usługi i programy; `--purge` także dane |

Polecenia z wyjściem dla skryptów mają `--json`. Zmiany przyjmują `--base-rev <n>`: zmiana jest odrzucana, jeśli w międzyczasie powstała nowsza rewizja.

**Backup i restore.** Backup rób regularnie i trzymaj poza serwerem, osobno od hasła (`ENTRYPOINT_BACKUP_PASSWORD` albo `--password-file` dla automatyzacji). Odtworzenie na nowej maszynie:

```bash
sudo ENTRYPOINT_TARBALL=... sh install.sh
sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com
sudo entrypoint restore entrypoint-backup.age
```

Restore **unieważnia wszystkie odtworzone tokeny**, bo backup nie zna późniejszych odwołań. Wydaj nowe tokeny i zaktualizuj DNS, jeśli zmienił się adres IP. Klienci nie zmieniają niczego poza tokenem: CA i nazwa bramy zostają.

**Aktualizacja w v0.1 jest ręczna:** backup, `install.sh` z nową wersją (`ENTRYPOINT_VERSION=...`), `entrypoint install`.

**Rotacja CA tunelu:**

1. `ca rotate start`: nowe tokeny mają oba CA;
2. zaktualizuj klientów (nowe tokeny albo `entrypoint-connector rebundle --ca`);
3. `ca rotate switch`: serwer przechodzi na nowe CA, a `ca status` pokazuje klientów ze starym pakietem;
4. `ca rotate finish`.

## Bezpieczeństwo

- Admin API Caddy działa tylko na sockecie unixowym. API frps, plugin i porty serwisów słuchają tylko na 127.0.0.1. Z sieci osiągalne są wyłącznie porty 80, 443 i 55555 (zestaw e2e 1).
- Tokeny to 256 losowych bitów; w bazie zapisany jest tylko hash. Tokeny i pakiety połączenia nie trafiają do logów ani komunikatów błędów (zestaw e2e 12). Konektor przekazuje token frpc wyłącznie przez środowisko procesu.
- Brama ignoruje od klienta nagłówki adresu (`X-Forwarded-*`, `X-Real-IP`, `Forwarded`) i sama ustawia `X-Forwarded-For/Proto/Host`.
- Plugin frps dopuszcza tylko proxy `tcp`, a grupę, klucz grupy i port zawsze nadpisuje wartościami serwisu z tokenu. Pozostałe pola są sprawdzane z białą listą.
- Certyfikaty w locie dla tras wildcard (`*.example.com`): tylko dla hostów z trasą i najwyżej 10 nowych subdomen na godzinę, żeby nie wyczerpać limitu Let's Encrypt.
- Posiadacz tokenu jest zaufanym operatorem swojego serwisu: zmodyfikowany klient może wyłączyć health check, ale szkodzi tylko własnemu serwisowi.

## Rozwój SDK

`sdk/node`: TypeScript 7 z `erasableSyntaxOnly` i `isolatedDeclarations`, build `tsdown`, lint `oxlint`, formatowanie `oxfmt`:

```bash
cd sdk/node
npm install
npm run check      # typecheck (tsc --noEmit), oxlint, oxfmt --check
npm run build      # tsdown -> lib/
```

## Testy

```bash
make test                     # testy jednostkowe
make e2e                      # testy e2e naszego kodu w Dockerze (~4 min, 4 równoległe kopie środowiska)
make e2e-experiments          # eksperymenty: zachowanie Caddy i frp (~3 min; po zmianie versions.json)
make e2e SUITE=Routing        # jeden zestaw: Smoke Install TLS Routing Tunnels Replicas State Revoke Ops SDK Secrets
make e2e RUN='^TestOps_DNSCheck$' SHARDS=1 KEEP=1   # jeden test, środowisko zostaje
```

`make e2e` sprawdza nasz kod i naszą konfigurację: instalator, rewizje, routing, plugin i uprawnienia, drain, SDK, backup, ochronę sekretów. Nie wymaga VPS-a. Brama jest instalowana prawdziwym `install.sh` na czystym kontenerze z systemd. Pebble zastępuje Let's Encrypt, a CoreDNS publiczny DNS.

`make e2e-experiments` powtarza eksperymenty ze specyfikacji, które sprawdzają same Caddy i frp: odnawianie certyfikatów, atomowość `/load`, grupy i heartbeat frp, odporność na straty pakietów. Wyniki są w [docs/wyniki.md](docs/wyniki.md).

Każda kopia środowiska ma własny projekt compose i podsieci `10.77.N.0/24` i `10.78.N.0/24`. Przydział testów do kopii opiera się na czasach z poprzedniego przebiegu (`e2e/durations.json`, aktualizowanym automatycznie). Artefakty każdego testu (logi, konfiguracje Caddy i frps, stan bazy) trafiają do `e2e/artifacts/`.

## Czego nie ma w v0.1

- Panel webowy, adaptery Fastify i Hono, automatyczna aktualizacja z rollbackiem i tryb serwisowy (v0.2.0).
- Pełne testy awarii, testy na ARM64, benchmark wydajności (v0.2.0).
- Limity połączeń na adres IP po stronie publicznej (decyzja przed v0.2.0).
- Statyczne wydania stron (v0.3), przekierowania TCP/UDP i TURN (v0.4).
- Certyfikaty wildcard przez DNS-01, OIDC, HA, wbudowany VPN (v1).
- Publikacja w rejestrze npmjs.com: SDK jest dostępne z gita i z GitHub Packages.
- Konektor dla Linux arm64, macOS i Windows jest w wydaniu, ale nie ma testów e2e.

## Licencja

[Apache-2.0](LICENSE). Caddy i frp, używane jako oficjalne binarki, mają własne licencje (obie Apache-2.0).
