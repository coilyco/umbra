# install kdl-specs

Each cli-guard tag publishes raw `kdl-specs` binaries for Linux, macOS, and
Windows on amd64 and arm64. Verify the selected binary against the release's
`SHA256SUMS`, rename it to `kdl-specs` (or `kdl-specs.exe`), and place it on
`PATH`.

Go users can install the tagged command directly:

```sh
GOPRIVATE=forgejo.coilysiren.me go install forgejo.coilysiren.me/coilyco-flight-deck/cli-guard/cmd/kdl-specs@vX.Y.Z
```

The packaged driver still invokes the Go toolchain for `lock`, `build`, and
`run`. `kdl-specs --version` prints the driver version and the cli-guard module
ref that an unqualified `lock` will freeze. Release binaries and tagged
`go install` builds report the same tag for both values. A source checkout
reports `(devel)` and defaults the lock ref to `latest`.

## See also

- [kdl-specs.md](kdl-specs.md) - driver lifecycle, discovery, and locks.
- [release-pipeline.md](release-pipeline.md) - packaged artifact publication.
- [FEATURES.md](FEATURES.md) - shipped feature inventory.
