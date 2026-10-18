# Wyniki eksperymentów prototypu (sekcja 17)

Data: 2026-10-18. Zakres: eksperymenty 1–3 w części v0.1. Testy 3.4 i 3.5 należą do v0.2.0.

## Środowisko i wersje

| Składnik | Wersja |
| --- | --- |
| Caddy | 2.11.7 (oficjalna binarka, `versions.json`) |
| frp (frps, frpc) | 0.71.1 (oficjalna binarka, `versions.json`) |
| Pebble | 2.10.1 |
| CoreDNS | 1.14.7 |
| Go / Node.js | 1.27.2 / 24.22.0 |
| System bramy | Debian trixie z systemd w kontenerze (`--privileged`, cgroup prywatna) |

Uwaga: frp 0.71.1 ukazało się 4 dni przed testami. Przed v0.2.0 warto powtórzyć testy na nowszym wydaniu poprawkowym.

Testy uruchamia `make e2e SUITE=Replicas|Tunnels|Revoke` (pliki `e2e/tests/replicas_test.go`, `tunnels_test.go`, `revoke_test.go`). Pomiary pochodzą z linii `WYNIK:` w logu testów. Artefakty każdego testu zapisuje `e2e/artifacts/<test>/`.

Parametry istotne dla wyników:

- Caddy: keep-alive do upstreamu 4 s, połączenie 5 s, `stream_close_delay` 1 h;
- frps: `heartbeatTimeout` 40 s;
- konektor: heartbeat co 10 s, health check co 1 s, 2 próby (w testach).

Limity w kryteriach nie zostały zmienione.

## Decyzja ogólna

**Wariant B (grupy frp na stałych portach serwisów) jest przyjęty.** Wszystkie testy eksperymentów 1–3 z zakresu v0.1 spełniają kryteria. Wariant SRV nie jest rozwijany.

Do architektury doszły trzy uzupełnienia w ramach tych samych czterech procesów:

1. **Odwołanie sesji przez odrzucanie Ping** (test 3.1). Plugin obsługuje operacje Login, NewProxy i Ping. Restart frps zostaje jako opcja `entrypoint token revoke --now`.
2. **Gniazdo pluginu trzymane przez systemd** (`entrypointd.socket`, testy 1.6 i 3.1b). Awaria i restart entrypointd nie zrywają sesji, bo zapytania frps czekają w kolejce gniazda. Sesje zależą od entrypointd dopiero przy jego pełnej niedostępności. Tabela awarii w sekcji 3 specyfikacji została zaktualizowana.
3. **Restart frpc po nieudanym health checku** (test 1.5). Zrywa połączenia keep-alive przypięte do chorej repliki.

## Eksperyment 1: grupy frp i drain

### 1.1 Nowe żądanie na istniejącym keep-alive po wycofaniu repliki

- **Oczekiwane:**
  - z adapterem brak nowych żądań do wycofanej repliki po 5 s;
  - w kontrakcie bazowym brak nowych żądań po `drainTimeout`.
- **Zaobserwowane:** dwie repliki, 4 klienty z keep-alive, ruch co 20 ms.
  - Adapter: w oknie 5–15 s po wycofaniu replika dostała 0 żądań. Klienci nie dostali żadnego błędu (0 z 3276).
  - Kontrakt bazowy (`drainTimeout` 10 s): ostatnie żądanie do wycofanej repliki 9,7 s po wycofaniu, frpc zakończony po 9,8 s, potem 0 żądań i 0 błędów.
- **Ograniczenia:** w kontrakcie bazowym wycofana replika obsługuje żądania na przypiętych połączeniach aż do końca `drainTimeout`, jeśli ruch na tych połączeniach nie ustaje.
- **Decyzja:** zaliczone. Opis gwarancji w sekcji 7 zgadza się z pomiarem.

### 1.2a Łagodny drain w trakcie uploadu

- **Oczekiwane:** odpowiedź 2xx i kompletne dane w aplikacji.
- **Zaobserwowane:** upload 8 MB z prędkością 1 MB/s; drain zaczął się po 2 s. Wynik: kod 200 po 8,0 s, zgodny SHA-256, `uploadsOK=1`.
- **Ograniczenia:** brak.
- **Decyzja:** zaliczone.

