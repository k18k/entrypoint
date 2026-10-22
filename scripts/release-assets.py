#!/usr/bin/env python3
"""Assembles the release files in dist/release-assets/.

  scripts/release-assets.py assets     -> dist/release-assets/ + SHA256SUMS

The URLs point to the release at https://github.com/<repo>/releases/download/v<version>/.
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
    """(platform, source file, name in the release) for the built platforms."""
    out = []
    for p in PLATFORMS:
        d = os.path.join(ROOT, "dist", "connector", p)
        if not os.path.isdir(d):
            continue
        ext = ".exe" if p.startswith("windows") else ""
        out.append((p, "connector", os.path.join(d, "entrypoint-connector" + ext), f"entrypoint-connector_{VERSION}_{p}{ext}"))
        out.append((p, "frpc", os.path.join(d, "frpc" + ext), f"frpc_{FRP}_{p}{ext}"))
    return out


def assets():
    dest = os.path.join(ROOT, "dist", "release-assets")
    shutil.rmtree(dest, ignore_errors=True)
    os.makedirs(dest)
    files = []
    for name in os.listdir(os.path.join(ROOT, "dist", "release")):
        if name.endswith(("_linux_amd64.tar.gz", "_linux_arm64.tar.gz")):
            files.append((os.path.join(ROOT, "dist", "release", name), name))
    for script in ("install.sh", "install-connector.sh"):
        files.append((os.path.join(ROOT, "scripts", script), script))
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
    {"assets": assets}[sys.argv[1]]()
