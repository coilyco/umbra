# Release pipeline, lint config, and the mark

Development is on Forgejo and every release is built and published on GitHub Actions (COI-2945), where the Go module is fetched from. umbra is the base library of the umbra / ward stack.

## Two-stage flow

`main` is the integration branch, `release` is last-known-good, and only gate-green shas release.

A push to `main` fires `promote.yml`: the full repo gate (vet, build, race test, godoc-current, mod tidy, golangci-lint, govulncheck, secret scan) runs, then it publishes the commit-scoped draft tag `draft-${sha}` and only then fast-forwards `release` to that sha. govulncheck and the release build run on the newest stable Go, because a standard-library fix ships only in it. Both pushes use a coilyco-ops App installation token (`actions/app-token`), because a `GITHUB_TOKEN` push starts no workflow, so `release.yml` would never run. The checkout sets `persist-credentials: false` so the stored job token cannot override the push URL.

The `release` push fires `release.yml` under a no-cancel concurrency queue, so promoted shas release in sequence. It verifies the matching draft tag, applies the automatic minor bump (major stays hand-driven), creates the tag, builds the six-platform `umbra` matrix, renders and verifies the Homebrew formula and Scoop manifest, and creates the GitHub release with every binary plus `SHA256SUMS`. `wait-github-assets.sh` then confirms every asset the formula and manifest name answers, and only then `release-publish-package.sh` commits the formula to `coilyco/homebrew-tap` and the manifest to `coilyco/scoop-bucket`, rebasing and retrying when another train moved them. A wait that runs out fails the job loudly with the missing URLs, leaving the tap on the last good version.

## golangci config notes

`.golangci.yaml` is adopted from the cli-* family and leans on cyclomatic-complexity checks, because these packages are security boundaries or wire-protocol layers where tangled branchy code is where the bugs live.

- **G204** fires on every `exec.CommandContext` even with argv properly constructed. Argv validation happens at the umbra policy layer, and refusing it here would defeat the point of the wrappers.
- **G301/G302/G304/G306** file permissions are managed deliberately per call site, so the per-site choice is trusted over a blanket rule.
- Generated files and tests relax complexity and a few correctness linters: mechanical or long table-driven code is fine.
- The generated-code relaxation matches on `(^|/)internal/clitrees/` rather than `^internal/clitrees/`. In a git worktree golangci-lint reports paths prefixed with the relative hop back to the checkout, which a start-anchored pattern would miss, leaking generated-code noise into every dispatched commit.

## The mark

Four uneven strips laid over a lit opening, with a pair of tapered slits cut into the deepest one. A sibling of the coilyco org avatars, sharing their ink, mint, and lilac.

The mark says what umbra does: the strips hold the opening's whole rim and never cross its middle, because umbra guards a boundary and leaves the work inside it alone. The slits are the framework looking out from behind that boundary. Files and their canvases live in [assets/mark/README.md](../assets/mark/README.md).
