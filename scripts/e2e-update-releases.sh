#!/bin/sh
# Builds signed test releases for the update e2e tests (dist/update/<variant>/): version 9.9.9 of
# the gateway, good or with a fault compiled in (version.TestFault), signed with a test key.
#   scripts/e2e-update-releases.sh <deps dir with caddy, frps, headscale, tailscale, tailscaled>
set -eu
DEPS="$1"
OUT=dist/update
V=9.9.9
ARCH="$(go env GOARCH)"
mkdir -p "$OUT"
[ -f "$OUT/test.pem" ] || openssl genpkey -algorithm ed25519 -out "$OUT/test.pem" 2>/dev/null
openssl pkey -in "$OUT/test.pem" -pubout -outform DER | tail -c 32 | base64 > "$OUT/test.pub"
for variant in good:"" migration:migration health:health; do
  name="${variant%%:*}"; fault="${variant#*:}"
  d="$OUT/$name"; pkg="entrypoint_${V}_linux_${ARCH}"
  rm -rf "$d"; mkdir -p "$d/$pkg/bin"
  (cd gateway && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/k18k/entrypoint/gateway/internal/version.Version=$V -X github.com/k18k/entrypoint/gateway/internal/version.TestFault=$fault" \
    -o "../$d/$pkg/bin/" ./cmd/entrypoint ./cmd/entrypointd ./cmd/entrypoint-panel)
  for b in caddy frps headscale tailscale tailscaled; do cp "$DEPS/$b" "$d/$pkg/bin/"; done
  cp versions.json "$d/$pkg/"; echo "$V" > "$d/$pkg/VERSION"
  tar -C "$d" -cf - "$pkg" | gzip -1 > "$d/$pkg.tar.gz"
  rm -rf "$d/$pkg"
  (cd "$d" && sha256sum "$pkg.tar.gz" > SHA256SUMS)
  openssl pkeyutl -sign -inkey "$OUT/test.pem" -rawin -in "$d/SHA256SUMS" | base64 -w0 > "$d/SHA256SUMS.sig"
done
echo "$OUT: good migration health"
