#!/usr/bin/env bash
# Runs the e2e tests in SHARDS parallel environment copies (separate compose projects,
# subnets 10.77.N.0/24 and 10.78.N.0/24). Each copy runs its share of the tests
# (assigned by e2e/durations.json). After the run it updates durations.json.
#
#   e2e/run.sh <e2e|experiments> <shards> <regex -run> [keep]
set -uo pipefail

MODE=$1
SHARDS=$2
RUN=$3
KEEP=${4:-}
REPO=$(cd "$(dirname "$0")/.." && pwd)
COMPOSE_FILE="$REPO/e2e/docker-compose.yml"
ART="$REPO/e2e/artifacts"
EXPERIMENTS=""
[ "$MODE" = experiments ] && EXPERIMENTS=1

mkdir -p "$ART"
docker volume create ep-e2e-gocache >/dev/null

shard_env() {
  export REPO SHARD=$1 NET=10.77.$1 PRIV=10.78.$1 SHARDS EXPERIMENTS
}

compose() { docker compose -f "$COMPOSE_FILE" "$@"; }

run_shard() {
  local s=$1
  shard_env "$s"
  local run="$REPO/e2e/.run/$s"
  mkdir -p "$run/zones" "$run/shared"
  rm -f "$run/zones/ep.test.db" && cp "$REPO/e2e/dns/ep.test.db" "$run/zones/ep.test.db"
  local log="$ART/$MODE-$s.log"
  {
    compose --profile restore --profile tools down --remove-orphans >/dev/null 2>&1
    compose up -d --force-recreate --remove-orphans 2>&1 | grep -v "^ Container\|^ Network\|^ Volume" || true
    compose exec -T runner go test -p 1 -count=1 -timeout 60m -v -run "$RUN" ./tests/
  } >"$log" 2>&1
  local status=$?
  compose exec -T runner chown -R "$(id -u):$(id -g)" "$ART" "$REPO/e2e/.run" >/dev/null 2>&1
  [ -n "$KEEP" ] || compose --profile restore --profile tools down --remove-orphans >/dev/null 2>&1
  echo "$status" >"$run/status"
}

start=$(date +%s)
for s in $(seq 0 $((SHARDS - 1))); do
  run_shard "$s" &
done
wait

fail=0
for s in $(seq 0 $((SHARDS - 1))); do
  st=$(cat "$REPO/e2e/.run/$s/status" 2>/dev/null || echo 1)
  log="$ART/$MODE-$s.log"
  pass=$(grep -c '^--- PASS' "$log" || true)
  failed=$(grep '^--- FAIL' "$log" || true)
  echo "shard $s: passed $pass, status $st"
  [ -n "$failed" ] && echo "$failed"
  [ "$st" = 0 ] || fail=1
done
cat "$ART"/"$MODE"-*.log >"$ART/$MODE.log"

# Test durations used for assignment in the next run.
python3 - "$REPO" "$MODE" <<'EOF'
import glob, json, os, re, sys
repo, mode = sys.argv[1], sys.argv[2]
path = os.path.join(repo, "e2e", "durations.json")
try:
    d = json.load(open(path))
except Exception:
    d = {}
for log in glob.glob(os.path.join(repo, "e2e", "artifacts", f"{mode}-*.log")):
    for m in re.finditer(r"^--- (PASS|FAIL): (\S+) \(([0-9.]+)s\)", open(log).read(), re.M):
        name, sec = m.group(2), float(m.group(3))
        kind = os.path.join(repo, "e2e", "artifacts", name, "kind.txt")
        exp = os.path.exists(kind) and open(kind).read().strip() == "experiment"
        d[name] = {"seconds": round(sec, 1), "experiment": exp}
json.dump(dict(sorted(d.items())), open(path, "w"), indent=2)
open(path, "a").write("\n")
EOF

echo "time: $(($(date +%s) - start)) s, shards: $SHARDS, log: e2e/artifacts/$MODE.log"
exit $fail
