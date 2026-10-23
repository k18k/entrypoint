#!/bin/sh
# Installs entrypoint-connector and frpc into /usr/local/bin (Linux, SHA256-checked) and, given
# connector arguments, runs them as a systemd service that starts at boot.
#
#   curl -fsSLO https://raw.githubusercontent.com/k18k/entrypoint/master/scripts/install-connector.sh
#   sudo sh install-connector.sh                                          # programs only
#   sudo sh install-connector.sh vpn --name vatro-postgres --expose 5432  # + service, asks for ENTRYPOINT_VPN
#   sudo sh install-connector.sh tunnel --upstream http://127.0.0.1:3000  # + service, asks for ENTRYPOINT_CONNECT
#
# The bundle is read from the terminal (or ENTRYPOINT_VPN / ENTRYPOINT_CONNECT) and stored in
# /etc/entrypoint-connector/<service>.env (0600), never on a command line.
# For `vpn --forward <name>:<port>` the local end gets its own loopback address (127.77.x.y, from
# the name, so it never collides with a local service on the same port) and an /etc/hosts entry
# makes <name> resolve to it.
set -eu

VERSION="${ENTRYPOINT_VERSION:-0.2.0}"
REPO="${ENTRYPOINT_REPO:-k18k/entrypoint}"
FILES="${ENTRYPOINT_CONNECTOR_FILES:-}" # tests: a directory with the release files instead of GitHub
BIN=/usr/local/bin
ETC=/etc/entrypoint-connector

die() { echo "install-connector.sh: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fetch() {
  if [ -n "$FILES" ]; then
    cp "$FILES/$1" "$TMP/$1"
  else
    curl -fsSL -o "$TMP/$1" "https://github.com/${REPO}/releases/download/v${VERSION}/$1"
  fi
}

fetch SHA256SUMS
for f in "entrypoint-connector_${VERSION}_linux_${ARCH}" $(grep -o "frpc_[^ ]*_linux_${ARCH}\$" "$TMP/SHA256SUMS" | head -1); do
  fetch "$f"
  want="$(grep " \*\{0,1\}$f\$" "$TMP/SHA256SUMS" | cut -d' ' -f1)"
  got="$(sha256sum "$TMP/$f" | cut -d' ' -f1)"
  [ -n "$want" ] && [ "$want" = "$got" ] || die "wrong SHA256 of $f"
  case "$f" in
    entrypoint-connector_*) install -m 0755 "$TMP/$f" "$BIN/entrypoint-connector" ;;
    frpc_*) install -m 0755 "$TMP/$f" "$BIN/frpc" ;;
  esac
done
echo "Installed $BIN/entrypoint-connector $VERSION and $BIN/frpc."

[ $# -gt 0 ] || exit 0
MODE="$1"
case "$MODE" in
  vpn) VAR=ENTRYPOINT_VPN ;;
  tunnel) VAR=ENTRYPOINT_CONNECT ;;
  *) die "the first argument is vpn or tunnel" ;;
esac
[ -d /run/systemd/system ] || die "a service needs systemd; run the connector yourself instead"

# Service name: vpn -> --name, tunnel -> --service or the bundle's service.
NAME=""
prev=""
for a in "$@"; do
  case "$prev" in --name|--service) NAME="$a" ;; esac
  case "$a" in --name=*|--service=*) NAME="${a#*=}" ;; esac
  prev="$a"
done
[ -n "$NAME" ] || NAME="$MODE"
case "$NAME" in *[!a-z0-9-]*|"") die "invalid name $NAME (lowercase letters, digits and dashes)" ;; esac
UNIT="entrypoint-connector-$MODE-$NAME"

eval "BUNDLE=\${$VAR:-}"
if [ -z "$BUNDLE" ]; then
  [ -r /dev/tty ] || die "set $VAR"
  printf "Paste %s (from the panel): " "$VAR" > /dev/tty
  stty -echo < /dev/tty 2>/dev/null || true
  IFS= read -r BUNDLE < /dev/tty
  stty echo < /dev/tty 2>/dev/null || true
  echo > /dev/tty
fi
[ -n "$BUNDLE" ] || die "empty $VAR"

id entrypoint-connector >/dev/null 2>&1 || useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin entrypoint-connector 2>/dev/null || useradd --system --user-group --no-create-home --shell /bin/false entrypoint-connector
mkdir -p "$ETC"
chmod 0755 "$ETC"
umask 077
printf '%s=%s\n' "$VAR" "$BUNDLE" > "$ETC/$NAME-$MODE.env"
chmod 0600 "$ETC/$NAME-$MODE.env"

# vpn --forward <name>:<port> -> 127.77.x.y:<port>=<name>:<port>, and <name> in /etc/hosts.
loopback() {
  n="$(printf %s "$1" | cksum | cut -d' ' -f1)"
  echo "127.77.$((n / 256 % 256)).$((n % 254 + 1))"
}
HOSTS=""
if [ "$MODE" = vpn ]; then
  out=""
  prev=""
  for a in "$@"; do
    spec=""
    case "$prev" in --forward) spec="$a" ;; esac
    if [ -n "$spec" ]; then
      case "$spec" in
        *=*) listen="${spec%%=*}"; name="${spec#*=}"; name="${name%:*}"; HOSTS="$HOSTS ${listen%:*}/$name" ;;
        *) name="${spec%:*}"; ip="$(loopback "$name")"; HOSTS="$HOSTS $ip/$name"; a="$ip:${spec##*:}=$spec" ;;
      esac
    fi
    out="$out $a"
    prev="$a"
  done
  # shellcheck disable=SC2086
  set -- $out
fi

ARGS=""
for a in "$@"; do
  case "$a" in *[!A-Za-z0-9_./:=@,+-]*) die "unsupported character in argument: $a" ;; esac
  ARGS="$ARGS $a"
done
EXTRA=""
[ "$MODE" = vpn ] && EXTRA=" --state-dir /var/lib/$UNIT"

cat > "/etc/systemd/system/$UNIT.service" <<EOF
[Unit]
Description=Entrypoint connector ($MODE $NAME)
After=network-online.target
Wants=network-online.target

[Service]
User=entrypoint-connector
Group=entrypoint-connector
EnvironmentFile=$ETC/$NAME-$MODE.env
ExecStart=$BIN/entrypoint-connector$ARGS$EXTRA
StateDirectory=$UNIT
StateDirectoryMode=0700
Restart=always
RestartSec=2
KillSignal=SIGTERM
TimeoutStopSec=45
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF

for h in $HOSTS; do
  ip="${h%%/*}"
  name="${h#*/}"
  case "$name" in *[!a-z0-9.-]*|"") continue ;; esac
  case "$ip" in *[!0-9.]*|"") continue ;; esac
  # Rewritten in place: /etc/hosts may be a mount point (containers).
  grep -vE "^[0-9.]+[[:space:]]+${name}([[:space:]]|\$)" /etc/hosts > "$TMP/hosts" || true
  cat "$TMP/hosts" > /etc/hosts
  printf '%s %s # entrypoint-connector %s\n' "$ip" "$name" "$UNIT" >> /etc/hosts
  echo "Added /etc/hosts: $ip $name"
done

systemctl daemon-reload
systemctl enable --now "$UNIT.service" >/dev/null
echo "Service $UNIT is running. Logs: journalctl -u $UNIT -f"
