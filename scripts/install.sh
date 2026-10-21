#!/bin/sh
# Entrypoint installer: downloads the release tarball, verifies its SHA256
# and unpacks it into /opt/entrypoint/releases/<version>. The gateway configuration
# is created afterwards: sudo entrypoint install --gateway-name <name> --email <e-mail>
#
#   curl -fsSL <url>/install.sh | sudo sh
#   sudo ENTRYPOINT_TARBALL=./entrypoint_0.1.1_linux_amd64.tar.gz sh install.sh
set -eu

VERSION="${ENTRYPOINT_VERSION:-0.1.1}"
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

if [ -z "$TARBALL" ]; then
  command -v curl >/dev/null 2>&1 || die "curl is missing"
  BASE="https://github.com/${REPO}/releases/download/v${VERSION}"
  echo "Downloading ${NAME}.tar.gz..."
  curl -fsSL -o "$TMP/$NAME.tar.gz" "$BASE/$NAME.tar.gz"
  curl -fsSL -o "$TMP/$NAME.tar.gz.sha256" "$BASE/$NAME.tar.gz.sha256"
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
echo "Dalej: sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com"
