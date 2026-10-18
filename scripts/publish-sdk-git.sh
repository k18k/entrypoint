#!/usr/bin/env bash
# Publikuje zbudowane SDK (sdk/node: lib/, connector.json) do repozytorium k18k/entrypoint-ts,
# żeby działało: npm install github:k18k/entrypoint-ts#v<wersja>
# Wymaga: VERSION, DEPLOY_KEY (klucz wdrożeniowy z prawem zapisu do entrypoint-ts).
set -euo pipefail

: "${VERSION:?}" "${DEPLOY_KEY:?brak sekretu ENTRYPOINT_TS_DEPLOY_KEY}"
TARGET="${SDK_GIT_REPO:-git@github.com:k18k/entrypoint-ts.git}"
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

KEY="$WORK/deploy_key"
printf '%s\n' "$DEPLOY_KEY" > "$KEY"
chmod 600 "$KEY"
export GIT_SSH_COMMAND="ssh -i $KEY -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

git clone --quiet "$TARGET" "$WORK/repo"
cd "$WORK/repo"
git rm -rq --ignore-unmatch . >/dev/null
cp -r "$ROOT/sdk/node/lib" "$ROOT/sdk/node/connector.json" "$ROOT/sdk/node/README.md" .
mkdir -p scripts && cp "$ROOT/sdk/node/scripts/postinstall.mjs" scripts/
[ -f "$ROOT/LICENSE" ] && cp "$ROOT/LICENSE" .

# package.json do instalacji z gita: bez narzędzi deweloperskich, gotowe lib/ w repozytorium.
python3 - "$ROOT/sdk/node/package.json" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
for k in ("devDependencies", "publishConfig"):
    d.pop(k, None)
d["scripts"] = {"postinstall": d["scripts"]["postinstall"]}
d["repository"] = {"type": "git", "url": "git+https://github.com/k18k/entrypoint-ts.git"}
json.dump(d, open("package.json", "w"), indent=2, ensure_ascii=False)
open("package.json", "a").write("\n")
EOF

git add -A
git -c user.name="entrypoint release" -c user.email="noreply@github.com" \
  commit --quiet -m "SDK ${VERSION} (z k18k/entrypoint@${GITHUB_SHA:-lokalnie})" || echo "bez zmian"
git tag -f "v${VERSION}"
git push --quiet origin HEAD:master
git push --quiet --force origin "v${VERSION}"
echo "Opublikowano SDK ${VERSION} w ${TARGET}"
