#!/usr/bin/env bash
# Sends this repo's GitHub heads and tags to Tangled, then reads main back (COI-2939).
# Delivery is also checked from outside by infrastructure scripts/check_mirror_sync.py.
set -euo pipefail

repo="${GITHUB_REPOSITORY#*/}"
# The two overrides exist so the push can run against local bare repositories.
source_url="${TANGLED_MIRROR_SOURCE:-https://github.com/${GITHUB_REPOSITORY}.git}"
remote="${TANGLED_MIRROR_REMOTE:-git@tangled.org:coilysiren.me/${repo}}"

# Warn and stop rather than fail, because the key arrives after this file does.
# A missing key cannot hide a stalled mirror: the scheduled ref check compares both sides.
if [ -z "${TANGLED_SSH_KEY:-}" ]; then
  echo "::warning title=Tangled mirror::TANGLED_SSH_KEY is not set, nothing was pushed."
  echo "Nothing pushed: TANGLED_SSH_KEY is not set." >> "$GITHUB_STEP_SUMMARY"
  exit 0
fi

install -d -m 700 "$HOME/.ssh"
printf '%s\n' "$TANGLED_SSH_KEY" > "$HOME/.ssh/tangled_key"
chmod 600 "$HOME/.ssh/tangled_key"
# Pinned tangled.org host key, read 2026-10-27, SHA256:fLyp6ivr5HqmGI8yJiPYstTiJa2AXF/
# RAa9kF/ur1xo. A rotation fails the push loudly instead of trusting a new key.
echo "tangled.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAII2UShEm/FFPmYZUizaPnIqOJuynoCQpcLhl5PPHd02n" > "$HOME/.ssh/known_hosts"
export GIT_SSH_COMMAND="ssh -i $HOME/.ssh/tangled_key -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$HOME/.ssh/known_hosts -o AddressFamily=inet"

# Explicit refspecs, not --mirror, which would also send GitHub's hidden refs/pull/*.
git init --bare --quiet "$RUNNER_TEMP/mirror.git"
cd "$RUNNER_TEMP/mirror.git"
git fetch --quiet --prune --no-tags "$source_url" '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'
git push --prune "$remote" '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'

# A push that exits 0 only claims sending.
want="$(git rev-parse refs/heads/main)"
got="$(git ls-remote "$remote" refs/heads/main | cut -f1)"
if [ "$want" != "$got" ]; then
  echo "::error title=Tangled mirror::main is ${got:-absent} on Tangled, expected $want"
  exit 1
fi
echo "Tangled main is $want" >> "$GITHUB_STEP_SUMMARY"
