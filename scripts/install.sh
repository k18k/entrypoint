#!/bin/sh
# Entrypoint installer: downloads the release tarball, verifies its SHA256
# and unpacks it into /opt/entrypoint/releases/<version>. The gateway configuration
# is created afterwards: sudo entrypoint install --gateway-name <name> --email <e-mail>
#
#   curl -fsSL <url>/install.sh | sudo sh
#   sudo ENTRYPOINT_TARBALL=./entrypoint_0.1.1_linux_amd64.tar.gz sh install.sh
set -eu

VERSION="${ENTRYPOINT_VERSION:-0.2.0}"
REPO="${ENTRYPOINT_REPO:-k18k/entrypoint}"
TARBALL="${ENTRYPOINT_TARBALL:-}"
OPT=/opt/entrypoint

die() { echo "install.sh: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
[ -d /run/systemd/system ] || die "only systemd-based systems are supported"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
NAME="entrypoint_${VERSION}_linux_${ARCH}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Official release signing key (Ed25519); entrypoint update checks the same key.
RELEASE_KEY="-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA8reBNWMRtX615Ph1b1Z0LXRD9jvCjqgO4gOFq+N8eqk=
-----END PUBLIC KEY-----"

if [ -z "$TARBALL" ]; then
  command -v curl >/dev/null 2>&1 || die "curl is missing"
  command -v openssl >/dev/null 2>&1 || die "openssl is missing (needed to verify the release signature)"
  BASE="https://github.com/${REPO}/releases/download/v${VERSION}"
  echo "Downloading ${NAME}.tar.gz..."
  curl -fsSL -o "$TMP/$NAME.tar.gz" "$BASE/$NAME.tar.gz"
  curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"
  curl -fsSL -o "$TMP/SHA256SUMS.sig" "$BASE/SHA256SUMS.sig"
  printf '%s\n' "$RELEASE_KEY" > "$TMP/release.pub"
  base64 -d < "$TMP/SHA256SUMS.sig" > "$TMP/SHA256SUMS.raw" 2>/dev/null || die "SHA256SUMS.sig is not base64"
  openssl pkeyutl -verify -pubin -inkey "$TMP/release.pub" -rawin -in "$TMP/SHA256SUMS" -sigfile "$TMP/SHA256SUMS.raw" >/dev/null 2>&1 \
    || die "the release signature does not match the Entrypoint release key"
  echo "Release signature verified."
  grep " \*\{0,1\}$NAME.tar.gz\$" "$TMP/SHA256SUMS" > "$TMP/$NAME.tar.gz.sha256" || die "$NAME.tar.gz is not in SHA256SUMS"
  TARBALL="$TMP/$NAME.tar.gz"
  SUMFILE="$TMP/$NAME.tar.gz.sha256"
else
  [ -f "$TARBALL" ] || die "file $TARBALL not found"
  SUMFILE="${ENTRYPOINT_SHA256_FILE:-$TARBALL.sha256}"
fi

if [ -f "$SUMFILE" ]; then
  want="$(cut -d' ' -f1 < "$SUMFILE")"
  got="$(sha256sum "$TARBALL" | cut -d' ' -f1)"
  [ "$want" = "$got" ] || die "wrong SHA256 of the tarball ($got, expected $want)"
  echo "Tarball SHA256 matches."
else
  die "checksum file $SUMFILE not found"
fi

mkdir -p "$TMP/x"
tar -xzf "$TARBALL" -C "$TMP/x"
[ -f "$TMP/x/$NAME/bin/entrypoint" ] || die "tarball does not contain bin/entrypoint"
got_version="$(cat "$TMP/x/$NAME/VERSION")"
[ "$got_version" = "$VERSION" ] || die "tarball contains version $got_version, expected $VERSION"

# Per-IP connection limits are nftables rules; cloud images often lack the tool.
if ! command -v nft >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
  echo "Installing nftables (per-IP connection limits)..."
  { apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nftables; } >/dev/null 2>&1 \
    || echo "warning: could not install nftables; per-IP connection limits stay off"
fi

mkdir -p "$OPT/releases"
DEST="$OPT/releases/$VERSION"
rm -rf "$DEST.new"
mv "$TMP/x/$NAME" "$DEST.new"
chown -R root:root "$DEST.new"
chmod -R go-w "$DEST.new"
rm -rf "$DEST"
mv "$DEST.new" "$DEST"
ln -sfn "releases/$VERSION" "$OPT/current.new"
mv -T "$OPT/current.new" "$OPT/current"
ln -sfn "$OPT/current/bin/entrypoint" /usr/local/bin/entrypoint

echo "Installed Entrypoint $VERSION in $DEST."
echo "Next: sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com"