### 1.2b Natychmiastowe odwołanie tokenu w trakcie uploadu

- **Oczekiwane:** upload przerwany, klient dostaje błąd, aplikacja nie traktuje częściowych danych jako sukcesu.
- **Zaobserwowane:**
  - upload przerwany 6,2 s po odwołaniu, przy najbliższym heartbeat konektora;
  - klient dostał 502;
  - aplikacja zapisała przerwany upload (`uploadsAborted=1`, `uploadsOK=0`);
  - konektor zakończył się zdarzeniem `revoked` i nie łączy się ponownie.
- **Ograniczenia:** odwołanie przez Ping działa z opóźnieniem do jednego interwału heartbeat (10 s). Natychmiastowe przerwanie daje `--now` (test 3.1).
- **Decyzja:** zaliczone.

### 1.3 SSE i WebSocket przez wycofywaną replikę

- **Oczekiwane:**
  - adapter zamyka strumienie kodem 1001 przed `drainTimeout`;
  - w kontrakcie bazowym strumienie są zrywane po `drainTimeout`.
- **Zaobserwowane:**
  - Adapter: WebSocket zamknięty 32 ms po rozpoczęciu drainu kodem 1001 („server draining”). SSE zakończone poprawnie (EOF) po 31 ms.
  - Kontrakt bazowy (10 s): oba strumienie zerwane po 10,0 s, WebSocket bez ramki zamknięcia.
- **Ograniczenia:** adapter rozpoznaje SSE po `Content-Type: text/event-stream`. WebSockety obsługiwane przez bibliotekę `ws` zamyka przez przekazane serwery. Inne upgrade'y dostają surową ramkę 1001.
- **Decyzja:** zaliczone.

### 1.4 Wycofanie, wygaszanie i koniec frpc jako osobne kroki

- **Oczekiwane:** każdy krok obserwowalny i wywoływany osobno.
- **Zaobserwowane:** `drainTimeout` 8 s.
  - Krok 1: replika znika ze statusu po 0,1 s (zdarzenia `draining` i `withdrawn`), a frpc działa dalej.
  - Krok 2: żądanie trwające 4 s, rozpoczęte przed drainem, kończy się kodem 200 po 3,5 s.
  - Krok 3: frpc kończy się po 8,1 s.
- **Ograniczenia:** konektor wycofuje proxy przez przeładowanie konfiguracji frpc (`/api/reload` bez proxy). frps zamyka wtedy tylko nasłuch i istniejące połączenia zostawia, co potwierdza kod frps (`BaseProxy.Close`).
- **Decyzja:** zaliczone.

### 1.5 Jedna replika zwraca błędy, druga jest zdrowa

- **Oczekiwane:** serwis odpowiada przez zdrową replikę. Chora znika z grupy w czasie interwał × liczba prób + 5 s (tu 7 s).
- **Zaobserwowane:**
  - Pierwsza wersja: chora replika zniknęła z grupy po 1,4 s, ale **błędy trwały dalej**: 394 z 796 żądań po upływie limitu. Połączenia keep-alive Caddy przypięte do chorej repliki przy stałym ruchu nigdy nie stają się bezczynne, a frps zamyka tylko nasłuch.
  - Po zmianie konektora (restart frpc po `check failed`): replika usunięta po 1,5 s, ostatni błąd 1,2 s po awarii, po limicie 0 błędów na 796 żądań.
- **Ograniczenia:** restart frpc przerywa także żądania w toku na chorej replice. Replika wraca do grupy dopiero po udanym health checku. Health check frpc to mechanizm zaufanego klienta (sekcja 5).
- **Decyzja:** zaliczone po zmianie. Restart frpc po nieudanym health checku jest częścią konektora.

### 1.6 Awaria entrypointd przy ruchu

- **Oczekiwane:** nowe żądania są obsługiwane; nowe logowania odrzucane; po restarcie logowania wracają w 30 s.
- **Zaobserwowane:**
  - Pierwsza wersja (plugin Ping, gniazdo otwierane przez demona): przy zatrzymanym entrypointd 428 z 774 żądań zakończyło się błędem. frpc zamyka sesję po odpowiedzi Pong z błędem, a frps zamienia każdy błąd pluginu na taki Pong.
  - Po zmianie (gniazdo pluginu w `entrypointd.socket`, aktywacja przez systemd): 3× `kill -9 entrypointd` w odstępach 3 s przy ruchu, 0 błędów na 816 żądań. Nowa replika uruchomiona w trakcie awarii zalogowała się po 9,5 s od pierwszej awarii. systemd restartuje demona (inny PID).
