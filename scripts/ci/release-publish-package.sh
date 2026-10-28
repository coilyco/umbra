#!/bin/sh
# Commit one rendered package file into a GitHub package repo as the coilyco-ops App.
#
# usage: release-publish-package.sh <repo> <dist file> <path in repo>
#
# The tap and the bucket take bumps from several release trains, so a push that
# loses the race is rebased and retried rather than failed. Only the one file
# this train owns is staged.
set -eu

repo="${1:?usage: release-publish-package.sh <repo> <dist file> <path in repo>}"
source_file="${2:?missing dist file}"
target="${3:?missing path in repo}"
: "${APP_TOKEN:?APP_TOKEN is required}"
: "${TAG:?TAG is required}"
# The override lets the test push to a local bare repository.
remote="${PACKAGE_REMOTE_BASE:-https://x-access-token:${APP_TOKEN}@github.com/coilyco}/${repo}.git"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM

git clone --quiet --depth 1 "$remote" "$work/repo"
cd "$work/repo"
cp "$OLDPWD/$source_file" "$target"
if git diff --quiet -- "$target" && [ -z "$(git ls-files --others --exclude-standard -- "$target")" ]; then
  echo "release-publish-package: $repo already carries $TAG."
  exit 0
fi
git config user.name "coilyco-ops[bot]"
git config user.email "coilyco-ops[bot]@users.noreply.github.com"
git add "$target"
git commit --quiet -m "chore(umbra): bump ${target##*/} to ${TAG} [skip ci]"

attempt=1
until git push --quiet "$remote" HEAD:main; do
  if [ "$attempt" -ge 3 ]; then
    echo "::error::release-publish-package: $repo main kept moving, gave up after $attempt pushes." >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  git pull --quiet --rebase "$remote" main
done
echo "release-publish-package: $repo now carries $target at $TAG."
