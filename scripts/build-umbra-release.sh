#!/usr/bin/env bash
set -euo pipefail

version=${1:?usage: build-umbra-release.sh VERSION [DIST_DIR]}
dist_dir=${2:-dist}

case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *)
    echo "release version must be a v-prefixed semantic version, got: $version" >&2
    exit 2
    ;;
esac

mkdir -p "$dist_dir"

targets=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/arm64
)

for target in "${targets[@]}"; do
  target_os=${target%/*}
  target_arch=${target#*/}
  extension=
  if [[ "$target_os" == windows ]]; then
    extension=.exe
  fi

  artifact="$dist_dir/umbra-${target_os}-${target_arch}${extension}"
  echo "building $artifact"
  GOOS="$target_os" GOARCH="$target_arch" CGO_ENABLED=0 \
    go build -trimpath \
      -ldflags "-s -w -X github.com/coilyco/umbra/http/umbra.buildVersion=${version}" \
      -o "$artifact" ./cmd/umbra
done

(
  cd "$dist_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum umbra-* > SHA256SUMS
  else
    shasum -a 256 umbra-* > SHA256SUMS
  fi
)
