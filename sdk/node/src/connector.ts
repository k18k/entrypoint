// Downloads the connector and frpc for the current platform from the Entrypoint GitHub release.
// The SHA256 sums are stored in connector.json in the SDK package, so a downloaded file is
// rejected if it does not match what was released together with this SDK version.
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { chmod, mkdir, rename, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

interface Asset {
  url: string;
  sha256: string;
}

interface Manifest {
  version: string;
  platforms: Record<string, { connector: Asset; frpc: Asset }>;
}

const PLATFORMS: Record<string, string> = {
  "linux-x64": "linux_amd64",
  "linux-arm64": "linux_arm64",
  "darwin-x64": "darwin_amd64",
  "darwin-arm64": "darwin_arm64",
  "win32-x64": "windows_amd64",
};

const PKG_ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");

export function connectorExe(): string {
  return process.platform === "win32" ? "entrypoint-connector.exe" : "entrypoint-connector";
}

function frpcExe(): string {
  return process.platform === "win32" ? "frpc.exe" : "frpc";
}

/** Platform key in the release, e.g. linux_amd64. */
export function platformKey(): string | undefined {
  return PLATFORMS[`${process.platform}-${process.arch}`];
}

function readManifest(): Manifest | undefined {
  const p = join(PKG_ROOT, "connector.json");
  if (!existsSync(p)) return undefined;
  return JSON.parse(readFileSync(p, "utf8")) as Manifest;
}

/** Directories where we look for (and download to) the connector of this SDK version. */
export function connectorDirs(): string[] {
  const version = readManifest()?.version ?? "dev";
  const cache = process.env.XDG_CACHE_HOME ?? join(homedir(), ".cache");
  return [join(PKG_ROOT, "bin"), join(cache, "entrypoint", version)];
}

// Binaries come only from GitHub releases over HTTPS; integrity is checked by SHA256 below.
const ALLOWED_PREFIX = "https://github.com/";

async function download(asset: Asset, dest: string): Promise<void> {
  if (!asset.url.startsWith(ALLOWED_PREFIX))
    throw new Error(`disallowed download URL: ${asset.url}`);
  const res = await fetch(asset.url, { redirect: "follow" });
  if (!res.ok) throw new Error(`download ${asset.url}: HTTP ${res.status}`);
  const buf = Buffer.from(await res.arrayBuffer());
  const got = createHash("sha256").update(buf).digest("hex");
  if (got !== asset.sha256) {
    throw new Error(`bad SHA256 sum for ${asset.url} (${got}, expected ${asset.sha256})`);
  }
  const tmp = `${dest}.tmp-${process.pid}`;
  await writeFile(tmp, buf, { mode: 0o755 });
  await chmod(tmp, 0o755);
  await rename(tmp, dest);
}

/**
 * Ensures the connector and frpc for the current platform: returns the connector path,
 * downloading the files from the release if they are not there yet. frpc goes next to the connector.
 */
export async function ensureConnector(): Promise<string> {
  for (const dir of connectorDirs()) {
    const c = join(dir, connectorExe());
    if (existsSync(c) && existsSync(join(dir, frpcExe()))) return c;
  }
  const manifest = readManifest();
  const key = platformKey();
  const assets = key ? manifest?.platforms[key] : undefined;
  if (!manifest || !key || !assets) {
    throw new Error(`no connector for platform ${process.platform}-${process.arch}`);
  }
  let lastErr: unknown;
  for (const dir of connectorDirs()) {
    try {
      await mkdir(dir, { recursive: true });
      await download(assets.frpc, join(dir, frpcExe()));
      await download(assets.connector, join(dir, connectorExe()));
      return join(dir, connectorExe());
    } catch (err) {
      lastErr = err; // e.g. a read-only package directory: try the cache
    }
  }
  throw lastErr instanceof Error ? lastErr : new Error(String(lastErr));
}