- **Ograniczenia:**
  - W czasie awarii nowe logowania **czekają** na restart demona, zamiast być odrzucane. frps wywołuje plugin bez timeoutu, a połączenie czeka w kolejce gniazda.
  - Pełna niedostępność (zatrzymane także gniazdo) opisuje test 3.1b.
- **Decyzja:** zaliczone po zmianie. Zachowanie przy awarii jest lepsze od zakładanego; tabela w sekcji 3 została zaktualizowana.

### 1.7 Zimny start Caddy przy niedziałającym entrypointd

- **Oczekiwane:** Caddy startuje z pliku i obsługuje ruch.
- **Zaobserwowane:**
  - Test wykrył błąd: plik startowy zapisany przez entrypointd miał grupę `entrypoint` zamiast `entrypoint-caddy`, więc Caddy nie mógł go przeczytać. Przyczyna: `os.Chmod(0o2750)` w Go nie ustawia bitu setgid; potrzebny jest `os.ModeSetgid`.
  - Po poprawce Caddy obsługuje ruch 1,0 s po restarcie.
- **Ograniczenia:** brak.
- **Decyzja:** zaliczone po poprawce.

### 1.8 Usunięcie ostatniej repliki

- **Oczekiwane:** odpowiedź 502 lub 503 w czasie do 1 s; żadne połączenie do usuniętej repliki nie trwa dłużej niż 5 s.
- **Zaobserwowane:**
  - `kill -9` procesu Node ostatniej repliki;
  - pierwsza odpowiedź 502 po 0,11 s, najdłuższa przerwa między odpowiedziami 53 ms;
  - konektor (koniec rury stdin) i frpc zakończone.
- **Ograniczenia:** brak.
- **Decyzja:** zaliczone.

### 1.9 Restart frps

- **Oczekiwane:** repliki wracają do grup na tych samych portach w 30 s.
- **Zaobserwowane:** obie repliki wróciły po 2,9 s, port serwisu bez zmian.
- **Ograniczenia:** restart frps zrywa wszystkie tunele na kilka sekund.
- **Decyzja:** zaliczone.

### 1.10 Nowa replika z health checkiem

- **Oczekiwane:** żadne połączenie nie trafia do niej przed pierwszym udanym sprawdzeniem.
- **Zaobserwowane:**
  - Przez 8 s z niezdanym health checkiem replika dostała 0 żądań i nie pojawiła się w statusie: frpc wysyła NewProxy dopiero po udanym sprawdzeniu.
  - Po wyzdrowieniu, przy stałym ruchu 4 połączeń, nadal 0 żądań. Po zwiększeniu ruchu do 24 połączeń replika dostała 3989 żądań.
- **Ograniczenia:**
  - **Nowa replika dostaje ruch tylko na nowych połączeniach.** Przy stałym ruchu Caddy używa dalej istniejących połączeń keep-alive do starej repliki: rozkład jest po połączeniach, a nie po żądaniach (sekcja 7).
  - Ruch wyrównuje się, gdy połączenia stają się bezczynne (4 s) albo rośnie współbieżność.
- **Decyzja:** zaliczone. Ograniczenie trafia do README.

## Eksperyment 2: tożsamość i cykl życia replik

### 2.1 Zmodyfikowany frpc podaje cudzą grupę, port lub serwis

- **Oczekiwane:** odrzucony przez plugin i zalogowany, bez wpływu na inne serwisy.
- **Zaobserwowane:** frpc bez konektora z tokenem serwisu `other` zgłosił trzy proxy.
  - Proxy `api.<token>.x` z nazwą cudzego serwisu: odrzucone.
  - Proxy z grupą `svc-api` i portem serwisu `api`: plugin nadpisał grupę na `svc-other` i port na port serwisu `other`.
  - Proxy `http` z `customDomains`: odrzucone.
  - Do atakującego trafiło 0 żądań, serwis api miał 0 błędów, a odrzucenia są w logu entrypointd.
