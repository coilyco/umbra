# exec-dialect verbs (execverb)

The exec-transport sibling of [specverb](specverb.md): policy as KDL sentences, pointed at wrapped binaries rather than HTTP APIs.

```kdl
wrap ward git {
    exec git
    can run commit { deny-flag "--no-verify" aliases="-n" }
    never run "reflog expire"
}
```

- **`passthrough <bin>`** - funnel sugar: `exec` plus an implicit open funnel, for a tool whose verbs are impractical to name one by one. Mutually exclusive with `exec`.
- **`exec <bin>`** - the binary, fixed at parse. `argv-prefix` pins an unoverridable leading argv, the remote-exec transport. `env <NAME> { value <provider> "<addr>" }` resolves at exec time, so a secret comes from SSM rather than the guardfile.
- **`can run <sub>`** - deny-by-default, so only named subcommands mount. A quoted multi-word sentence is a nested path. `can run "*"` is an open funnel and must be the only grant.
- **`never run <sub>`** - a refused path, checked ahead of every grant, including a `can run` parent or funnel that covers it. It refuses with its own text (`is never allowed by this guardfile`), so a `never` reads differently from an ungranted verb and deleting the line changes what a caller sees. Naming a path the guardfile also grants or withholds, `never run "*"`, and sitting beside `allow` all fail closed. `never pass` is the wrap-level argv form. Before umbra#8120 a `never run` was documentation only.
- **`argv <tokens...>`** - fixed fragments replacing the subcommand. **`embed`** compiles a file in and inserts its runtime path. **`sealed`** forbids trailing caller args. **`bin`** overrides the wrap binary for one leaf and does **not** inherit `argv-prefix`.
- **Flag policy** - `deny-flag` (default-allow minus denials, see [deny-flag spellings](#deny-flag-spellings)) or `allow-flag` (strict allowlist).
- **`when` / `deny-when <sel> matches <glob...>`** - argv guards. The selector is a flag name (`secret-id` reads `--secret-id`), `any-arg`, or `argN`.
- **`gate <name>`** - a registered preflight gate. The registry ships empty, so every name fails closed until a consumer registers one.

Unknown nodes fail closed. `execverb.Mount` mirrors `specverb.Mount`: one leaf per grant under `verb.Wrap`, `SkipFlagParsing` so caller args pass through after the check. The invocation is `bin + argv-prefix + (subcommand or argv) + caller args`.

## `allow <bin...>` inspect lists

Opens N read-only funnels from one wrap. `allow grep cat` desugars mechanically to two `exec` + `can run "*"` wraps, so it is exactly as safe as what it stands in for. Bare names only: a path separator or metacharacter fails closed. Mutually exclusive with `exec`/`can run`. A wrap-level guard composes onto every leaf, and a wrap guard with no list fails closed.

## Complex actions

A wrap may declare `action` nodes: ordered `call run <grant>` sequences over granted leaves, run by `pkg/stepflow`. Step `args` are positional tokens appended after the pinned `argv`. Named `args` blocks are refused. Each step decodes to `{exit_code, ok, stdout, stderr, last_line, kv{...}}`, and later steps read `$as.field`. A non-zero exit stops the sequence. Guards hold throughout, each step audits its own row, and `--dry-run` renders the plan without firing.

## deny-flag spellings

`deny-flag "--no-verify"` once matched argv by exact string, so `git commit -n` walked past it (COI-1899). umbra holds no alias table for a wrapped binary, so a `deny-flag` states its other spellings, `aliases="-n"` or `aliases="a,b"`, or says there are none with `aliases="none"`. One that says neither fails when the guardfile parses, naming the flag. Every listed spelling is refused with the same text.

Past the list, two forms the wrapped parser accepts are refused too: a denied short flag inside a bundle (`-an` carries `-n`) and a long flag cut short (git takes `--no-verif`). Both over-refuse on purpose: `-mnote` carries an `n`, and `--force` is a prefix of a denied `--force-with-lease`. An allowlist sees every spelling without any of this, so prefer `allow-flag` where the grant can be named.

## Value flags

`valueFlags` in `cli/execverb/argv.go` names the long flags whose value arrives as a separate token. Without it `--region us-east-1` leaves `us-east-1` looking positional, slipping past an `argN` guard. The table is one vendor's shape, so a grant declares its own with `value-flag <name>`, merged over the built-ins. umbra#282.

A grant that guards `argN` or `any-arg` while allowing a long flag neither the table nor its own `value-flag` list names is **refused when the guardfile parses**, not at runtime. The arity is unknowable from the flag alone, and guessing it wrong binds the guard to the wrong token with no signal. Declare the flag either way: as a value-taker so the value is consumed, or to state that it is a boolean. agentic-os#1351.

## Shaping a call beyond allow and deny

Three grant-level primitives state more than presence or absence: `withhold` mounts a verb that says why it is refused, `pin` fixes a flag's only safe value, and `resolve-flag` reads a flag's value through `pkg/valuesource` instead of forwarding it. See [occlusion primitives](execverb-occlusion.md).

A wrap may also declare `replace`, which installs the generated binary under the wrapped tool's own name instead of as a verb under a driver's tree. The grants become the tool's whole visible surface. See [occluded replacement binaries](execverb-replacement.md).
