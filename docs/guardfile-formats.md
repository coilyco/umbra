# YAML and TOML guardfiles

A guardfile for the spec, inline, exec, or mcp dialect may be written in YAML or TOML as well as KDL. The file is **lowered to KDL text** before any loader sees it, so the KDL loaders stay the only owner of what a guardfile means. Every load-time check (unknown nodes, missing fields, fail-closed rules) runs unchanged, and a YAML guardfile means exactly what its KDL twin means.

The seam is `guardfile.Lower(path, src)`. It dispatches on the extension: `.kdl` and any unrecognised extension return the source byte for byte, `.yaml`, `.yml`, and `.toml` are decoded and lowered. `Flatten` calls it, so `inherit` crosses formats in either direction. Discovery and `--guardfile` call it too.

## Where umbra looks

- **Project root** - a `.yaml`, `.yml`, or `.toml` file is a member only when its top level has a `wrap` mapping that names a `command`. Any other YAML or TOML in the tree (compose files, `Cargo.toml`, `wrap: true`) is skipped as unrelated, as KDL without `wrap` is.
- **Malformed with intent** - a file that opens a `wrap` and then fails to decode or lower fails the load, the same rule `operationIntent` applies to KDL.
- **Legacy cwd discovery** - `*.guardfile.{kdl,yaml,yml,toml}`.
- **Embedded artifact** - a YAML or TOML member embeds its lowered KDL as `<path>.kdl`, so the generated binary parses what its name says.

## Schema

Keys are snake_case and map to the kebab-case node. An unknown key is an error, so a misspelling cannot silently drop a rule.

- **Top level** - `description`, `instructions` (list of lines), `wrap`, `withhold` (list of `tool`, `reason`, `alternative`), `reject_empty_argument` (list of `tool`, `field`), `kdl`.
- **wrap** - `command` (required, list), `inherit`, `spec`, `base_url`, `auth`, `restrict`, `allow_metacharacters`, `can`, `cannot`, `never`, `override`, `provider`, `kdl`.
- **base_url** - a string, or `{value: <source>}`.
- **auth** - `scheme` (required), `header`, `prefix`, `value`, `params` (list of `name`, `value`).
- **A value source** - `{env: NAME}`, `{ssm: /path}`, or another provider, one per mapping. A list of them is an ordered fallback chain.
- **restrict** - list of `param`, `matches`.
- **A grant** - `verb` and `resource` (required), `qualifiers`, `props`, `op` (an id, or `{method, path}`), `path`, `method`, `describe`, `message`, `fail_when`, `raw_response`, `query`, `body`, `returns`, `fixed_body`, `set`, `kdl`.
- **query and body** - a list of names, or a list of typed entries. An entry names its kind (`field`, `array`, `object`, or `map`), carries its bounds as sibling keys, and nests through `entries`.
- **returns** - a list of names, or the same typed-entry grammar as `body`, minus upstream aliasing (a response field has no outgoing wire name to rename). Declares the response shape a caller is granted; a successful response is pruned to it before anything renders it. Undeclared keeps the prior whole-response pass-through. See [docs/opcore-returns.md](opcore-returns.md).
- **fixed_body** - the spec dialect's `body key=value` toggle. It is its own key because the inline dialect's `body` lists field names.

## The `kdl` escape

Any node the schema has no key for goes through `kdl`, a string of KDL placed at that spot. It must parse on its own, so unbalanced braces cannot break out of the block that carries it. Reach for it for `action`, `fetch`, `graphql`, `sql`, `proxy`, `exec`, `mcp`, and grant-body nodes such as `deny-when`.

```yaml
wrap:
  command: [ward-kdl, ops, aws]
  kdl: exec aws
  can:
    - verb: run
      resource: s3
      qualifiers: [ls]
      kdl: 'deny-when arg0 matches "*tfstate*"'
```

## Limits

- **Order** - grants and every other list keep author order in both formats. TOML writes a table's scalars before its sub-tables, so `command`, `inherit`, and `spec` lead the `wrap` table, which is also where `inherit` must sit.
- **YAML** - merge keys (`<<`) and duplicate keys are rejected, because either would change what a rule means without showing it. Anchors and aliases are allowed.
- **Values** - strings, integers, floats, and booleans. TOML dates and YAML timestamps have no KDL form and are rejected.
- **Not covered** - the `cli/execverb` grammar keeps KDL until a schema for its nodes is written. Use `kdl` for those nodes meanwhile.
