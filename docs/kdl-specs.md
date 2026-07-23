# the no-code driver (kdl-specs / cmd/kdl-specs)

`ward-kdl` is a **no-code** CLI: the consumer authors only policy plus its committed locks, never Go or build glue. `kdl-specs` is the uv-style driver. See [specverb.md](specverb.md). Every spec may carry a top-level [`description`](kdl-description.md) node: standing context, not a comment header.

## Install

See [install kdl-specs](kdl-specs-install.md) for release binaries, checksums,
`go install`, and the driver-to-framework version contract.

## Discovery and merging

A `--guardfile` selects a **binary**, not the whole build: the driver reads every `*.guardfile.kdl` in that file's directory sharing its `wrap <binary>` name (`Group[0]`) and merges them, each mounting as its own command group. With no `--guardfile`, the cwd must resolve to one binary, else the driver lists them and asks you to pick. A different wrap name is a **separate** binary, never merged in.

`gen`, `build`, and `run` accept `--binary <name>` when the generated executable should be named differently from the source `wrap <binary>` group. This renames the generated `cli.Command` and the cache/build output name only. Discovery, merging, command groups, and spec override env vars still come from the Guardfile wrap group.

Separate per guardfile: the spec lock and reference doc. Shared: the generated `main.go` and the one `specverb.lock`. The override env var is keyed on the full wrap group, so merged specs never collide. Spec and exec members can share one binary - see [mixed transports](specverb-mixed-transports.md).

## The five verbs

- **`gen`** - render the merged `main.go` into the cache (or `--out` to inspect it), and write each member's reference doc (`<name>.md`) from the committed spec lock. A debug step; `run` materializes for you. `--binary <name>` overrides the generated app name for inspection or renamed builds.
- **`lock`** - the deliberate online step. For each member, fetches the upstream Swagger, **prunes it to the granted surface**, writes the committed spec lock (`<spec>.lock.json`), and refreshes its reference doc; then resolves the merged build's module graph (`go mod tidy` in a throwaway module) once and freezes it into the shared `specverb.lock`. `--cli-guard-ref` pins the framework version (defaults to the driver's own); `--cli-guard-replace` points at a local checkout for development.
- **`skew`** - for each member, prune live upstream to the same granted surface and diff it against the committed lock, each drift line prefixed with its member, so only operations the consumer exposes register as drift. Exit 3 on any drift, never writes; a fetch failure is a plain error, distinguishable from drift.
- **`build`** - materialize the binary **out-of-band** (same cache + staleness path as `run`) and copy it to `--out` (default `bin`) instead of execing it. `--out` follows `go build -o`: a directory (or trailing `/`) takes the generated binary name, else it is the explicit file path. `--binary <name>` sets that generated name, defaulting to the Guardfile-derived binary. `--set-version <v>` stamps the binary's `--version` via `-ldflags`, default `dev`. Refuses without committed locks.
- **`run`** - materialize the consumer binary **out-of-band** and exec it with the passed-through args. `--binary <name>` uses the renamed generated app.

## Out-of-band materialization

`run` and `build` share the cache and staleness path described in [kdl-specs-materialization.md](kdl-specs-materialization.md). The cache is keyed by the generated binary name plus member paths, so a repo can cache both source and renamed builds.

## The two locks

The committed build artifacts are:

- **`<spec>.lock.json`** - one pruned embedded API snapshot per merged API.
- **`specverb.lock`** - one frozen Go dependency graph per binary.

See [kdl-specs-materialization.md](kdl-specs-materialization.md) for cache and offline-build detail.

Origin: the KDL specs surface.
