#!/bin/sh
# Render Homebrew and Scoop metadata from version-stamped umbra binaries.
set -eu

version=${1:?usage: render-umbra-packaging.sh VERSION [DIST_DIR]}
dist=${2:-dist}

case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *)
    echo "umbra release version must be a v-prefixed semantic version, got: $version" >&2
    exit 2
    ;;
esac

bare=${version#v}
base="https://github.com/coilyco/umbra/releases/download/${version}"

sha() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

darwin_amd64=$(sha "$dist/umbra-darwin-amd64")
darwin_arm64=$(sha "$dist/umbra-darwin-arm64")
linux_amd64=$(sha "$dist/umbra-linux-amd64")
linux_arm64=$(sha "$dist/umbra-linux-arm64")
windows_amd64=$(sha "$dist/umbra-windows-amd64.exe")
windows_arm64=$(sha "$dist/umbra-windows-arm64.exe")

cat > "$dist/umbra.rb" <<EOF
class Umbra < Formula
  desc "Generate guarded CLIs from KDL policy and committed API locks"
  homepage "https://github.com/coilyco/umbra"
  version "${bare}"
  license "MIT"

  on_macos do
    on_intel do
      url "${base}/umbra-darwin-amd64"
      sha256 "${darwin_amd64}"
    end
    on_arm do
      url "${base}/umbra-darwin-arm64"
      sha256 "${darwin_arm64}"
    end
  end
  on_linux do
    on_intel do
      url "${base}/umbra-linux-amd64"
      sha256 "${linux_amd64}"
    end
    on_arm do
      url "${base}/umbra-linux-arm64"
      sha256 "${linux_arm64}"
    end
  end

  def install
    bin.install Dir["umbra-*"].first => "umbra"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/umbra --version")
  end
end
EOF

cat > "$dist/umbra.json" <<EOF
{
    "version": "${bare}",
    "description": "Generate guarded CLIs from KDL policy and committed API locks",
    "homepage": "https://github.com/coilyco/umbra",
    "license": "MIT",
    "architecture": {
        "64bit": {
            "url": "${base}/umbra-windows-amd64.exe",
            "hash": "${windows_amd64}",
            "bin": [["umbra-windows-amd64.exe", "umbra"]]
        },
        "arm64": {
            "url": "${base}/umbra-windows-arm64.exe",
            "hash": "${windows_arm64}",
            "bin": [["umbra-windows-arm64.exe", "umbra"]]
        }
    }
}
EOF

echo "$dist/umbra.rb"
echo "$dist/umbra.json"
