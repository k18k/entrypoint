// Pobieranie konektora i frpc dla bieżącej platformy z wydania Entrypoint na GitHubie.
// Sumy SHA256 są zapisane w connector.json w paczce SDK, więc pobrany plik jest
// odrzucany, jeśli nie zgadza się z tym, co wydano razem z tą wersją SDK.
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

/** Klucz platformy w wydaniu, np. linux_amd64. */
export function platformKey(): string | undefined {
  return PLATFORMS[`${process.platform}-${process.arch}`];
}

function readManifest(): Manifest | undefined {
  const p = join(PKG_ROOT, "connector.json");
  if (!existsSync(p)) return undefined;
  return JSON.parse(readFileSync(p, "utf8")) as Manifest;
}

/** Katalogi, w których szukamy (i do których pobieramy) konektora tej wersji SDK. */
export function connectorDirs(): string[] {
  const version = readManifest()?.version ?? "dev";
  const cache = process.env.XDG_CACHE_HOME ?? join(homedir(), ".cache");
  return [join(PKG_ROOT, "bin"), join(cache, "entrypoint", version)];
}

async function download(asset: Asset, dest: string): Promise<void> {
  const res = await fetch(asset.url, { redirect: "follow" });
  if (!res.ok) throw new Error(`pobieranie ${asset.url}: HTTP ${res.status}`);
  const buf = Buffer.from(await res.arrayBuffer());
  const got = createHash("sha256").update(buf).digest("hex");
  if (got !== asset.sha256) {
    throw new Error(`błędna suma SHA256 pliku ${asset.url} (${got}, oczekiwano ${asset.sha256})`);
  }
  const tmp = `${dest}.tmp-${process.pid}`;
  await writeFile(tmp, buf, { mode: 0o755 });
  await chmod(tmp, 0o755);
  await rename(tmp, dest);
}

/**
 * Zapewnia konektor i frpc dla bieżącej platformy: zwraca ścieżkę konektora,
 * pobierając pliki z wydania, jeśli ich jeszcze nie ma. frpc trafia obok konektora.
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
    throw new Error(`brak konektora dla platformy ${process.platform}-${process.arch}`);
  }
  let lastErr: unknown;
  for (const dir of connectorDirs()) {
    try {
      await mkdir(dir, { recursive: true });
      await download(assets.frpc, join(dir, frpcExe()));
      await download(assets.connector, join(dir, connectorExe()));
      return join(dir, connectorExe());
    } catch (err) {
      lastErr = err; // np. katalog paczki tylko do odczytu: próbujemy pamięci podręcznej
    }
  }
  throw lastErr instanceof Error ? lastErr : new Error(String(lastErr));
}
