# umbra features

Inventory of umbra today. See `examples/<feature>/` for each.

## Framework primitives

Grouped by **guarded surface** over a shared `pkg/`. See [architecture.md](architecture.md).

### CLI passthrough surface (`cli/`)

- **passthrough** - Audited urfave subcommand around an existing binary.
- **execverb** - Exec-dialect KDL verbs + the `passthrough <bin>` funnel. See [execverb.md](execverb.md); actions: [execverb-actions.md](execverb-actions.md).
- **verb** - Middleware around every `*cli.Command.Action`.
- **shell** / **sandbox** - Subprocess exec + seccomp/namespace jail (Linux), auto-degrading on either denial mode. See [sandbox](sandbox.md).
- **gittree** - Clean+synced gate for repo-shaped verbs.
- **repocfg** - Per-repo config file loading under a consumer-chosen filename.

### HTTP request surface (`http/`)

- **egress** - Per-run CONNECT proxy with consumer allowlist.
- **Specgen/codegen** - Discovery, locks, generation, and
  [embedded fixed files](specgen-embedded-files.md).
- **Inline HTTP contracts** - Typed query, nested-string body projection, and
  JMESPath response postconditions. See
  [body mapping](opcore-body-mapping.md) and [inline operations](opcore-inline.md).
- **complex actions** - `poll`/`call`/`collect`. See [actions](specverb-actions.md).
- **respfmt** - JSON renderer + JMESPath, five formats.

### Shared core (`pkg/`)

- **audit** - Rotated JSONL invocation log with optional typed CI attribution.
- **policy** - Argv validation rejecting shell metachars.
- **scope** - Resolve cwd to git root for audit.
- **exitcode** - Public exit-code taxonomy.
- **valuesource** - Shared `value <provider>` resolution: env/file/literal built in, store-backed resolvers declared by the consumer. See [value-providers.md](value-providers.md).
- **config** - Layered-config primitives + `OverlayFile[T]`.
- **stepflow** - Transport-agnostic ordered sequence engine with explicit data threading.
- **ttlcache** - Generic TTL-keyed cache.
- **skillgen** - Render deterministic native agent skills from CLI command trees.
- **broker** / **credseed** - Credential broker and env seeder. See [broker.md](broker.md).
- **scan** / **attribution** / **flock** / **version** / **issueref** /
  **ownertrust** - Ward-lifted helpers. See [ward-helpers.md](ward-helpers.md).

## Repo development

- `Makefile` is the source of truth for dev verbs (umbra is unguarded).
- `.golangci.yaml` / `staticcheck.conf` mirror urfave/cli. CI validates code, secrets, and docs. GitHub publishes and deploys nothing.
- Release is automated and Forgejo-canonical, with commit-scoped draft tags on `main`, public release tags on `release`, packaged `specgen` binaries, and automatic Homebrew tap plus Scoop bucket updates. Consumers self-bump. See [release-pipeline.md](release-pipeline.md).

## See also

- [README.md](../README.md) - human-facing intro.
- [AGENTS.md](../AGENTS.md) - agent-facing operating rules.
- [features-detail.md](features-detail.md) - per-primitive details.

Cross-reference convention from the shared repo-pointer rule in the agentic-os docs.
