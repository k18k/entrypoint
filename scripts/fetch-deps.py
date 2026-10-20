#!/usr/bin/env python3
"""Pobiera przypięte binarki Caddy i frp z versions.json, weryfikuje SHA256
i rozpakowuje je do dist/deps/<os>_<arch>/.

Użycie: scripts/fetch-deps.py [platforma ...]   (domyślnie linux_amd64)
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

# Które pliki wyciągamy z archiwum danego komponentu.
WANTED = {"caddy": ["caddy"], "frp": ["frps", "frpc"]}


def fetch(url, sha256):
    # Only official release downloads over HTTPS; integrity is checked by SHA256 below.
    if not url.startswith("https://github.com/"):
        sys.exit(f"niedozwolony adres pobierania: {url}")
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
        sys.exit(f"błędna suma SHA256 dla {url}: {got}, oczekiwano {sha256}")
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
        sys.exit(f"w {name} brakuje plików: {', '.join(sorted(missing))}")


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
                    continue  # Caddy potrzebny tylko na bramie (Linux)
                sys.exit(f"brak {comp} dla {plat} w versions.json")
            data = fetch(asset["url"], asset["sha256"])
            extract(data, asset["url"].rsplit("/", 1)[1], wanted, dest)
        print(f"{plat}: {', '.join(sorted(os.listdir(dest)))}")


if __name__ == "__main__":
    main()