- **Ograniczenia:** brak.
- **Decyzja:** zaliczone.

### 2.2 Pola NewProxy spoza białej listy

- **Oczekiwane:** odrzucone albo nadpisane przez plugin.
- **Zaobserwowane:** bezpośrednie wywołania pluginu.
  - Odrzucone: `custom_domains`, `subdomain`, `locations`, `http_user`, `sk`, `multiplexer`, typy `http`, `udp`, `stcp`, nazwa innego serwisu, nazwa bez repliki.
  - Nadpisane: `group`, `group_key`, `remote_port`.
  - Login odrzucony dla tokenu usuniętego serwisu, zniekształconego tokenu i poprawnego tokenu z polem `user`.
- **Ograniczenia:** biała lista dotyczy pól NewProxy wersji frp 0.71.1. Nowe pola w kolejnych wersjach frp są odrzucane, dopóki nie trafią na listę.
- **Decyzja:** zaliczone.

### 2.3 Usunięcie serwisu i utworzenie nowego

- **Oczekiwane:** port usuniętego serwisu nigdy nie trafia do nowego.
- **Zaobserwowane:**
  - `x`=20000; po usunięciu `y`=20001; nowy `x`=20002;
  - wszystkie rezerwacje zostają w bazie, a próba `DELETE FROM ports` jest blokowana triggerem;
  - token starego `x` jest odrzucany dla nowego `x`, bo token jest związany z identyfikatorem serwisu, a nie z nazwą.
- **Ograniczenia:** pula 10 000 portów; usunięte serwisy zużywają ją trwale.
- **Decyzja:** zaliczone.

### 2.4 Restarty entrypointd i frps w różnej kolejności

- **Oczekiwane:** status w entrypointd zgodny ze stanem frps, bez rozjazdu.
- **Zaobserwowane:** status był zgodny z API frps w pięciu sekwencjach:
  - restart frps (zgodność po 2,9 s);
  - restart entrypointd (0,3 s);
  - frps, potem entrypointd (0,9 s);
  - entrypointd, potem frps (1,3 s);
  - `kill -9` obu (3,6 s).
- **Ograniczenia:** entrypointd nie trzyma stanu replik; status zawsze czyta go z frps.
- **Decyzja:** zaliczone.

### 2.5 Rotacja CA

- **Oczekiwane:** klient z pakietem dwóch CA łączy się przed przełączeniem i po nim; klient tylko ze starym CA jest odrzucany po przełączeniu.
- **Zaobserwowane:**
  - po `ca rotate start` nowy pakiet ma 2 CA;
  - `ca status` wskazuje klienta logującego się ze starym pakietem;
  - po `ca rotate switch` frps restartuje się z nowym certyfikatem (jednostka `.path`);
  - klient z dwoma CA wraca po 3,0 s, a klient ze starym CA nie łączy się ponownie;
  - po `ca rotate finish` pakiet ma 1 CA.
- **Ograniczenia:**
  - Przełączenie restartuje frps (krótka przerwa wszystkich tuneli). Odnowienie certyfikatu portu tunelu raz w roku też restartuje frps.
  - frpc zgłasza odrzucenie TLS tylko jako „session shutdown”, dlatego konektor sam sprawdza certyfikat i emituje `tls_error` z przyczyną.
  - Istniejącym klientom nowe CA dostarcza nowy token albo `entrypoint-connector rebundle`.
- **Decyzja:** zaliczone.

## Eksperyment 3: trwałe odwołanie tokenów

### 3.1 Odwołanie tokenu klienta A (bez konektora), klient B na innym tokenie

- **Oczekiwane:** A odłączony i nie łączy się ponownie; B bez przerwy. Przy rezerwowym restarcie frps B wraca w 10 s.
- **Zaobserwowane:**
  - **Ping, frpc A z heartbeat:** sesja A zerwana po 7,0 s, nie łączy się ponownie przez 30 s, a logowania są odrzucane z `ep:revoked`. Ruch B: 0 błędów na 1406 żądań, brak rozłączeń.
  - **Ping, frpc A bez heartbeat (klient niewspółpracujący):** sesja A zerwana po 37,3 s, gdy frps nie dostał Ping przez `heartbeatTimeout`. Ruch B: 0 błędów na 2560 żądań.
  - **`--now` (restart frps):** sesja A zerwana po 0,16 s; B wraca po 2,5 s (96 błędów z 1150).
