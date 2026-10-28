#!/bin/sh
# Fixture coverage for the package bump, against local bare repositories: a bump
# lands, a repeat is a no-op, and a push that loses a race is rebased and lands.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
publisher="$script_dir/release-publish-package.sh"
fixture_root=$(mktemp -d)
cleanup() {
  chmod -R u+w "$fixture_root" 2>/dev/null || true
  rm -rf "$fixture_root"
}
trap cleanup EXIT HUP INT TERM

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=commit.gpgSign GIT_CONFIG_VALUE_0=false
git init -q --bare -b main "$fixture_root/remotes/homebrew-tap.git"
seed="$fixture_root/seed"
git init -q -b main "$seed"
mkdir "$seed/Formula"
printf 'old\n' >"$seed/Formula/umbra.rb"
printf 'other\n' >"$seed/Formula/aos.rb"
git -C "$seed" add -A
git -C "$seed" -c user.name=Fixture -c user.email=fixture@example.test commit -q -m seed
git -C "$seed" push -q "$fixture_root/remotes/homebrew-tap.git" HEAD:main

mkdir "$fixture_root/dist"
printf 'new\n' >"$fixture_root/dist/umbra.rb"
publish() {
  (cd "$fixture_root" && APP_TOKEN=unused TAG=v9.9.9 PACKAGE_REMOTE_BASE="file://$fixture_root/remotes" \
    sh "$publisher" homebrew-tap dist/umbra.rb Formula/umbra.rb >/dev/null)
}
remote_file() { git -C "$fixture_root/remotes/homebrew-tap.git" show "main:$1"; }

publish
[ "$(remote_file Formula/umbra.rb)" = new ] || { echo "test-release-publish-package: the bump did not land" >&2; exit 1; }
[ "$(remote_file Formula/aos.rb)" = other ] || { echo "test-release-publish-package: a file this train does not own changed" >&2; exit 1; }
commits=$(git -C "$fixture_root/remotes/homebrew-tap.git" rev-list --count main)
publish
[ "$(git -C "$fixture_root/remotes/homebrew-tap.git" rev-list --count main)" = "$commits" ] ||
  { echo "test-release-publish-package: a repeat bump made a commit" >&2; exit 1; }

# The bare repo moves main itself and refuses the first push, as a lost race does.
# Quarantine vars are unset: objects written under them vanish with the refusal.
bare="$fixture_root/remotes/homebrew-tap.git"
cat >"$bare/hooks/pre-receive" <<HOOK
#!/bin/sh
[ -e "$fixture_root/raced" ] && exit 0
: >"$fixture_root/raced"
unset GIT_QUARANTINE_PATH GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
tree=\$(git --git-dir="$bare" rev-parse 'main^{tree}')
rival=\$(git --git-dir="$bare" -c user.name=Rival -c user.email=rival@example.test commit-tree -m "rival bump" -p main "\$tree")
git --git-dir="$bare" update-ref refs/heads/main "\$rival"
exit 1
HOOK
chmod +x "$bare/hooks/pre-receive"
printf 'newest\n' >"$fixture_root/dist/umbra.rb"
publish
[ -e "$fixture_root/raced" ] || { echo "test-release-publish-package: the race never happened" >&2; exit 1; }
[ "$(remote_file Formula/umbra.rb)" = newest ] || { echo "test-release-publish-package: the raced bump did not land" >&2; exit 1; }
git -C "$fixture_root/remotes/homebrew-tap.git" log --format=%s main | grep -q "rival bump" ||
  { echo "test-release-publish-package: the rival commit was lost in the rebase" >&2; exit 1; }

echo "test-release-publish-package: ok"
