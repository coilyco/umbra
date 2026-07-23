# cli-guard features

Inventory of cli-guard today. See `examples/<feature>/` for each.

## Framework primitives

Grouped by **guarded surface** over a shared `pkg/`. See [architecture.md](architecture.md).

### CLI passthrough surface (`cli/`)

- **passthrough** - Audited urfave subcommand around an existing binary.
- **execverb** - Exec-dialect KDL verbs + the `passthrough <bin>` funnel. See [execverb.md](execverb.md); actions: [execverb-actions.md](execverb-actions.md).
- **awsgate** - Deny sensitive-glob aws reads.
- **verb** - Middleware around every `*cli.Command.Action`.
- **shell** / **sandbox** - Subprocess exec + seccomp/namespace jail (Linux). See [sandbox](sandbox.md).
- **gittree** - Clean+synced gate for repo-shaped verbs.
- **repocfg** / **allowlist** - Per-repo command allowlist YAML, validated vs the Makefile.
- **catalog** - Assert a repo config carries a `catalog:` block with required keys.
- **hook** / **hookcfg** - PreToolUse engine expanding `repocfg.Security` into the guard registry + installer.
- **shim** - PATH shim per protected binary basename. See [deny-by-structure.md](deny-by-structure.md).
- **doctor** - Verify the basename target floor and integrity hints.
- **sudo** - Policy-free sudo plumbing over any stdin transport.
- **profiles** / **profile** / **decision** - Profile registry, axes, per-call evaluator.
- **cmd/cli-guard-hook** - PreToolUse binary for shell-only consumers.

### HTTP request surface (`http/`)

- **egress** - Per-run CONNECT proxy with consumer allowlist.
- **guardfile** / **specverb** / **opcore** / **codegen** / **cmd/kdl-specs** - Spec-driven HTTP engine, codegen, and packaged no-code driver. See [kdl-specs.md](kdl-specs.md).
- **complex actions** - `wrap`-block `poll`/`call`/`collect` verbs; the mount form shadows its leaf. See [specverb-actions.md](specverb-actions.md).
- **guarded rollback** - `compensate` + `canary` health-window rollback. See [specverb-rollback.md](specverb-rollback.md).
- **respfmt** - JSON renderer + JMESPath, five formats.
- **ghcache** / **ghidcache** / **ghratelimit** / **stscache** - Response, id, rate-limit, STS caches.

### Shared core (`pkg/`)

- **audit** - Append-only rotated JSONL invocation log.
- **policy** - Argv validation rejecting shell metachars.
- **scope** - Resolve cwd to git toplevel for audit.
- **exitcode** - Public exit-code taxonomy.
- **valuesource** - Shared `value <provider>` resolution.
- **config** - Layered-config primitives + `OverlayFile[T]`.
- **agentclaim** - Policy-free, versioned KDL and JSON identity contract with
  separate context and authority role domains. See [agent-claim.md](agent-claim.md).
- **fleetconfig** - KDL fleet-config validator (core, unguarded). See [fleetconfig.md](fleetconfig.md).
- **stepflow** - Transport-agnostic sequence/rollback engine.
- **ttlcache** - Generic TTL-keyed cache.
- **workdir** - Working-directory resolver.
- **skillgen** - Render a cli command tree to markdown/yaml.
- **broker** / **credseed** - Credential broker + env seeder. See [broker.md](broker.md).
- **scan** / **attribution** / **flock** / **version** / **issueref** / **ownertrust** - Ward-lifted helpers. See [ward-helpers.md](ward-helpers.md).
- **agentid** - Canonical agent-id generator (Go port).

## Repo development

- `Makefile` is the source of truth for dev verbs (cli-guard is unguarded).
- `.golangci.yaml` / `staticcheck.conf` mirror urfave/cli; CI runs vet, build, `test -race`, golangci-lint, trufflehog.
- Release is automated and Forgejo-canonical, with commit-scoped draft tags on `main`, public release tags on `release`, and packaged `kdl-specs` binaries; consumers self-bump. See [release-pipeline.md](release-pipeline.md).

## See also

- [README.md](../README.md) - human-facing intro.
- [AGENTS.md](../AGENTS.md) - agent-facing operating rules.
- [features-detail.md](features-detail.md) - per-primitive details.

Cross-reference convention from the shared repo-pointer rule in the agentic-os docs.