- **Ograniczenia:**
  - Odwołanie przez Ping zrywa sesję w czasie do jednego interwału heartbeat klienta (10 s w konektorze). Klient, który nie wysyła Ping albo ignoruje błąd, jest zrywany najpóźniej po `heartbeatTimeout` (40 s).
  - Nowe logowania i rejestracje są odrzucane od razu.
- **Decyzja:** **wybrano odrzucanie Ping.** Restart frps zostaje jako opcja `--now` do natychmiastowego zerwania kosztem krótkiej przerwy pozostałych tuneli. Ping przy każdym heartbeat sprawdza sesję wobec bieżącego snapshotu, więc obejmuje też rollback rewizji i usunięcie serwisu.

### 3.1b Powtórzenie testu 1.6 przy odrzucaniu Ping

- **Oczekiwane:** zmierzony i zaakceptowany wpływ awarii entrypointd na aktywne sesje; aktualizacja tabeli awarii w sekcji 3.
- **Zaobserwowane:**
  - Awaria procesu: brak wpływu (test 1.6). Gniazdo trzyma systemd, a zapytania frps czekają w kolejce.
  - Pełna niedostępność na 45 s (zatrzymane `entrypointd.socket` i `entrypointd.service`): sesja zerwana po 6,1 s, przy najbliższym Ping; od tej chwili ruch przez tunele kończy się błędem. Po starcie replika jest zdrowa po 18,2 s (backoff frpc do 20 s).
- **Ograniczenia:** pełne zatrzymanie entrypointd (także gniazda) albo stała awaria startu wyłącza tunele po najbliższym heartbeat. Nie należy zatrzymywać `entrypointd.socket` na działającej bramie.
- **Decyzja:** wpływ zmierzony; tabela w sekcji 3 zaktualizowana. **Do Twojej akceptacji.**

### 3.2 Odwołanie w trakcie częściowego stosowania rewizji, potem rollback

- **Oczekiwane:** odwołanie trwa; sesje otwarte w międzyczasie są sprawdzone ponownie.
- **Zaobserwowane:** rewizja 6 zatrzymana w trybie testowym po zapisie snapshotu (15 s). W tym czasie:
  - odwołano token A;
  - otwarto nową sesję C;
  - zatrzymano Caddy.

  Wynik:
  - rewizja 6 ma status `failed` z błędem admin API Caddy;
  - snapshot wrócił do rewizji 5;
  - token A pozostał odwołany, a jego sesja została zerwana;
  - sesja C przeszła ponowne sprawdzenie (Ping wobec przywróconego snapshotu) i działa;
  - w grupie została tylko replika C.
- **Ograniczenia:** CLI przyjmuje wydanie tokenu dopiero po zastosowaniu rewizji, więc sesja z tokenem istniejącym tylko w rewizji N nie może powstać. Ponowne sprawdzenie dotyczy wszystkich sesji przy każdym Ping.
- **Decyzja:** zaliczone.

### 3.3 Backup przed odwołaniem, utrata maszyny, restore

- **Oczekiwane:** wszystkie odtworzone tokeny unieważnione przed pierwszym logowaniem.
- **Zaobserwowane:**
  - backup, potem odwołanie tokenu A, potem usunięcie kontenera bramy;
  - nowa brama pod innym adresem IP z tą samą nazwą: `install.sh`, `entrypoint install`, `entrypoint restore`;
  - oba odtworzone tokeny unieważnione, także B, nieodwołany przed utratą; stary token B jest odrzucany;
  - nowy token działa, a HTTPS pod nowym IP obsługuje ruch z certyfikatem z backupu.
- **Ograniczenia:** po restore trzeba wydać nowe tokeny wszystkim serwisom (decyzja ze specyfikacji, sekcja 6).
- **Decyzja:** zaliczone.

## Lista gotowości v0.1 (sekcja 19)

