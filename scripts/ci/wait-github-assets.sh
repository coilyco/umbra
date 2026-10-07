#!/usr/bin/env bash
# Wait until every release asset the formula and manifest name answers on GitHub.
# They name GitHub URLs, and the ser8 mirror copies the assets there after the Forgejo
# release, so a tap bump that landed first would 404 every install
# (teable:coilyco/umbra#8694).
set -euo pipefail

dist=${1:-dist}
urls=$(grep -ho "https\{0,1\}://[^\"' ]*/releases/download/[^\"' ]*" \
  "$dist/umbra.rb" "$dist/umbra.json" | sort -u)
if [ -z "$urls" ]; then
  echo "::error::no release URLs in $dist/umbra.rb or $dist/umbra.json" >&2
  exit 1
fi

err=$(mktemp)
trap 'rm -f "$err"' EXIT
deadline=$(( $(date +%s) + ${UMBRA_ASSET_WAIT:-1200} ))
# The runner's route to github.com drops connections now and then (COI-2427), so an asset
# that answered once stays answered rather than having to answer again in the same pass.
seen=" "
while :; do
  missing=""
  for url in $urls; do
    case "$seen" in *" $url "*) continue ;; esac
    # One byte is enough to prove the asset is there, whatever its size.
    code=$(curl -sSL --connect-timeout 10 --max-time 30 -r 0-0 -o /dev/null \
      -w '%{http_code} ip=%{remote_ip}' "$url" 2>"$err") && rc=0 || rc=$?
    case "$code" in 200\ * | 206\ *) [ "$rc" -eq 0 ] && { seen="$seen$url "; continue; } ;; esac
    echo "missing ${url##*/}: http=$code curl=$rc $(head -c 160 "$err" | tr '\n' ' ')" >&2
    missing="$missing $url"
  done
  if [ -z "$missing" ]; then
    echo "every release asset answers"
    exit 0
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "::error::the release assets are still missing, so the tap and bucket are not bumped:$missing" >&2
    exit 1
  fi
  sleep "${UMBRA_ASSET_POLL:-30}"
done
