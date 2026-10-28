#!/usr/bin/env bash
# Publish the draft tag that release.yml later verifies, proving the promoted
# commit is the one CI gated.
set -euo pipefail

if [ -z "${PROMOTE_TOKEN:-}" ]; then
  echo "::error::the App token is missing; the draft tag push must be attributable and start workflows" >&2
  exit 1
fi

remote_url="https://x-access-token:${PROMOTE_TOKEN}@github.com/${REPO}.git"
draft_tag="draft-${GITHUB_SHA}"

if git ls-remote --exit-code --tags "${remote_url}" "refs/tags/${draft_tag}" >/dev/null 2>&1; then
  echo "draft tag already published: ${draft_tag}"
  exit 0
fi

git tag "${draft_tag}"
git push "${remote_url}" "refs/tags/${draft_tag}"
echo "published ${draft_tag}"
