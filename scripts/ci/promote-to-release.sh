#!/usr/bin/env bash
# Fast-forward main onto the release branch.
set -euo pipefail

if [ -z "${PROMOTE_TOKEN:-}" ]; then
  echo "::error::the App token is missing; a GITHUB_TOKEN push would not start release.yml" >&2
  exit 1
fi

# Plain push: git refuses a non-fast-forward, so a diverged release branch fails
# loud here instead of being silently rewritten.
git push "https://x-access-token:${PROMOTE_TOKEN}@github.com/${REPO}.git" HEAD:release
echo "promoted $(git rev-parse --short HEAD) to release"
