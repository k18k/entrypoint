# entrypoint

Samodzielnie hostowana brama do publikowania usług.
Źródło prawdy: docs/specyfikacja.md. Wyniki eksperymentów: docs/wyniki.md.

## Zasady
- Zakres bieżącej pracy to wersja wskazana w zadaniu; bez funkcji z późniejszych wersji.
- Monorepo: gateway/ (Go: entrypointd, CLI, konektor), sdk/node (TypeScript),
  e2e/ (Docker Compose), scripts/install.sh.
- Caddy i frp jako oficjalne binarki w wersjach z versions.json.
- Architektura: wariant B (grupy frp). Zmiana architektury tylko po opisaniu
  problemu i mojej zgodzie.
- Każda funkcja kończy się zielonym testem e2e; `make e2e` musi przechodzić przed commitem.
- Sekrety nigdy w logach, błędach ani wyjściu poleceń. Klient tunelu zawsze weryfikuje CA.
- Małe commity z opisem po polsku; nie pushuj bez pytania.
- Przed jakąkolwiek operacją na VPS zapytaj mnie o zgodę.