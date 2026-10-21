// After install, the SDK downloads the connector and frpc for the current platform from the GitHub
// release (SHA256 sums from connector.json). A download failure does not abort the install: the SDK retries
// on the first tunnel(), and ENTRYPOINT_CONNECTOR lets you point to your own binary.
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
      `entrypoint: connector not downloaded (${err instanceof Error ? err.message : err}); will retry on the first tunnel`,
    );
  }
}
