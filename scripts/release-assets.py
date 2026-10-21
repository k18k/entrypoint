#!/usr/bin/env python3
"""Składa pliki wydania w dist/release-assets/ i manifest konektora SDK.

  scripts/release-assets.py manifest   -> sdk/node/connector.json (adresy i SHA256 binarek)
  scripts/release-assets.py assets     -> dist/release-assets/ + SHA256SUMS

Adresy wskazują wydanie https://github.com/<repo>/releases/download/v<wersja>/.
"""
import hashlib
import json
import os
import shutil
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO = os.environ.get("ENTRYPOINT_REPO", "k18k/entrypoint")
PLATFORMS = ["linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64", "windows_amd64"]

versions = json.load(open(os.path.join(ROOT, "versions.json")))
VERSION = versions["entrypoint"]
FRP = versions["frp"]["version"]
BASE = f"https://github.com/{REPO}/releases/download/v{VERSION}"


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def binaries():
    """(platforma, plik źródłowy, nazwa w wydaniu) dla zbudowanych platform."""
    out = []
    for p in PLATFORMS:
        d = os.path.join(ROOT, "dist", "connector", p)
        if not os.path.isdir(d):
            continue
        ext = ".exe" if p.startswith("windows") else ""
        out.append((p, "connector", os.path.join(d, "entrypoint-connector" + ext), f"entrypoint-connector_{VERSION}_{p}{ext}"))
        out.append((p, "frpc", os.path.join(d, "frpc" + ext), f"frpc_{FRP}_{p}{ext}"))
    return out


def manifest():
    plats = {}
    for p, kind, src, name in binaries():
        plats.setdefault(p, {})[kind] = {"url": f"{BASE}/{name}", "sha256": sha256(src)}
    out = os.path.join(ROOT, "sdk", "node", "connector.json")
    json.dump({"version": VERSION, "frp": FRP, "platforms": plats}, open(out, "w"), indent=2)
    open(out, "a").write("\n")
    print(f"{out}: {', '.join(sorted(plats))}")


def assets():
    dest = os.path.join(ROOT, "dist", "release-assets")
    shutil.rmtree(dest, ignore_errors=True)
    os.makedirs(dest)
    files = []
    for name in os.listdir(os.path.join(ROOT, "dist", "release")):
        # Brama tylko dla linux_amd64 (ARM64 wymaga testów w v0.2.0).
        if "_linux_amd64.tar.gz" in name:
            files.append((os.path.join(ROOT, "dist", "release", name), name))
    files.append((os.path.join(ROOT, "scripts", "install.sh"), "install.sh"))
    for _, _, src, name in binaries():
        files.append((src, name))
    sums = []
    for src, name in files:
        shutil.copy2(src, os.path.join(dest, name))
        if not name.endswith(".sha256"):
            sums.append(f"{sha256(src)}  {name}")
    open(os.path.join(dest, "SHA256SUMS"), "w").write("\n".join(sorted(sums, key=lambda l: l.split()[1])) + "\n")
    for f in sorted(os.listdir(dest)):
        print(f"dist/release-assets/{f}")


if __name__ == "__main__":
    {"manifest": manifest, "assets": assets}[sys.argv[1]]()
