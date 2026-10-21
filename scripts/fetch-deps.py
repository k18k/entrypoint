#!/usr/bin/env python3
"""Downloads the pinned Caddy and frp binaries from versions.json, verifies SHA256
and unpacks them into dist/deps/<os>_<arch>/.

Usage: scripts/fetch-deps.py [platform ...]   (default: linux_amd64)
"""
import hashlib
import io
import json
import os
import sys
import tarfile
import urllib.request
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CACHE = os.path.join(ROOT, "dist", "cache")
DEPS = os.path.join(ROOT, "dist", "deps")

# Which files to extract from each component's archive.
WANTED = {"caddy": ["caddy"], "frp": ["frps", "frpc"]}


def fetch(url, sha256):
    # Only official release downloads over HTTPS; integrity is checked by SHA256 below.
    if not url.startswith("https://github.com/"):
        sys.exit(f"download URL not allowed: {url}")
    os.makedirs(CACHE, exist_ok=True)
    path = os.path.join(CACHE, url.rsplit("/", 1)[1])
    if os.path.exists(path):
        with open(path, "rb") as f:
            data = f.read()
        if hashlib.sha256(data).hexdigest() == sha256:
            return data
    with urllib.request.urlopen(url, timeout=120) as r:
        data = r.read()
    got = hashlib.sha256(data).hexdigest()
    if got != sha256:
        sys.exit(f"wrong SHA256 for {url}: {got}, expected {sha256}")
    with open(path, "wb") as f:
        f.write(data)
    return data


def extract(data, name, wanted, dest):
    os.makedirs(dest, exist_ok=True)
    found = set()
    if name.endswith(".zip"):
        with zipfile.ZipFile(io.BytesIO(data)) as z:
            for info in z.infolist():
                base = os.path.basename(info.filename)
                stem = base[:-4] if base.endswith(".exe") else base
                if stem in wanted:
                    with open(os.path.join(dest, base), "wb") as f:
                        f.write(z.read(info))
                    found.add(stem)
    else:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as t:
            for m in t.getmembers():
                base = os.path.basename(m.name)
                if m.isfile() and base in wanted:
                    out = os.path.join(dest, base)
                    with open(out, "wb") as f:
                        f.write(t.extractfile(m).read())
                    os.chmod(out, 0o755)
                    found.add(base)
    missing = set(wanted) - found
    if missing:
        sys.exit(f"{name} is missing files: {', '.join(sorted(missing))}")


def main():
    with open(os.path.join(ROOT, "versions.json")) as f:
        versions = json.load(f)
    platforms = sys.argv[1:] or ["linux_amd64"]
    for plat in platforms:
        dest = os.path.join(DEPS, plat)
        for comp, wanted in WANTED.items():
            asset = versions[comp]["assets"].get(plat)
            if asset is None:
                if comp == "caddy" and not plat.startswith("linux"):
                    continue  # Caddy is only needed on the gateway (Linux)
                sys.exit(f"no {comp} asset for {plat} in versions.json")
            data = fetch(asset["url"], asset["sha256"])
            extract(data, asset["url"].rsplit("/", 1)[1], wanted, dest)
        print(f"{plat}: {', '.join(sorted(os.listdir(dest)))}")


if __name__ == "__main__":
    main()
