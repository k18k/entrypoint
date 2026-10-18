// Po instalacji SDK pobiera konektor i frpc dla bieżącej platformy z wydania na GitHubie
// (sumy SHA256 z connector.json). Błąd pobierania nie przerywa instalacji: SDK spróbuje
// ponownie przy pierwszym tunnel(), a ENTRYPOINT_CONNECTOR pozwala wskazać własną binarkę.
import { existsSync } from "node:fs";

const skip =
  process.env.ENTRYPOINT_SKIP_CONNECTOR_DOWNLOAD ||
  process.env.ENTRYPOINT_CONNECTOR ||
  !existsSync(new URL("../lib/connector.js", import.meta.url));

if (!skip) {
  try {
    const { ensureConnector } = await import("../lib/connector.js");
    await ensureConnector();
  } catch (err) {
    console.warn(
      `entrypoint: nie pobrano konektora (${err instanceof Error ? err.message : err}); spróbuję przy pierwszym tunelu`,
    );
  }
}
