# Entrypoint — specyfikacja

Oct 18, 2026 · @Krystian

Kompletna specyfikacja projektu po dwóch rundach recenzji. Zastępuje wcześniejsze wersje v0.1 i v0.2 oraz osobne karty planu prototypu i testów.

## 1. Cel i pozycjonowanie

Entrypoint to samodzielnie hostowana brama do publikowania usług. Działa na maszynie z publicznym IP, przyjmuje ruch HTTP(S) i kieruje go do usług osiągalnych bezpośrednio, przez VPN albo przez tunel z maszyn bez publicznego IP.

Przejmuje wybrane funkcje usług typu Cloudflare Tunnel: reverse proxy z automatycznym TLS, tunele dla aplikacji za NAT-em, a w dalszych etapach hosting statycznych frontendów. Nie zapewnia globalnej sieci, odporności na awarię lokalizacji ani ochrony przed nasyceniem łącza.

**Zależności zewnętrzne, które zostają:** dostawca DNS, urząd certyfikacji (domyślnie Let's Encrypt) i sieć dostawcy serwera. Wszystkie publikowane usługi dzielą jedną bramę, więc jej awaria dotyka wszystkich naraz. To świadomy kompromis.

**Zasada budowy.** Ścieżkę danych obsługują sprawdzone projekty: Caddy (HTTP i TLS) oraz frp (tunele). Własny kod to warstwa sterowania: stan, uprawnienia, cykl życia replik, odwołania, operacje, SDK i CLI. Własny protokół tunelowy nie jest planowany.

## 2. Wersje i zakres

Pierwsza używalna wersja to v0.1, przeznaczona do własnego użytku i sterowana przez CLI. Pierwszym wydaniem publicznym jest v0.2.0, z panelem i pełnymi testami awarii.

&#91;embedded content: etapy · prototyp, v0.1, bramka v0.2.0, dalsze wersje\]

| Wersja | Charakter | Zakres | Warunek wydania |
| --- | --- | --- | --- |
| v0.0.x | Prototyp | Rdzeń wariantu B sterowany z CLI, eksperymenty 1–3 | Wyniki eksperymentów zapisane (sekcja 17) |
| v0.1 | Do własnego użytku | Instalator, routing HTTP(S) i WebSockety, upstreamy IP:port, tunele z tokenami per serwis, prywatne CA, odwołanie tokenów, konektor, SDK z adapterem Express, CLI, backup i restore | Lista gotowości v0.1 (sekcja 19) |
| v0.2.0 | Pierwsze publiczne | Panel, adaptery Fastify i Hono, aktualizacja z automatycznym rollbackiem, tryb serwisowy, pełne testy e2e i awarii, ARM64 | Lista gotowości v0.2.0 (sekcja 19) |
| v0.3 | Rozszerzenie | Statyczne wydania | Testy zestawu statyki |
| v0.4 | Rozszerzenie | Przekierowania TCP/UDP i TURN, po osobnych prototypach | Wyniki prototypów UDP i TURN |
| v1 | Integracje i skala | Certyfikaty wildcard przez DNS-01, load balancing według zasobów, OIDC, HA, wbudowany VPN | Do ustalenia |

Aktualizacja w v0.1 jest ręczna: backup, potem instalacja nowej wersji. Automatyczny rollback aktualizacji wchodzi w v0.2.0.

## 3. Architektura

Cztery osobne procesy pod systemd, instalowane jednym instalatorem. Repliki serwisu tworzą grupę frp na jednym stałym porcie, więc Caddy ma dla każdego serwisu jeden statyczny upstream, a entrypointd nie leży w ścieżce danych (wariant B).

&#91;embedded content: architektura · wariant B, 4 procesy pod systemd\]

| Proces | Pochodzenie | Odpowiada za |
| --- | --- | --- |
| caddy | Oficjalna binarka, przypięta wersja | TLS i ACME, HTTP/1.1 i HTTP/2, WebSockety, reverse proxy do upstreamów i portów serwisów |
| frps | Oficjalna binarka, przypięta wersja | Transport tuneli; grupy replik na stałych portach serwisów, wiązanie tylko do 127.0.0.1, vhost wyłączony |
| entrypointd | Nasz kod, Go | Stan w SQLite, rewizje i ich stosowanie, plugin autoryzacji frps, przydział portów, dziennik odwołań, endpoint zgody na certyfikaty, API dla CLI i panelu |
| panel | Nasz kod, Node.js (od v0.2.0) | Interfejs; bez własnego stanu, wszystko przez API entrypointd |

Caddy i frps startują z ostatniej w pełni zastosowanej konfiguracji zapisanej na dysku. Brama uruchamia się więc nawet wtedy, gdy entrypointd i panel nie działają.

| Gdy nie działa | Ruch publiczny | Istniejące tunele | Nowe logowania tuneli | Zmiany konfiguracji |
| --- | --- | --- | --- | --- |
| panel | działa | działają | działają | przez CLI |
| entrypointd | działa | działają | odrzucane do restartu | niemożliwe |
| frps | działa poza tunelami | zerwane | po restarcie frps | możliwe |
| caddy | nie działa | bez ruchu | działają | możliwe |

Tabela zakłada plugin obsługujący tylko Login i NewProxy. Jeśli do zrywania sesji zostanie wybrane odrzucanie Ping, aktywne sesje zależą od entrypointd i wiersz entrypointd trzeba zmienić po teście 3.1b.

**Wariant porównawczy (SRV).** Dynamiczne upstreamy Caddy z lokalnego serwera DNS prowadzonego przez entrypointd. Rozwijany tylko wtedy, gdy wariant B nie przejdzie eksperymentu 1. Ma znane wady: zależność nowych żądań od demona, brak aktywnych health checków Caddy przy dynamicznych upstreamach i brak potwierdzenia wycofania repliki.

**Plan awaryjny dla frp** to ocena innego istniejącego projektu tunelowego, a nie własny protokół.

## 4. Stan konfiguracji

Konfiguracja to ciąg numerowanych, niezmiennych rewizji w SQLite. entrypointd zapisuje dla każdego komponentu numer ostatniej zastosowanej rewizji i uzgadnia stan oczekiwany z zastosowanym. Odwołania tokenów są poza rewizjami (sekcja 6).

**Rewizje.** Każda zmiana z CLI lub panelu tworzy rewizję N z autorem, czasem i różnicą względem N−1. Zmiana wskazuje rewizję bazową (`baseRev`); jeśli w międzyczasie powstała nowsza, zmiana jest odrzucana.

**Walidacja przed zapisem:** zgodność ze schematem, brak konfliktów tras (sekcja 8), istnienie wskazywanych serwisów, a na końcu wyrenderowanie konfiguracji Caddy i jej sprawdzenie bez stosowania.

**Stosowanie rewizji N:**

1. Oznaczenie N jako `applying`.
2. Render artefaktów: konfiguracja Caddy (JSON) i snapshot autoryzacji (serwisy, porty, tokeny z zakresami).
3. Snapshot autoryzacji: plik tymczasowy, fsync, atomowa zmiana nazwy, podmiana w pamięci. Przy podmianie zawsze odejmowane są tokeny z dziennika odwołań.
4. Caddy: pełna konfiguracja przez admin API (`/load`) i zapis tej samej treści do pliku startowego Caddy.
5. Oznaczenie N jako `applied` i zapis numeru per komponent.

**Częściowe niepowodzenie.** Gdy krok 4 zawiedzie po udanym kroku 3, entrypointd przywraca snapshot z ostatniej rewizji `applied`, nadal z odjętymi odwołaniami. N dostaje status `failed` z treścią błędu i nie jest ponawiana w pętli. Sesje tuneli otwarte w czasie obowiązywania snapshotu N są po rollbacku ponownie sprawdzane wobec przywróconego stanu, a niezgodne zrywane.

**Restart i awaria w trakcie.** Po starcie entrypointd stosuje ponownie ostatnią rewizję `applied` do wszystkich komponentów (operacja idempotentna). Rewizja przerwana w stanie `applying` jest ponawiana raz, a przy błędzie wycofywana jak wyżej.

**Dryf.** Co 30 s entrypointd porównuje hash konfiguracji zastosowanej w Caddy z oczekiwanym. Rozbieżność kończy się ponownym zastosowaniem i wpisem w logu.

**Stan replik nie jest częścią rewizji.** Źródłem prawdy o replikach jest frps, który sam zarządza członkostwem w grupach. entrypointd odczytuje go do statusu i kontroli spójności (sekcja 7).

## 5. Uprawnienia i model zaufania

Token daje wyłącznie jawnie przypisane uprawnienia, a sprawdza je brama, nigdy SDK. Trasy wskazują serwisy, więc token serwisu nie ma żadnego wpływu na routing.

| Uprawnienie | Na co pozwala | Kto je dostaje |
| --- | --- | --- |
| `replica:connect` serwis X | Podłączenie repliki do grupy serwisu X | Token serwisu |
| `routes:write` | Tworzenie i zmiana tras oraz serwisów | Tylko administrator (CLI, panel) |
| `tokens:manage` | Wydawanie i odwoływanie tokenów | Tylko administrator |
| `site:deploy` strona Y | Publikacja wydań strony Y | Osobny token deploy (v0.3) |
| `ports:open` | Otwieranie portów TCP/UDP | Tylko administrator (v0.4) |

Przykład: token backendu dla `example.com/api` pozwala tylko podłączać repliki serwisu `api`. Nie zmieni trasy `example.com/*` ani nie opublikuje frontendu.

**Model zaufania:**

- Posiadacz tokenu serwisu jest zaufanym operatorem tego serwisu. Health check frpc i drain konektora to mechanizmy zaufanego klienta; zmodyfikowany frpc może je wyłączyć, ale szkodzi wtedy tylko własnemu serwisowi.
- Granice między serwisami i odwołanie tokenu egzekwuje brama. Działają także wobec zmodyfikowanego frpc, który ignoruje konektor.
- Procesy lokalne na serwerze bramy są zaufane; porty serwisów na 127.0.0.1 nie są chronione przed nimi osobno.

**Egzekwowanie w pluginie frps:**

- **Login:** token z metadanych klienta. Nieznany lub odwołany jest odrzucany, a nieudane próby limitowane per adres IP.
- **NewProxy:** dozwolony tylko typ `tcp`. Plugin nadpisuje grupę, klucz grupy i port zdalny wartościami serwisu z tokenu. Pozostałe pola są sprawdzane względem białej listy; pola spoza niej, w tym customDomains, subdomain, locations i ustawienia innych typów proxy, są odrzucane. Nazwa proxy musi wskazywać serwis, do którego token ma uprawnienie.
- **Konfiguracja frps:** vhost HTTP i HTTPS wyłączony, wiązanie portów tylko do 127.0.0.1, dozwolone porty ograniczone do puli.

**Przechowywanie.** Token to losowe 256 bitów, pokazywane tylko raz. W bazie zapisany jest wyłącznie jego hash.

## 6. Tunele

Klient tunelu zawsze weryfikuje certyfikat serwera, autoryzacja działa bez panelu, a odwołanie tokenu jest trwałe i egzekwowane przez bramę.

**Tożsamość serwera.** frpc domyślnie szyfruje połączenie, ale nie weryfikuje certyfikatu frps; weryfikacja wymaga wskazania zaufanego CA. Instalator tworzy prywatne CA (klucz tylko na serwerze, prawa 0600) i certyfikat portu tunelu (domyślnie 55555) dla stałej nazwy DNS bramy, np. `tunnel.example.com`. Konektor zawsze ustawia weryfikację CA i nazwy serwera, a bez CA odmawia startu. Tryb bez weryfikacji istnieje tylko jako jawna flaga deweloperska.

**Pakiet połączenia** `ENTRYPOINT_CONNECT` zawiera nazwę DNS bramy, port, listę zaufanych CA i token. Używa nazwy, a nie adresu IP, więc przeniesienie bramy na inny IP wymaga tylko zmiany rekordu DNS.

**Odnawianie i rotacja.** Certyfikat portu tunelu ma ważność 1 roku i jest odnawiany przez entrypointd 30 dni przed wygaśnięciem, z tym samym CA, bez wpływu na klientów. Rotacja CA przebiega w trzech krokach: nowe pakiety połączenia z obydwoma CA, przełączenie certyfikatu serwera na nowe CA po aktualizacji klientów, usunięcie starego CA z pakietów. `entrypoint ca status` pokazuje, którzy klienci logują się jeszcze ze starym pakietem.

**Autoryzacja bez panelu.** Plugin frps to endpoint entrypointd na 127.0.0.1. Odpowiada z ostatniego zastosowanego snapshotu w pamięci, bez udziału panelu. Gdy entrypointd nie działa, frps odrzuca nowe logowania, a istniejące tunele pracują dalej; systemd restartuje demona, a klienci ponawiają logowanie.

**Porty serwisów.** Każdy serwis dostaje przy utworzeniu stały port z puli (domyślnie 20000–29999). Port usuniętego serwisu pozostaje trwale zarezerwowany i nigdy nie trafia do innego serwisu. Ruch serwisu A nie może więc trafić do serwisu B przez nieaktualny wpis.

**Reconnect.** frpc ponawia połączenie z wykładniczym backoffem. Po powrocie replika dołącza do grupy swojego serwisu na tym samym porcie.

**Odwołanie tokenu:**

1. Wpis trafia do dziennika odwołań: osobnej tabeli tylko do dopisywania, poza rewizjami. Rollback rewizji ani automatyczny rollback aktualizacji go nie cofają.
2. Snapshot autoryzacji jest aktualizowany od razu, więc nowe logowania i rejestracje z tym tokenem są odrzucane.
3. Aktywne sesje tokenu są zrywane przez bramę. Mechanizm wybiera prototyp (test 3.1): odrzucanie operacji Ping przez plugin albo, rezerwowo, restart frps z krótką przerwą dla pozostałych tuneli.

**Odwołania a awaryjny restore.** Backup sprzed odwołania nie zna tego odwołania. Dlatego awaryjny restore unieważnia wszystkie odtworzone tokeny, zanim frps i plugin zaczną przyjmować logowania; administrator wydaje je ponownie. Kopia dziennika odwołań poza maszyną, pozwalająca tokeny zachować, jest opcją na później.

## 7. Repliki i drain

Repliki serwisu tworzą grupę frp na porcie serwisu. Grupa rozdziela połączenia TCP, a nie żądania HTTP, i od tego zależą wszystkie gwarancje drainu.

**Konsekwencje rozdziału po połączeniach:**

- Kolejne żądania na jednym połączeniu keep-alive Caddy trafiają do tej samej repliki.
- Wycofanie repliki z grupy zatrzymuje nowe połączenia, ale nie nowe żądania na połączeniach już otwartych.
- Caddy widzi całą grupę jako jeden upstream. Jego health check nie rozróżnia replik, więc dla upstreamów tunelowych aktywne health checki Caddy są wyłączone. Inaczej błąd jednej repliki mógłby wyłączyć cały serwis.

**Gotowość i zdrowie.** Odpowiada za nie health check frpc ustawiany przez konektor: replika chora nie przyjmuje nowych połączeń z grupy. To mechanizm zaufanego klienta (sekcja 5). entrypointd odczytuje stan proxy z frps do `entrypoint status` i ostrzega o serwisie bez żadnej zdrowej repliki.

**Drain** przebiega w trzech osobnych krokach:

1. **Wycofanie z grupy:** konektor zamyka proxy, więc frps nie kieruje do repliki nowych połączeń.
2. **Wygaszanie połączeń:** istniejące połączenia nadal obsługują żądania. Adapter frameworka odpowiada wtedy z nagłówkiem `Connection: close`, przestaje przyjmować nowe połączenia i zamyka WebSockety kodem 1001. Kontrakt bazowy tego nie robi: czeka tylko na wygaśnięcie połączeń.
3. **Zakończenie:** po `drainTimeout` (domyślnie 30 s) konektor kończy frpc, co zrywa pozostałe połączenia.

| Gwarancja | Kontrakt bazowy | Adapter frameworka |
| --- | --- | --- |
| Brak nowych połączeń po wycofaniu | Tak | Tak |
| Brak nowych żądań na istniejących połączeniach | Dopiero po zamknięciu połączenia lub `drainTimeout` | Po zakończeniu bieżącego żądania na danym połączeniu |
| Żądanie krótsze niż `drainTimeout` kończy się poprawnie | Tak, jeśli aplikacja nadal działa | Tak |
| WebSockety i SSE zamknięte w kontrolowany sposób | Zerwane po `drainTimeout` | Zamknięte kodem 1001 |

Kontrakt bazowy nie kontroluje procesu aplikacji. Jeśli aplikacja zakończy się przed `drainTimeout`, aktywne żądania kończą się błędem.

| Timeout | Wartość domyślna | Gdzie |
| --- | --- | --- |
| Nawiązanie połączenia z portem serwisu | 5 s | Caddy |
| Oczekiwanie na nagłówki odpowiedzi | 60 s, zmienialne per trasa | Caddy |
| Bezczynne połączenie keep-alive do upstreamu | 4 s | Caddy |
| Drain repliki | 30 s, zmienialne w SDK | Konektor |
| WebSocket, SSE, strumień gRPC | Bez limitu, poza drainem | Caddy |

Krótki czas bezczynności keep-alive (4 s) ogranicza, jak długo wycofana replika może dostawać żądania na bezczynnych połączeniach.

**WebSockety a zmiany konfiguracji.** Zmiana listy replik nie dotyka konfiguracji Caddy, więc nie przerywa strumieni. Zmiana rewizji przeładowuje Caddy, a Caddy domyślnie zamyka wtedy WebSockety; ustawiamy `stream_close_delay` na 1 h. Restart lub aktualizacja Caddy zrywa wszystkie połączenia. Klienci WebSocket i SSE muszą umieć połączyć się ponownie.

## 8. Routing HTTP

Trasa to para host i prefiks ścieżki, wskazująca serwis tunelu albo stały upstream `IP:port`. Para jest unikalna, a konflikty są odrzucane przy walidacji rewizji. Trasy zmienia tylko administrator.

- **Dopasowanie:** dokładny host wygrywa z wildcardem (`*.example.com`). W obrębie hosta wygrywa najdłuższy prefiks. Prefiks pasuje na granicy segmentu: `/api` obejmuje `/api` i `/api/x`, ale nie `/apix`.
- **Prefiks** jest domyślnie przekazywany do upstreamu bez zmian; opcja `stripPrefix` go usuwa.
- **Normalizacja ścieżek:** zachowanie dla `..`, podwójnych ukośników i zakodowanego `%2F` ustalają testy routingu. Nie zakładamy niczego ponad to, co pokażą.
- **Nagłówki:** Host przekazywany bez zmian. Caddy ustawia X-Forwarded-For, X-Forwarded-Proto i X-Forwarded-Host, a wartości przysłane przez klienta są ignorowane, bo przed Caddy nie stoi zaufane proxy. Potwierdzają to testy.
- **Limity per trasa:** maksymalny rozmiar treści żądania (domyślnie 50 MB) i timeout nagłówków odpowiedzi.
- **Protokoły:** HTTP/1.1, HTTP/2 i WebSockety. HTTP/3 nie jest w zakresie v0.1.

## 9. TLS i ACME

Caddy wystawia certyfikaty per host przez ACME (wyzwania HTTP-01 i TLS-ALPN). Entrypoint nie konfiguruje DNS: `entrypoint dns check` pokazuje wymagane rekordy i ich bieżący stan.

**Limity Let's Encrypt:** do 50 nowych certyfikatów na domenę rejestrowalną w ciągu 7 dni, z odnawianiem puli o 1 certyfikat co 202 minuty. Wszystkie subdomeny jednej domeny dzielą ten limit. Odnowienia przez ACME Renewal Info są z limitów zwolnione.

- **Sprawdzenie DNS przed zamówieniem** to test wstępny, a nie ochrona przed limitem.
- **Licznik wystawionych certyfikatów** w `entrypoint status` liczy tylko certyfikaty tej instalacji i służy wyłącznie do ostrzeżeń. Nie widzi certyfikatów wystawionych poza Entrypointem, więc nie jest kontrolą.
- **Testy i CI** używają testowego urzędu Pebble, a próby na prawdziwym serwerze środowiska staging Let's Encrypt. Każdy host można przełączyć w tryb testowy.

**Subdomeny w locie** przy rekordzie `A *.example.com`: przed wystawieniem certyfikatu Caddy pyta endpoint zgody w entrypointd, a ten zgadza się tylko dla hostów z trasą. Każda nowa subdomena zużywa miejsce w limicie tygodniowym.

**Certyfikaty wildcard (v1)** wymagają wyzwania DNS-01: przez API dostawcy DNS albo przez delegację `_acme-challenge` rekordem CNAME do strefy, którą kontrolujemy.

Port tunelu korzysta z prywatnego CA (sekcja 6), a nie z ACME.

## 10. SDK i konektor

Podstawowy kontrakt to tunel do istniejącego lokalnego adresu. Aplikacja słucha jak zwykle, a SDK wystawia ten adres przez tunel. Adaptery frameworków dają silniejsze gwarancje drainu (sekcja 7).

```ts
import { tunnel } from "@k18k/entrypoint"

const t = await tunnel({
  connect: process.env.ENTRYPOINT_CONNECT, // nazwa bramy, port, CA, token
  service: "api",
  upstream: "http://127.0.0.1:3000",
  healthCheck: "/health",
})

process.on("SIGTERM", () => t.drain({ timeoutMs: 30_000 }))
```

To samo z linii poleceń, dla aplikacji w dowolnym języku:

```bash
npx @k18k/entrypoint tunnel --service api --upstream http://127.0.0.1:3000
```

| Adapter | Wersja | Co robi |
| --- | --- | --- |
| `@k18k/entrypoint/express` | v0.1 | `connectTo(app)` uruchamia serwer na `127.0.0.1:0` i tunel; przy drainie przestaje przyjmować połączenia, wysyła `Connection: close`, zamyka WebSockety kodem 1001 |
| `@k18k/entrypoint/fastify` | v0.2.0 | Czeka na `ready()` i używa serwera Fastify, z tym samym drainem |
| `@k18k/entrypoint/hono` | v0.2.0 | Uruchamia handler przez adapter Node dla Hono, z tym samym drainem |

Każdy adapter ma testy w CI dla wspieranych zakresów wersji frameworka i Node.js. Bun i Deno działają przez kontrakt bazowy do czasu dodania testów.

**Konektor** `entrypoint-connector` (Go) to mały proces pośredniczący nad oficjalną binarką frpc:

- buduje konfigurację frpc z pakietu połączenia, zawsze z weryfikacją CA i nazwy serwera oraz z health checkiem,
- obserwuje swoje standardowe wejście; gdy proces rodzica zginie, rura się zamyka, a konektor kończy frpc i siebie,
- prowadzi reconnect i drain oraz wysyła zdarzenia JSON (`connected`, `healthy`, `draining`, `disconnected`), które SDK tylko obserwuje,
- nigdy nie wypisuje tokenu ani pakietu połączenia.

Konektor rozmawia tylko z frps; nie ma osobnego kanału sterowania do entrypointd. Obsługiwane platformy: Linux x64 i arm64, macOS x64 i arm64, Windows x64, przez `optionalDependencies` per platforma.

## 11. CLI

CLI `entrypoint` to pełny interfejs administracyjny w v0.1 i równoważna alternatywa dla panelu od v0.2.0. Rozmawia z entrypointd przez lokalny socket, więc działa bez panelu.

| Polecenie | Co robi |
| --- | --- |
| `entrypoint install` | Instaluje usługi, tworzy prywatne CA, certyfikat portu tunelu i bazę |
| `entrypoint status` | Stan usług, rewizji, serwisów, replik i licznika certyfikatów |
| `entrypoint service add <nazwa> [--health <ścieżka>]` | Tworzy serwis i przydziela mu stały port |
| `entrypoint service rm <nazwa>` | Usuwa serwis; jego port zostaje trwale zarezerwowany |
| `entrypoint route add <host>[/prefiks] --to <ip:port>` | Trasa do upstreamu |
| `entrypoint route add <host>[/prefiks] --service <nazwa>` | Trasa do serwisu tunelu |
| `entrypoint route list`, `entrypoint route rm <id>` | Lista i usuwanie tras |
| `entrypoint token create --service <nazwa>` | Wydaje token i jednorazowo wypisuje `ENTRYPOINT_CONNECT` |
| `entrypoint token list`, `entrypoint token revoke <id>` | Lista i odwołanie tokenów |
| `entrypoint dns check` | Wymagane rekordy DNS i ich bieżący stan |
| `entrypoint ca status`, `entrypoint ca rotate` | Stan i rotacja prywatnego CA |
| `entrypoint revision list`, `entrypoint revision show <n>` | Historia rewizji i ich status |
| `entrypoint backup`, `entrypoint restore <plik>` | Kopia i odtworzenie |
| `entrypoint logs <usługa>` | Logi wybranej usługi |
| `entrypoint update` | Aktualizacja (automatyczny rollback od v0.2.0) |
| `entrypoint uninstall [--purge]` | Odinstalowanie, z danymi lub bez |

Każde polecenie zmieniające konfigurację tworzy rewizję i wypisuje jej numer oraz wynik stosowania.

## 12. Panel (od v0.2.0)

Panel jest minimalny i nie ma własnego stanu. Każda operacja przechodzi przez API entrypointd, więc awaria panelu odbiera tylko interfejs.

- **Zakres:** trasy i serwisy, tokeny (wydanie, lista, odwołanie), status replik i rewizji z błędami stosowania, wymagane rekordy DNS i stan certyfikatów.
- **Logowanie:** jedno konto administratora, hasło hashowane argon2id, limit prób per adres IP.
- **Sesja:** ciasteczko z atrybutami HttpOnly, Secure i SameSite=Strict.
- **CSRF:** każda operacja zmieniająca stan wymaga tokenu CSRF i zgodnego nagłówka Origin.
- **Ekspozycja:** panel jest publicznie osiągalny przez Caddy na własnej domenie; prywatny jest tylko jego port backendowy. Dostępne ograniczenia: lista dozwolonych adresów IP dla domeny panelu albo tryb lokalny bez trasy publicznej, z dostępem przez tunel SSH. Instalator zaleca jedno z nich.
- **Stack:** backend Express (TypeScript), frontend React + Vite budowany statycznie i serwowany przez Caddy.

## 13. Instalacja i operacje

Jeden instalator zakłada osobne usługi systemd spięte w `entrypoint.target`. Programy, dane i konfiguracja leżą w osobnych katalogach, więc aktualizacja i odinstalowanie nie dotykają danych.

```bash
curl -fsSL <adres-skryptu>/install.sh | sudo sh
sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com
```

| Ścieżka | Zawartość |
| --- | --- |
| `/opt/entrypoint/releases/<wersja>` | Programy danej wersji, w tym przypięte binarki Caddy i frps |
| `/opt/entrypoint/current` | Dowiązanie do aktywnej wersji |
| `/etc/entrypoint` | Konfiguracja startowa |
| `/var/lib/entrypoint` | SQLite z rewizjami i dziennikiem odwołań, prywatne CA, magazyn Caddy (konto ACME, certyfikaty), pliki startowe Caddy i frps |

Każdy proces działa jako osobny użytkownik systemowy. Caddy dostaje tylko uprawnienie do portów poniżej 1024. Logi trafiają do journald. Wspierany jest wyłącznie systemd.

**Aktualizacja (automatyczna od v0.2.0):**

1. Włączenie trybu serwisowego: blokada zmian administracyjnych poza odwołaniami tokenów, aż do końca kroku 8.
2. Pobranie nowej wersji do `releases/<wersja>` i weryfikacja podpisu wydania.
3. Zatrzymanie starego entrypointd, potem backup.
4. Migracje bazy: tylko do przodu, w transakcji.
5. Przełączenie dowiązania `current`.
6. Restart w kolejności: entrypointd, frps, caddy, panel.
7. Kontrola zdrowia wszystkich usług przez 60 s.
8. Przy błędzie: powrót dowiązania i bazy z kroku 3, ponowne naniesienie odwołań zapisanych w trybie serwisowym, potem wyłączenie trybu serwisowego.

Restart frps zrywa tunele, a restart Caddy wszystkie połączenia, więc aktualizacja jest krótkim oknem serwisowym. W v0.1 aktualizacja jest ręczna: backup, potem instalacja nowej wersji.

**Backup.** `entrypoint backup` tworzy archiwum zaszyfrowane hasłem: kopię SQLite wykonaną bez zatrzymywania usług, prywatne CA z kluczem, magazyn Caddy i manifest wersji; od v0.3 także wydania stron. Logi nie wchodzą do backupu.

**Restore:**

1. Instalacja tej samej wersji na czystej maszynie.
2. `entrypoint restore <plik>`. Przy awaryjnym restore wszystkie odtworzone tokeny są unieważniane, zanim frps zacznie przyjmować logowania (sekcja 6).
3. Rekoncyliacja i start usług.
4. Przy nowym adresie IP: aktualizacja rekordów DNS. Pakiety połączenia używają nazwy bramy, więc klienci nie wymagają zmian poza nowym tokenem.

**Odinstalowanie.** `entrypoint uninstall` usuwa usługi i programy, ale zostawia `/var/lib/entrypoint` i `/etc/entrypoint`. Flaga `--purge` usuwa także dane, dopiero po potwierdzeniu i z propozycją backupu.

## 14. Ochrona sekretów i limity

Sekrety nigdy nie trafiają do logów, a podstawowe limity obowiązują od v0.1.

**Sekrety:**

- Tokeny, pakiety połączenia, klucz CA i hasła nie pojawiają się w logach, komunikatach błędów ani zdarzeniach diagnostycznych żadnego komponentu. entrypointd maskuje metadane z żądań pluginu, a poziom logowania frps nie może wypisywać metadanych klientów (sprawdza zestaw testów 12).
- Pliki z sekretami mają prawa 0600 i należą do użytkownika usługi, która ich potrzebuje.
- Backup jest szyfrowany hasłem podawanym przy tworzeniu.

**Limity od v0.1:**

| Limit | Wartość domyślna |
| --- | --- |
| Repliki na token | 10 |
| Pula portów serwisów | 20000–29999 |
| Nieudane logowania tunelu na adres IP | 10 na minutę |
| Rozmiar treści żądania | 50 MB per trasa |
| Rozmiar nagłówków żądania | Domyślny limit Caddy |

Limity połączeń na adres IP po stronie publicznej wymagają decyzji przed v0.2.0: wtyczka Caddy albo reguły firewalla systemu. Lokalne limity nie chronią przed nasyceniem łącza.

## 15. Dalsze etapy

**Statyczne wydania (v0.3).** Publikacja nowego wydania nie psuje przeglądarek z poprzednią wersją, ale tylko dla zasobów z hashem treści w nazwie pliku. Ścieżki bez hasha, np. `/config.json`, zawsze zwracają wersję bieżącą; serwer nie rozstrzyga, którą wersję chce stara karta przeglądarki.

- **Retencja:** pliki z hashem z poprzednich wydań zostają dostępne przez 7 dni lub 5 ostatnich wydań, co trwa dłużej. `index.html` zawsze pochodzi z bieżącego wydania.
- **Brakujące pliki:** fallback na `index.html` tylko dla nawigacji (GET, Accept z `text/html`, ścieżka bez rozszerzenia). Brakujący `/assets/app.js` dostaje 404, nigdy HTML.
- **Upload:** tar.gz strumieniowo, z limitami rozmiaru całości, pliku i liczby plików. Odrzucane są ścieżki bezwzględne, segmenty `..`, dowiązania symboliczne i twarde oraz pliki specjalne. Rozpakowanie do katalogu tymczasowego, publikacja dopiero po weryfikacji.
- **Równoległe deploye:** kolejka per strona, wydania numerowane rosnąco, publikacja w kolejności przyjęcia. Opcja `--expect-current <id>` odrzuca deploy, jeśli bieżące wydanie zmieniło się od rozpoczęcia budowania.
- **Uprawnienie** `site:deploy` per strona, niezależne od tokenów tuneli.

**Przekierowania TCP/UDP i TURN (v0.4)** powstają dopiero po osobnych prototypach. Do sprawdzenia: UDP przez frp z transportem QUIC przy stratach, jitterze i przeciążeniu oraz TURN/TLS współdzielący port 443 z HTTPS przez SNI. TURN wymaga własnego modelu uwierzytelniania i limitów, żeby brama nie stała się otwartym przekaźnikiem ruchu.

**v1:** certyfikaty wildcard przez DNS-01, load balancing według zasobów zgłaszanych przez repliki, OIDC przed aplikacjami, kilka bram (HA) i wbudowany VPN.

## 16. Status twierdzeń

Twierdzenia tej specyfikacji dzielą się na potwierdzone dokumentacją (8), własne decyzje projektowe (9) i założenia wymagające eksperymentu (11). Eksperyment może zmienić decyzję; wtedy zmiana trafia do tej tabeli razem z wynikiem.

| Twierdzenie | Status | Źródło lub test |
| --- | --- | --- |
| frps wywołuje zewnętrzny plugin HTTP przed operacjami Login, NewProxy, CloseProxy, Ping, NewWorkConn i NewUserConn; plugin może odrzucić operację albo zmienić jej treść | Dokumentacja | [gofrp: Server Plugin](https://gofrp.org/en/docs/features/common/server-plugin/) |
| Globalne metadane klienta frpc trafiają do pluginu w operacji Login | Dokumentacja | [gofrp: Server Plugin](https://gofrp.org/en/docs/features/common/server-plugin/) |
| frpc domyślnie nie weryfikuje certyfikatu frps; weryfikację włącza wskazanie zaufanego CA | Dokumentacja | [gofrp: TLS](https://gofrp.org/en/docs/features/common/network/network-tls/) |
| Proxy typu tcp z tą samą grupą, kluczem grupy i portem zdalnym tworzą grupę, między którą frps rozdziela połączenia | Dokumentacja | [gofrp: Load Balancing](https://gofrp.org/en/docs/features/common/load-balancer/) |
| Health check frpc (tcp lub http) usuwa proxy z frps po kolejnych nieudanych sprawdzeniach | Dokumentacja | [gofrp: Load Balancing](https://gofrp.org/en/docs/features/common/load-balancer/) |
| Caddy domyślnie zamyka WebSockety przy przeładowaniu konfiguracji; `stream_close_delay` utrzymuje je przez zadany czas | Dokumentacja | [forum Caddy](https://caddy.community/t/way-to-achieve-zero-downtime-for-existing-connections-when-dynamic-patch-maps/33753) |
| Let's Encrypt: 50 nowych certyfikatów na domenę rejestrowalną na 7 dni; odnowienia przez ARI zwolnione | Dokumentacja | [Let's Encrypt: rate limits](https://letsencrypt.org/docs/rate-limits/) |
| Aktywne health checki Caddy nie działają z dynamicznymi upstreamami (dotyczy wariantu SRV) | Dokumentacja | [Caddy: reverse\_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy) |
| Wariant B: repliki jako grupy frp na stałych portach serwisów | Decyzja projektowa | Sekcja 3 |
| Port usuniętego serwisu trwale zarezerwowany | Decyzja projektowa | Sekcja 6 |
| Plugin obsługuje tylko Login i NewProxy, chyba że test 3.1 wymusi Ping | Decyzja projektowa | Sekcje 3 i 6 |
| Osobne procesy pod systemd, jeden instalator | Decyzja projektowa | Sekcja 3 |
| Prywatne CA i pakiet połączenia z nazwą DNS bramy | Decyzja projektowa | Sekcja 6 |
| Dziennik odwołań poza rewizjami; awaryjny restore unieważnia odtworzone tokeny | Decyzja projektowa | Sekcja 6 |
| Aktywne health checki Caddy wyłączone dla upstreamów tunelowych | Decyzja projektowa | Sekcja 7 |
| Health check frpc i drain konektora to mechanizmy zaufanego klienta | Decyzja projektowa | Sekcja 5 |
| Kontrakt SDK na lokalnym adresie; adaptery z osobnymi gwarancjami | Decyzja projektowa | Sekcje 7 i 10 |
| Wycofanie repliki z grupy, wygaszanie połączeń keep-alive i zakończenie frpc działają jako trzy osobne kroki | Eksperyment | Testy 1.1–1.4 |
| Proxy z health checkiem nie dostaje połączeń przed pierwszym udanym sprawdzeniem | Eksperyment | Test 1.10 |
| Brama potrafi zerwać aktywną sesję odwołanego tokenu bez współpracy klienta | Eksperyment | Testy 3.1 i 3.1b |
| Niedostępny plugin powoduje odrzucanie logowań, bez wpływu na istniejące tunele | Eksperyment | Test 1.6 |
| Konfiguracja przez `/load` jest stosowana atomowo, a przy błędzie Caddy zostaje przy poprzedniej | Eksperyment | Zestaw e2e 6 |
| Normalizacja ścieżek i ignorowanie nagłówków X-Forwarded-\* od klienta | Eksperyment | Zestaw e2e 3 |
| frps nie wypisuje metadanych klientów do logów przy wybranym poziomie logowania | Eksperyment | Zestaw e2e 12 |
| Plik zaufanego CA frpc może zawierać kilka certyfikatów, co umożliwia rotację CA | Eksperyment | Test 2.5 |
| entrypointd może wiarygodnie odczytać stan proxy z frps do statusu i kontroli spójności | Eksperyment | Test 2.4 |
| UDP przez frp z QUIC oraz TURN/TLS na porcie 443 | Eksperyment | Prototypy v0.4 |
| Cel wydajności na opisanej maszynie referencyjnej, z tunelem i bez | Eksperyment | Benchmark przed v0.2.0 |

## 17. Plan prototypu

Prototyp sprawdza trzy ryzyka architektury: drain w grupach frp, tożsamość i cykl życia replik oraz trwałe odwołanie tokenów. Wariant B jest testowany jako pierwszy; jeśli przejdzie eksperyment 1, wariant SRV nie jest rozwijany.

Zasady:

- Wersje Caddy i frp są przypięte w `versions.json` i wpisywane do wyników.
- Limity w kolumnie kryterium są wstępne. Pomiar rozpoznawczy może je poprzedzić, ale zmiana limitu jest zapisywana jawnie przed testem zaliczeniowym. Sam pomiar nie jest zaliczeniem.
- Wynik każdego testu trafia do `docs/wyniki.md` w czterech polach: oczekiwane, zaobserwowane, ograniczenia, decyzja.

**Eksperyment 1: grupy frp i drain**

| # | Test | Kryterium zaliczenia |
| --- | --- | --- |
| 1.1 | Nowe żądanie na istniejącym keep-alive Caddy po wycofaniu repliki z grupy | Z adapterem: brak nowych żądań do wycofanej repliki po 5 s. Kontrakt bazowy: brak po `drainTimeout` |
| 1.2a | Łagodny drain w trakcie uploadu mieszczącego się w `drainTimeout` | Odpowiedź 2xx i kompletne dane w aplikacji |
| 1.2b | Natychmiastowe odwołanie tokenu w trakcie uploadu | Upload przerwany, klient dostaje błąd, aplikacja nie traktuje częściowych danych jako sukcesu |
| 1.3 | SSE i WebSocket przez wycofywaną replikę | Z adapterem: zamknięte kodem 1001 przed `drainTimeout`. Kontrakt bazowy: zerwane po `drainTimeout` |
| 1.4 | Wycofanie z grupy, wygaszanie połączeń, koniec frpc | Każdy krok obserwowalny i wywoływany osobno |
| 1.5 | Jedna replika zwraca błędy, druga jest zdrowa | Serwis nadal odpowiada przez zdrową replikę; chora usunięta z grupy w czasie interwał × liczba prób + 5 s |
| 1.6 | Awaria entrypointd przy ruchu | Nowe żądania obsługiwane; nowe logowania odrzucane; po restarcie logowania wracają w 30 s |
| 1.7 | Zimny start Caddy przy niedziałającym entrypointd | Caddy startuje z pliku i obsługuje ruch |
| 1.8 | Usunięcie ostatniej repliki | Odpowiedź 502 lub 503 w czasie do 1 s; żadne połączenie do usuniętej repliki nie trwa dłużej niż 5 s |
| 1.9 | Restart frps | Repliki wracają do grup na tych samych portach w 30 s |
| 1.10 | Nowa replika z health checkiem | Żadne połączenie nie trafia do niej przed pierwszym udanym sprawdzeniem |

**Eksperyment 2: tożsamość i cykl życia replik**

| # | Test | Kryterium zaliczenia |
| --- | --- | --- |
| 2.1 | Zmodyfikowany frpc podaje cudzą grupę, port lub serwis | Odrzucony przez plugin i zalogowany, bez wpływu na inne serwisy |
| 2.2 | Pola NewProxy spoza białej listy | Odrzucone albo nadpisane przez plugin |
| 2.3 | Usunięcie serwisu i utworzenie nowego | Port usuniętego serwisu nigdy nie trafia do nowego |
| 2.4 | Restarty entrypointd i frps w różnej kolejności | Status w entrypointd zgodny ze stanem frps, bez rozjazdu |
| 2.5 | Rotacja CA | Klient z pakietem dwóch CA łączy się przed przełączeniem i po nim; klient tylko ze starym CA jest odrzucany po przełączeniu |

**Eksperyment 3: trwałe odwołanie tokenów**

| # | Test | Kryterium zaliczenia |
| --- | --- | --- |
| 3.1 | Klienci A (zmodyfikowany frpc ignorujący konektor) i B na różnych tokenach; odwołanie tokenu A | A odłączony i nie łączy się ponownie; B bez przerwy. Przy rezerwowym restarcie frps B wraca w 10 s |
| 3.1b | Tylko jeśli wybrano odrzucanie Ping: powtórzenie testu 1.6 | Zmierzony i zaakceptowany wpływ awarii entrypointd na aktywne sesje; aktualizacja tabeli awarii w sekcji 3 |
| 3.2 | Odwołanie w trakcie częściowego stosowania rewizji, potem rollback | Odwołanie trwa; sesje otwarte w międzyczasie sprawdzone ponownie |
| 3.3 | Backup o 10:00, odwołanie o 11:00, utrata maszyny, restore z 10:00 | Wszystkie odtworzone tokeny unieważnione przed pierwszym logowaniem |
| 3.4 | Odwołanie w trybie serwisowym podczas aktualizacji zakończonej rollbackiem (v0.2.0) | Odwołanie zachowane |
| 3.5 | Udana migracja, po której nowa wersja nie przechodzi kontroli zdrowia (v0.2.0) | Powrót do poprzedniej wersji bez utraty odwołań |

## 18. Testy e2e

Zestaw e2e uruchamia się jednym poleceniem (`make e2e`) w kontenerach Docker. Brama jest instalowana prawdziwym instalatorem na czystym kontenerze. Testy pokazują, że opisane scenariusze działają, ale nie dowodzą braku błędów poza nimi; zestaw rośnie z każdym znalezionym błędem.

| Kontener | Rola | Narzędzie |
| --- | --- | --- |
| gateway | Czysty system z systemd, na którym działa instalator i brama | Obraz Debian z systemd |
| acme | Testowy urząd certyfikacji zamiast Let's Encrypt | Pebble, skonfigurowany pod powtarzalne testy |
| dns | Rozwiązuje domeny testowe na adres bramy | CoreDNS |
| client-a, client-b | Aplikacje z konektorem na różnych tokenach | Node.js z SDK, frpc |
| attacker | Zmodyfikowany frpc próbujący obejść uprawnienia | frpc z własną konfiguracją |
| upstream | Usługa osiągalna przez IP:port | Serwer HTTP, SSE i WebSocket |
| chaos | Opóźnienia, straty pakietów i odcięcia sieci klientów | tc netem lub Toxiproxy |
| runner | Wykonuje scenariusze i zbiera artefakty | Testy w Go, Playwright dla panelu |

Po każdym teście zostają artefakty: logi wszystkich kontenerów, zastosowane konfiguracje Caddy i frps, stan bazy i wynik. `make e2e SUITE=<nazwa>` uruchamia jeden zestaw. Systemd w kontenerze wymaga dodatkowych uprawnień; jeśli zafałszuje to wyniki, zestaw 1 przechodzi do maszyny wirtualnej.

| # | Zestaw | Przypadki | Od wersji |
| --- | --- | --- | --- |
| 1 | Instalator i odinstalowanie | Instalacja na czystej maszynie; ponowna instalacja bez szkód; skan portów potwierdza niedostępność admin API Caddy, dashboardu frps i backendu panelu; `uninstall` zostawia dane, `--purge` je usuwa | v0.1 |
| 2 | TLS i ACME | Cert z Pebble po pojawieniu się rekordu DNS; brak certu dla hosta bez trasy; odnowienie; konektor odrzuca serwer z obcym CA | v0.1 |
| 3 | Routing HTTP | Host i prefiks, najdłuższy prefiks, granica segmentu, `stripPrefix`, konflikty, normalizacja ścieżek, X-Forwarded-\* od klienta, limity rozmiaru, upstream IP:port, WebSocket, SSE, HTTP/2 | v0.1 |
| 4 | Tunele i uprawnienia | Testy 2.1–2.5; token bez zakresu; niedozwolone typy i pola NewProxy | v0.1 |
| 5 | Repliki i drain | Testy 1.1–1.10 | v0.1 |
| 6 | Stan konfiguracji | Konflikt `baseRev`; błąd Caddy po kroku autoryzacji; zabicie entrypointd w trakcie stosowania; dryf | v0.1 |
| 7 | Odwołanie tokenów | Testy 3.1–3.3 w v0.1, 3.4–3.5 w v0.2.0 | v0.1 |
| 8 | Operacje | Backup i restore na świeżym kontenerze; restore pod nowym IP przy stałej nazwie bramy; aktualizacja z błędną migracją i z nieudaną kontrolą zdrowia; tryb serwisowy | v0.1 (backup, restore), v0.2.0 (aktualizacja) |
| 9 | SDK i adaptery | Kontrakt bazowy i adaptery w macierzy wersji Node.js; `kill -9` rodzica kończy konektor i frpc | v0.1 (Express), v0.2.0 (pozostałe) |
| 10 | Panel | Logowanie i limit prób; ciasteczko HttpOnly, Secure, SameSite; CSRF; trasy, serwisy, tokeny; odwołanie z panelu | v0.2.0 |
| 11 | Odporność | Awaria entrypointd, frps i atrapy klienta API; restart Caddy; odcięcie sieci klienta na 5 s i 60 s; straty pakietów | v0.1 (podstawowe), v0.2.0 (pełne) |
| 12 | Ochrona sekretów | Przeszukanie logów, błędów i zdarzeń pod kątem tokenów i kluczy; uprawnienia plików z sekretami | v0.1 |

**Czego kontenery nie pokażą:** prawdziwego Let's Encrypt i jego limitów, firewalla dostawcy VPS, publicznego DNS i IPv6, nasycenia łącza, ARM64 (wymaga osobnego runnera albo emulacji) ani wiarygodnej wydajności. Dlatego każde wydanie przechodzi jeszcze test dymny na prawdziwym serwerze.

## 19. Gotowość wydań

**v0.1, do własnego użytku:**

- [ ] Wyniki eksperymentów 1–3 (w zakresie v0.1) zapisane w `docs/wyniki.md`, a wariant B przyjęty albo odrzucony
- [ ] Zestawy e2e 1–7, 12 oraz części v0.1 zestawów 8, 9 i 11 zielone w Dockerze
- [ ] Instalacja na prawdziwym VPS z prawdziwym certyfikatem
- [ ] Host do upstreamu przez VPN działa, łącznie z WebSocketem
- [ ] Host do tunelu z dwiema replikami Express działa; zatrzymanie jednej nie przerywa serwisu
- [ ] Odwołany token nie łączy się ponownie, a jego aktywna sesja jest zrywana
- [ ] Backup i restore na świeżym kontenerze
- [ ] README z instalacją i użyciem CLI

**v0.2.0, pierwsze wydanie publiczne:**

- [ ] Zestawy e2e 1–12 zielone w CI na x86\_64
- [ ] Zestawy 1, 4 i 5 zielone na ARM64
- [ ] Panel z ochroną sesji i CSRF
- [ ] Aktualizacja z automatycznym rollbackiem i trybem serwisowym
- [ ] Decyzja o limitach połączeń na adres IP
- [ ] Test dymny na prawdziwym VPS ze stagingiem Let's Encrypt
- [ ] Restore na świeżej maszynie poza środowiskiem testowym
- [ ] Benchmark na maszynie referencyjnej z zapisanym wynikiem

## 20. Start w Claude Code

**Przygotowanie:**

- [ ] Claude Code na komputerze z Dockerem, Go i Node.js
- [ ] VPS z Linuksem i dostępem SSH
- [ ] Domena testowa z możliwością ustawiania rekordów
- [ ] Ta specyfikacja wyeksportowana do Markdown jako `docs/specyfikacja.md` w nowym repo

**Prompt startowy** do wklejenia jako pierwsza wiadomość, z Claude Code uruchomionym w katalogu repo:

```markdown
Budujemy entrypoint: samodzielnie hostowaną bramę do publikowania usług.
Pełna specyfikacja jest w docs/specyfikacja.md. Przeczytaj ją całą przed pisaniem kodu.

Cel sesji: wersja v0.1 (sekcja 2 i lista gotowości v0.1 w sekcji 19),
zainstalowana na moim VPS i sterowana przez CLI.

Zasady:
- Zakres wyłącznie v0.1. Bez panelu i bez funkcji z późniejszych wersji.
- Monorepo: gateway/ (Go: entrypointd, CLI, konektor), sdk/node (TypeScript),
  e2e/ (Docker Compose), scripts/install.sh.
- Caddy i frp jako oficjalne binarki w przypiętych wersjach, zapisanych w versions.json.
- Wariant B (grupy frp). Jeśli eksperyment z sekcji 17 pokaże, że B nie spełnia
  kryteriów, zatrzymaj się i opisz problem, zanim zmienisz architekturę.
- Najpierw środowisko e2e z sekcji 18, potem funkcje. Każdą kończ zielonym testem.
- Kolejność: szkielet i e2e, instalator, entrypointd i routing Caddy, tunele i plugin,
  konektor i SDK, odwołanie tokenów, backup i restore, instalacja na VPS.
- Sekrety nigdy w logach. Klient tunelu zawsze weryfikuje CA.
- Wyniki testów z sekcji 17 zapisuj w docs/wyniki.md:
  oczekiwane, zaobserwowane, ograniczenia, decyzja.
- Przed instalacją na VPS zapytaj mnie o dostęp i domenę.

Na koniec: README z instalacją i użyciem CLI oraz lista tego, co nie weszło.
```

## Źródła

- [gofrp: Server Plugin](https://gofrp.org/en/docs/features/common/server-plugin/)
- [gofrp: TLS](https://gofrp.org/en/docs/features/common/network/network-tls/)
- [gofrp: Load Balancing and Health Check](https://gofrp.org/en/docs/features/common/load-balancer/)
- [Caddy: reverse\_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)
- [forum Caddy: WebSockety przy przeładowaniu konfiguracji](https://caddy.community/t/way-to-achieve-zero-downtime-for-existing-connections-when-dynamic-patch-maps/33753)
- [Let's Encrypt: rate limits](https://letsencrypt.org/docs/rate-limits/)
