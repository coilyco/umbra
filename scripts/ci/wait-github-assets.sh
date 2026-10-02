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

deadline=$(( $(date +%s) + ${UMBRA_ASSET_WAIT:-1200} ))
while :; do
  missing=""
  for url in $urls; do
    # One byte is enough to prove the asset is there, whatever its size.
    curl -fsSL --max-time 60 -r 0-0 -o /dev/null "$url" 2>/dev/null || missing="$missing $url"
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
