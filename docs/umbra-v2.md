# umbra v2 on urfave/cli v4

A living plan. umbra v2 is umbra rebuilt on urfave/cli v4, with v4 doing the work umbra does by hand today on v3. It doubles as the first real downstream for v4, so every gap umbra hits becomes a concrete upstream issue rather than a hypothetical one.

Status: proposal. Nothing here ships yet. umbra main runs on `github.com/urfave/cli/v3` v3.9.0.

## Why now

The [v4 design](https://github.com/urfave/cli/blob/v4-spike/v4/DESIGN.md) on the `v4-spike` branch, discussed in [urfave/cli#2446](https://github.com/urfave/cli/discussions/2446) with its first milestone in [urfave/cli#2447](https://github.com/urfave/cli/issues/2447), is built around agents as a first-class caller. umbra exists to stand between agents and a host. Most of what v4 proposes for agents is something umbra already carries in its own packages, which is the overlap this page tracks.

## Where v4 meets umbra

Each line is a v4 proposal, then the umbra code it would replace or reshape.

* **Typed errors** - v4's `*cli.Error` with an open set of `cli.NewKind` kinds. umbra's policy refusals, metachar rejections and withheld verbs become registered kinds, matched with `errors.Is` instead of string checks.
* **Exit codes from a personality** - v4 presets own the exit-code table. umbra's public taxonomy (2 for a policy refusal, 5 for a user error, see [FEATURES](FEATURES.md)) becomes an umbra preset, so the code an agent sees and the code the audit row records come from one table.
* **Agent modifier** - one JSON object on stderr with each error's kind, message, flag and value, plus the exit code, and a `version` field. umbra's refusal output adopts that format instead of its own, and the audit log records the same object.
* **Sensitive flags** - a flag marked `Sensitive` is masked in errors, help, Agent JSON and the MCP schema. This overlaps umbra's `resolve-flag`, which spills a resolved value to a file rather than argv. v2 marks those flags `Sensitive` as well, so a mistyped secret never reaches an agent transcript.
* **Annotations** - `ReadOnly`, `Destructive` and `Idempotent` on a command, mapped to MCP tool hints. umbra guardfile grants can set these, so a client asks before a destructive verb runs.
* **MCP from the command tree** - `cli/v4/mcp` serves commands as MCP tools from the `jsonhelp` schema. umbra's MCP projection of granted leaves ([mcpverb serving](mcpverb-serving.md)) either sits on that module or becomes the reason it grows a policy hook.
* **Confirmation that never hangs** - `cmd.Confirm()` fails at once with `ConfirmationRequired` when no person can answer. umbra gets this behavior for free on every wrapped verb.
* **Flag rules and `cmd.Source`** - conflicts, requires and one-of groups as error kinds, plus where a value came from. umbra's `pin` (a flag fixed to one value the caller cannot override) can assert its source is umbra and not the caller.
* **Unknown flags: reject, pass through, or collect** - the closed default umbra wants for a guarded verb, and the pass-through `default-allow` wraps need, as one library switch.
* **No package globals** - root-command fields instead of `OsExiter` and friends, so umbra's parallel tests stop sharing state.

## The loop with urfave/cli

1. umbra v2 builds against `v4-spike` on a branch, one milestone at a time, in the order v4 lands them.
2. Every place umbra needs a hook v4 lacks becomes an issue or PR upstream, linked from the log below, with the umbra test that needs it.
3. umbra's own suites (negative controls, refusal and exit-code tests) run against v4 as an extra downstream check on each v4 milestone.
4. When v4 cuts a release, umbra v2 tracks it, and the v3 line gets fixes for the support window umbra publishes then.

umbra's own repo is the place to record the umbra side. The v4 decisions stay in urfave/cli's own threads, with their own maintainers.

## Open questions

* Should umbra's exit-code table become a v4 preset upstream, or stay an umbra-owned preset built on v4's API?
* Can a guardfile drive v4 flag rules directly, or does umbra keep its own validator and map results to kinds?
* Does the `mcp` module need a policy hook for umbra's grants, or does umbra wrap it?
* Is v4's `URFAVE_CLI_AGENT` the switch umbra sets for every wrapped call, or does umbra force the Agent modifier itself?

## Log

Newest first. One line per milestone or upstream link.

* 2026-10-02 - page opened. Read the v4 design at `v4-spike` (commit 4f949f1a6). umbra is on v3.9.0, and v3's latest is v3.14.0.
