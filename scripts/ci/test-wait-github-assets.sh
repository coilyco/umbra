#!/usr/bin/env bash
# Behavior of wait-github-assets.sh against a local server, since the real one is GitHub.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
# wait returns the killed server's 143, which would become the script's own status.
trap 'kill "${server:-}" 2>/dev/null; wait "${server:-}" 2>/dev/null || true; rm -rf "$work"' EXIT
mkdir -p "$work/site/releases/download/v1" "$work/dist"
printf x > "$work/site/releases/download/v1/umbra-linux-amd64"

# Port 0 lets the OS pick, and -u flushes the line that names it.
python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$work/site" >"$work/server.log" 2>&1 &
server=$!
for _ in $(seq 1 50); do
  port=$(grep -o "port [0-9]*" "$work/server.log" 2>/dev/null | head -1 | cut -d' ' -f2 || true)
  [ -n "$port" ] && break
  sleep 0.1
done
[ -n "${port:-}" ] || { echo "the test server did not start" >&2; exit 1; }
base="http://127.0.0.1:$port/releases/download/v1"

fail() { echo "FAIL: $*" >&2; exit 1; }
run() { UMBRA_ASSET_WAIT="$1" UMBRA_ASSET_POLL=1 bash "$here/wait-github-assets.sh" "$work/dist"; }

printf 'url "%s/umbra-linux-amd64"\n' "$base" > "$work/dist/umbra.rb"
printf '{"url": "%s/umbra-linux-amd64"}\n' "$base" > "$work/dist/umbra.json"
run 5 >/dev/null || fail "an asset that answers should pass"

printf 'url "%s/umbra-linux-amd64"\nurl "%s/umbra-darwin-arm64"\n' "$base" "$base" > "$work/dist/umbra.rb"
if run 3 >/dev/null 2>"$work/err"; then fail "a missing asset must fail once the wait is up"; fi
grep -q "umbra-darwin-arm64" "$work/err" || fail "the failure should name the missing asset"

( sleep 2; printf x > "$work/site/releases/download/v1/umbra-darwin-arm64" ) &
run 10 >/dev/null || fail "an asset that appears during the wait should pass"

: > "$work/dist/umbra.rb"; : > "$work/dist/umbra.json"
if run 3 >/dev/null 2>&1; then fail "no URLs at all must fail, not pass"; fi
echo ok