| Punkt | Stan |
| --- | --- |
| Wyniki eksperymentów 1–3 (zakres v0.1) w `docs/wyniki.md`, wariant B przyjęty albo odrzucony | Zrobione: B przyjęty |
| Zestawy e2e 1–7 i 12 oraz części v0.1 zestawów 8, 9 i 11 zielone w Dockerze | Zrobione: `make e2e`, zestawy Install, TLS, Routing, Tunnels, Replicas, State, Revoke, Secrets, Ops, SDK, Resilience |
| Instalacja na prawdziwym VPS z prawdziwym certyfikatem | Zrobione: test dymny poniżej |
| Host do upstreamu przez VPN działa, łącznie z WebSocketem | W Dockerze: upstream w osobnej sieci prywatnej, WebSocket i SSE (`TestRouting_UpstreamWebSocketSSEAndHTTP2`). Na prawdziwym VPN: **otwarte**, bo serwer testowy nie ma VPN |
| Host do tunelu z dwiema replikami Express działa (od 2026-10-18 adapter `node:http`, zob. niżej); zatrzymanie jednej nie przerywa serwisu | Zrobione: w Dockerze (testy 1.1, 1.5 i 1.8, macierz SDK) i na VPS (test dymny poniżej) |
| Odwołany token nie łączy się ponownie, a jego aktywna sesja jest zrywana | Zrobione: testy 3.1 i 1.2b |
| Backup i restore na świeżym kontenerze | Zrobione: `TestOps_BackupRestoreFreshContainer`, test 3.3 (nowy IP) |
| README z instalacją i użyciem CLI | Zrobione: `README.md` |

## Test dymny na prawdziwym serwerze (2026-10-18)

Serwer: Debian 13, systemd 257, 2 vCPU, 4 GB RAM, publiczny IPv4, bez firewalla systemu. Nazwa bramy `tunnel.mwdentry.stream`; DNS w Cloudflare (A `@` i `*`, DNS only). Repliki: dwie aplikacje Express (adapter) na komputerze w WSL2 za NAT-em, połączone przez internet.

| Sprawdzenie | Wynik |
| --- | --- |
| `install.sh` i `entrypoint install` na czystym systemie | Usługi aktywne. **Wykryty błąd:** entrypointd wystartował przed Caddy, a rewizja początkowa zakończyła się statusem `failed` i brama nie miała zastosowanej rewizji. Poprawka: odtwarzanie czeka na admin API Caddy, a brak zastosowanej rewizji tworzy nową. Test regresyjny: `TestState_DaemonStartsBeforeCaddy`. Po aktualizacji rewizja 2 zastosowana automatycznie |
| Certyfikat Let's Encrypt staging (`--acme-staging`) | Wystawiony dla `test.mwdentry.stream` (wystawca „(STAGING) Artificial Amaranth YE1”) |
| Certyfikat produkcyjny | Wystawiony dla `app.mwdentry.stream` (Let's Encrypt YE2), weryfikacja zaufania przechodzi, HTTP/2 |
| Przekierowanie HTTP na HTTPS | 308 |
| Dwie repliki za NAT-em | Obie w grupie. Ruch sekwencyjny: 20 z 20 żądań do jednej repliki (jedno połączenie keep-alive Caddy). Ruch równoległy (20 naraz): 44/36 |
| Drain jednej repliki pod ruchem | 300 z 300 żądań zakończonych kodem 200; replika usunięta z grupy |
| Odwołanie tokenu pod ruchem | Sesja zerwana po 5,4 s, 400 z 400 żądań zakończonych kodem 200 na pozostałej replice, konektor zakończony zdarzeniem `revoked` |
| WebSocket przez tunel | Echo przez `wss://app.mwdentry.stream/ws` |
| Porty z internetu | Otwarte 22, 80, 443 i 55555; zamknięte 2019, 7400, 7500, 20000 |

## Zmiana adaptera: Express na node:http (2026-10-18)

Na prośbę właściciela specyfikacji adapter `@k18k/entrypoint/express` zastąpił adapter `@k18k/entrypoint/http`. Przyjmuje dowolny handler `(req, res)`, więc aplikacje Express działają bez zmian, a SDK nie zależy już od Express. Logika drainu jest ta sama. Testy e2e (zestawy Replicas i SDK) uruchomiono ponownie na aplikacji testowej opartej na czystym `node:http`; wyniki eksperymentu 1 dla adaptera dotyczą obu wersji.
