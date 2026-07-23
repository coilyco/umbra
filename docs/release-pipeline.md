# Release pipeline

Forgejo is the canonical and only release surface for cli-guard. GitHub is not
in the loop. cli-guard is the base library of the cli-guard / ward stack
(coily, the original third member, has been retired).

## Flow

Two-stage (ward#1117): main is the integration branch, `release` is
last-known-good, and only gate-green shas release.

- Push to `main` lands on Forgejo and fires `.forgejo/workflows/promote.yml`
  (stage 1): the full repo gate (vet, build, race test, godoc-current, mod
  tidy, golangci-lint, secret scan) runs, then the workflow publishes the
  commit-scoped draft tag (`draft-${sha}`) and only then fast-forwards
  `release` to that sha. The promote push uses the `CI_RELEASE_TOKEN` secret
  (a real-user PAT with `write:repository` + `read:user` from SSM
  `/forgejo/ci-release-token`, synced by aos `ward exec sync-actions-secrets`):
  job-token pushes and PATs without `read:user` get an empty actor and
  silently enqueue no workflow.
- The `release` push fires `.forgejo/workflows/release.yml` (stage 2) under a
  no-cancel concurrency queue, so promoted shas release in sequence. The
  workflow first verifies the matching draft tag exists, then the **release**
  job: `tag-bump` applies the automatic minor bump (major stays hand-driven),
  creates the tag, then
  builds the six-platform `kdl-specs` matrix, creates the Forgejo release, and
  attaches every binary plus `SHA256SUMS`. The writes use the auto-issued job
  token, so only the promote push needs the cross-repo secret.

The release assets cover Linux, macOS, and Windows on amd64 and arm64. The
stamped `kdl-specs` version is also the default cli-guard ref frozen by
`kdl-specs lock`. A tagged `go install` is the source-install alternative.
cli-guard ships no Homebrew formula, so there is no formula-bump job here.

## Tag-only by design: cli-guard does not bump its consumers

The stack's dependency direction is cli-guard -> ward. cli-guard is
the base, so its automation must not reach up into its consumers. Having
cli-guard open dependency-bump PRs on ward would reverse the
`dependsOn` edge (a dependency mutating its dependents), which couples the
tree backwards.

Downstream bumps belong to the consumers, pulled along the dependency arrow:
ward watches cli-guard's tags and opens its own self-bump PR.
That keeps every cross-repo write pointing from a consumer toward what it
depends on. The tree-direction rule is being made enforceable as a linter in
the consumer self-bump policy.

See [cli-guard release automation](https://forgejo.coilysiren.me/coilyco-flight-deck/cli-guard).
