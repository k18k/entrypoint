#!/bin/sh
# Instalator programów Entrypoint: pobiera paczkę wydania, weryfikuje SHA256
# i rozpakowuje ją do /opt/entrypoint/releases/<wersja>. Konfigurację bramy
# zakłada potem: sudo entrypoint install --gateway-name <nazwa> --email <e-mail>
#
#   curl -fsSL <adres>/install.sh | sudo sh
#   sudo ENTRYPOINT_TARBALL=./entrypoint_0.1.0_linux_amd64.tar.gz sh install.sh
set -eu

VERSION="${ENTRYPOINT_VERSION:-0.1.0}"
REPO="${ENTRYPOINT_REPO:-k18k/entrypoint}"
TARBALL="${ENTRYPOINT_TARBALL:-}"
OPT=/opt/entrypoint

die() { echo "install.sh: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "uruchom jako root (sudo)"
[ -d /run/systemd/system ] || die "wspierany jest wyłącznie system z systemd"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "nieobsługiwana architektura $(uname -m)" ;;
esac
NAME="entrypoint_${VERSION}_linux_${ARCH}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if [ -z "$TARBALL" ]; then
  command -v curl >/dev/null 2>&1 || die "brak curl"
  BASE="https://github.com/${REPO}/releases/download/v${VERSION}"
  echo "Pobieram ${NAME}.tar.gz..."
  curl -fsSL -o "$TMP/$NAME.tar.gz" "$BASE/$NAME.tar.gz"
  curl -fsSL -o "$TMP/$NAME.tar.gz.sha256" "$BASE/$NAME.tar.gz.sha256"
  TARBALL="$TMP/$NAME.tar.gz"
  SUMFILE="$TMP/$NAME.tar.gz.sha256"
else
  [ -f "$TARBALL" ] || die "brak pliku $TARBALL"
  SUMFILE="${ENTRYPOINT_SHA256_FILE:-$TARBALL.sha256}"
fi

if [ -f "$SUMFILE" ]; then
  want="$(cut -d' ' -f1 < "$SUMFILE")"
  got="$(sha256sum "$TARBALL" | cut -d' ' -f1)"
  [ "$want" = "$got" ] || die "błędna suma SHA256 paczki ($got, oczekiwano $want)"
  echo "Suma SHA256 paczki zgodna."
else
  die "brak pliku sumy kontrolnej $SUMFILE"
fi

mkdir -p "$TMP/x"
tar -xzf "$TARBALL" -C "$TMP/x"
[ -x "$TMP/x/$NAME/bin/entrypoint" ] || die "paczka nie zawiera bin/entrypoint"
got_version="$(cat "$TMP/x/$NAME/VERSION")"
[ "$got_version" = "$VERSION" ] || die "paczka zawiera wersję $got_version, oczekiwano $VERSION"

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

echo "Zainstalowano Entrypoint $VERSION w $DEST."
echo "Dalej: sudo entrypoint install --gateway-name tunnel.example.com --email admin@example.com"
