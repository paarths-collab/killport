#!/bin/sh
# End-to-end demo of the killport CLI on Linux, inside ONE throwaway
# `docker run --rm` of golang:1.25. Nothing runs on the host except Docker.
#
#   sh scripts/demo-run.sh
#
# The transcript goes to $EVIDENCE_DIR/linux-run.txt (default docs/evidence/)
# and the key JSON documents to $EVIDENCE_DIR/{plan,apply,apply-drift,
# inspect,check,legacy-kill,error}.json. The repo is mounted read-only.
# Not covered: the Docker-container branch (no Docker socket is mounted).
set -eu

REPO=$(cd "$(dirname "$0")/.." && pwd)
EVIDENCE="${EVIDENCE_DIR:-$REPO/docs/evidence}"

if ! command -v docker >/dev/null 2>&1; then
  echo "demo-run.sh: docker is required" >&2
  exit 1
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK" 2>/dev/null || true' EXIT
mkdir -p "$WORK/out/json" "$EVIDENCE"

# The in-container script. Quoted heredoc: nothing expands on the host.
cat >"$WORK/inner.sh" <<'INNER'
#!/bin/sh
# Runs as root inside golang:1.25. Outputs go to /work (bind mount);
# FIFOs and /tmp files stay in the container filesystem.
OUT=/work/out
JSON=$OUT/json
mkdir -p "$JSON"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq lsof iproute2 netcat-openbsd python3 procps >/dev/null 2>&1 \
  || { echo "apt-get install failed"; exit 1; }

echo "== build: go build from read-only /src"
cd /src && go build -o /tmp/killport . || { echo "go build failed"; exit 1; }
install -m 0755 /tmp/killport /usr/local/bin/killport
go version

# run NAME CMD...  : stdout -> $OUT/NAME.out, stderr -> $OUT/NAME.err,
# prints the command, the stdout, any stderr, then exit=<code>.
run() {
  name=$1; shift
  printf '\n$ %s\n' "$*"
  "$@" >"$OUT/$name.out" 2>"$OUT/$name.err"
  rc=$?
  cat "$OUT/$name.out"
  if [ -s "$OUT/$name.err" ]; then printf '[stderr]\n'; cat "$OUT/$name.err"; fi
  printf 'exit=%s\n' "$rc"
}

# state PID: ALIVE, or DEAD. A killed child of this shell stays a zombie
# until reaped, so kill -0 alone would wrongly report it alive.
state() {
  if kill -0 "$1" 2>/dev/null; then
    st=$(sed -n 's/^State:[[:space:]]*//p' "/proc/$1/status" 2>/dev/null)
    case "$st" in
      Z*) echo "DEAD (zombie)" ;;
      *)  echo "ALIVE" ;;
    esac
  else
    echo "DEAD"
  fi
}

# liveness LABEL PID: one line per fixture.
liveness() {
  if kill -0 "$2" 2>/dev/null; then ks=ok; else ks=fail; fi
  printf '  %-24s pid=%-7s kill-0=%-4s -> %s\n' "$1" "$2" "$ks" "$(state "$2")"
}

# table TITLE: liveness of every fixture still known.
table() {
  printf '\n-- fixture liveness (%s):\n' "$1"
  liveness "server :3000 (target)" "$PID_SRV3000"
  liveness "server :3001 (other)" "$PID_SRV3001"
  liveness "nc client -> :3000" "$PID_CLIENT"
  liveness "udp listener :5353" "$PID_UDP"
  if [ -n "${PID_SRV3002A:-}" ]; then liveness "server :3002 (first)" "$PID_SRV3002A"; fi
  if [ -n "${PID_SRV3002B:-}" ]; then liveness "server :3002 (second)" "$PID_SRV3002B"; fi
}

echo
echo "== 1. environment"
printf '\n$ killport --version\n'; killport --version
printf '\n$ uname -sm\n';         uname -sm
printf '\n$ lsof -v (first lines)\n'; lsof -v 2>&1 | head -n 3
printf '\n$ ss -V\n';            ss -V
printf '\n$ id -un\n';           id -un

