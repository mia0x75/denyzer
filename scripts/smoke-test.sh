#!/usr/bin/env bash
# End-to-end smoke test for a Denyzer release binary.
#
# The release binaries are built with CGO because the query log is stored in
# SQLite through the native driver (github.com/mattn/go-sqlite3). A binary
# that was built without CGO still starts, but the query log silently falls
# back to the console writer, so a plain "does it start" check is not enough.
#
# This script proves that the binary really talks to SQLite:
#
#   1. it starts the server with a query log target,
#   2. it waits for the HTTP API to come up,
#   3. it resolves a name that is answered from the custom DNS mapping, so no
#      external network is needed,
#   4. it reads the query log back through the API and fails when the entry
#      does not show up.
#
# Usage: scripts/smoke-test.sh <path-to-denyzer-binary>
set -euo pipefail

BIN="${1:-}"
if [ -z "$BIN" ]; then
  echo "usage: $0 <path-to-denyzer-binary>" >&2
  exit 2
fi

case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN*) OS=windows ;;
  *) OS=unix ;;
esac

# Windows has no executable bit, so only the file itself is checked there.
if [ "$OS" = "windows" ]; then
  if [ ! -f "$BIN" ]; then
    echo "smoke-test: $BIN is not a file" >&2
    exit 2
  fi
else
  if [ ! -x "$BIN" ]; then
    echo "smoke-test: $BIN is not an executable file" >&2
    exit 2
  fi
fi

HTTP_PORT=${SMOKE_HTTP_PORT:-14004}
DNS_PORT=${SMOKE_DNS_PORT:-15053}

WORKDIR=$(mktemp -d)
SERVER_PID=""
SERVER_LOG="$WORKDIR/server.log"
BIN_NAME="$(basename "$BIN")"

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    if [ "$OS" = "windows" ]; then
      # Git Bash reports an MSYS PID, which is not the PID Windows knows the
      # process by, so the process is stopped by its image name instead.
      taskkill //IM "$BIN_NAME" //T //F >/dev/null 2>&1 || true
    else
      kill "$SERVER_PID" >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

DB="$WORKDIR/denyzer.db"
CONFIG="$WORKDIR/config.yml"

# Native Windows binaries do not understand the MSYS paths, so hand them the
# Windows spelling of the same location.
if [ "$OS" = "windows" ] && command -v cygpath >/dev/null 2>&1; then
  DB=$(cygpath -m "$DB")
  CONFIG=$(cygpath -m "$CONFIG")
fi

# The upstream is never contacted: the custom DNS mapping answers the smoke
# query, but the resolver chain refuses to start without an upstream group.
cat >"$CONFIG" <<EOF
log:
  level: info
queryLog:
  type: sqlite
  target: '$DB'
  flushInterval: 1s
  batchSize: 1
ports:
  dns: $DNS_PORT
  http: $HTTP_PORT
  https: 15443
  tls: 15853
upstreams:
  groups:
    default:
      - 127.0.0.1
  strategy: parallel_best
customDNS:
  mapping:
    smoke.test: 192.0.2.1
EOF

echo "smoke-test: starting $BIN"
"$BIN" serve --config "$CONFIG" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

fail() {
  echo "smoke-test: FAILED - $1" >&2
  echo "--- server log ---" >&2
  cat "$SERVER_LOG" >&2 || true
  exit 1
}

# Git Bash cannot use "kill -0" on a native Windows process, and its PID is
# not the PID Windows uses, so the process list is queried by image name.
server_running() {
  if [ "$OS" = "windows" ]; then
    tasklist //FI "IMAGENAME eq $BIN_NAME" //NH 2>/dev/null | grep -qi "$BIN_NAME"
  else
    kill -0 "$SERVER_PID" 2>/dev/null
  fi
}

# Wait for the HTTP API. A binary built without CGO starts as well, but its
# query log falls back to the console, so the server log is checked for that
# explicitly instead of relying on the API alone.
API="http://127.0.0.1:$HTTP_PORT/api/logs/query?limit=10"
READY=0
for _ in $(seq 1 60); do
  if grep -q "requires cgo to work" "$SERVER_LOG"; then
    fail "the binary was built without CGO, the SQLite query log is a stub"
  fi

  if grep -q "using console as fallback" "$SERVER_LOG"; then
    fail "the query log writer fell back to the console, SQLite is not usable"
  fi

  if curl -fsS "$API" >/dev/null 2>&1; then
    READY=1
    break
  fi

  if ! server_running; then
    fail "the server exited before the API became available"
  fi
  sleep 1
done

if [ "$READY" != 1 ]; then
  fail "the HTTP API did not become available on port $HTTP_PORT"
fi

# A DNS query for smoke.test, encoded as a DoH GET request. The wire format
# is a standard query for smoke.test/A with the recursion-desired flag set.
DNS_QUERY=$(
  printf '\000\000\001\000\000\001\000\000\000\000\000\000\005smoke\004test\000\000\001\000\001' |
    base64 | tr '+/' '-_' | tr -d '=\n'
)

DOH="http://127.0.0.1:$HTTP_PORT/dns-query?dns=$DNS_QUERY"
if ! curl -fsS "$DOH" -H 'accept: application/dns-message' -o "$WORKDIR/answer.bin"; then
  fail "the DoH endpoint did not answer the smoke query"
fi

# The entry is flushed to SQLite on the next flush interval.
FOUND=0
for _ in $(seq 1 30); do
  BODY=$(curl -fsS "$API" 2>/dev/null || true)
  case "$BODY" in
    *smoke.test*)
      FOUND=1
      break
      ;;
  esac
  sleep 1
done

if [ "$FOUND" != 1 ]; then
  fail "the query was not written to the SQLite query log"
fi

echo "smoke-test: OK - the binary answered a query and logged it to SQLite"