echo
echo "== 2. fixtures"
python3 -m http.server 3000 --bind 0.0.0.0 >/dev/null 2>&1 &
PID_SRV3000=$!
python3 -m http.server 3001 >/dev/null 2>&1 &
PID_SRV3001=$!
sleep 1
# Held client: nc connects to 127.0.0.1:3000, so its REMOTE port is 3000.
# Its stdin is a FIFO kept open by a sleep, so the connection stays up.
mkfifo /tmp/hold-client /tmp/hold-udp
(exec sleep 600 >/tmp/hold-client) &
nc 127.0.0.1 3000 </tmp/hold-client >/dev/null 2>&1 &
PID_CLIENT=$!
(exec sleep 600 >/tmp/hold-udp) &
nc -u -l 5353 </tmp/hold-udp >/dev/null 2>&1 &
PID_UDP=$!
sleep 1
echo "fixture pids: srv3000=$PID_SRV3000 srv3001=$PID_SRV3001 nc-client=$PID_CLIENT udp5353=$PID_UDP"
printf '\n$ ss -tanp | grep 300[01]   (TCP sockets)\n'
ss -tanp 2>/dev/null | grep -E ':300[01]\b' || true
printf '\n$ ss -uanp | grep 5353   (UDP socket)\n'
ss -uanp 2>/dev/null | grep ':5353' || true
table "after start"

echo
echo "== 3. read-only: check, dry-run, inspect, plan"
run check_all killport --check
run check_3000 killport --check 3000 --json
run dry_3000 killport 3000 --dry-run
run dry_3000_json killport 3000 --dry-run --json
run inspect_3000 killport inspect 3000 --json
run plan_3000_3001 killport plan 3000 3001 -o /tmp/plan.json --json
printf '\n$ cat /tmp/plan.json\n'
cat /tmp/plan.json
cp /tmp/plan.json "$JSON/plan.json"
table "after plan and dry-run, must be unchanged"

echo
echo "== 4. apply the plan"
run apply_plan killport apply /tmp/plan.json --json
table "after apply"
run check_after killport --check 3000 --json

echo
echo "== 4b. drift: plan a fresh server, replace it, then apply"
python3 -m http.server 3002 --bind 0.0.0.0 >/dev/null 2>&1 &
PID_SRV3002A=$!
sleep 1
run plan_3002 killport plan 3002 -o /tmp/p2.json
kill "$PID_SRV3002A"
sleep 1
python3 -m http.server 3002 --bind 0.0.0.0 >/dev/null 2>&1 &
PID_SRV3002B=$!
sleep 1
printf '\n$ ss -tlnp | grep 3002   (after replacing the server)\n'
ss -tlnp 2>/dev/null | grep ':3002' || true
run apply_drift killport apply /tmp/p2.json --json
table "after drift apply"

echo
echo "== 4c. legacy kill and error handling"
run legacy_3002 killport 3002 --json
table "after legacy kill"
run err_range killport 99999999
run err_abc killport abc --json
run err_version killport --json --version

cp "$OUT/apply_plan.out" "$JSON/apply.json"
cp "$OUT/apply_drift.out" "$JSON/apply-drift.json"
cp "$OUT/inspect_3000.out" "$JSON/inspect.json"
cp "$OUT/check_3000.out" "$JSON/check.json"
cp "$OUT/legacy_3002.out" "$JSON/legacy-kill.json"
cp "$OUT/err_abc.out" "$JSON/error.json"

echo
echo "== 5. cleanup: kill the fixture pids this script started"
for p in "$PID_CLIENT" "$PID_UDP" "$PID_SRV3000" "$PID_SRV3001" "$PID_SRV3002A" "$PID_SRV3002B"; do
  kill "$p" 2>/dev/null || true
done
sleep 1
table "final"
echo
echo "== done"
INNER

# Start the container. The repo is read-only; only $WORK is writable.
# Named volumes hold the Go module and build caches.
docker run --rm --memory 1g --hostname killport-demo \
  -v "$REPO":/src:ro \
  -v killport-gomod:/go/pkg/mod \
  -v killport-gocache:/root/.cache/go-build \
  -v "$WORK":/work \
  -e GOFLAGS=-buildvcs=false \
  golang:1.25 sh /work/inner.sh 2>&1 | tee "$EVIDENCE/linux-run.txt"

cp "$WORK"/out/json/*.json "$EVIDENCE/"
echo "evidence written to $EVIDENCE"
