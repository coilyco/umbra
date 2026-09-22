# Declaring a grant's response shape

Every other primitive here narrows what a caller can send: `query`/`body`
gate the request, `fail_when` refuses a response that fails a postcondition,
`restrict` bounds a path value. Nothing narrowed what a caller gets *back* -
a successful call's response passed through whole, whatever fields the
upstream happened to include. `returns` closes that gap: a grant declares the
response shape it is willing to hand back, and a successful call is pruned to
it before anything renders it. teable:coilyco-flight-deck/umbra#8049.

## Declaring it

```kdl
can get repo {
    returns "id" "name" "private"
}
```

Flat names keep those top-level keys whole, dropping every other key the
upstream returned. For a nested shape, `returns` takes the same
`field | object | array` block grammar as `body`
([body projection](opcore-body.md)):

```kdl
can get repo {
    returns {
        field "id" type="integer"
        field "name" type="string"
        object "owner" {
            field "login" type="string"
        }
        array "topics" items="string"
    }
}
```

`keyed` and `variant` object shapes work too, identically to a body field -
see [keyed maps and discriminated unions](opcore-body-variants.md). The one
grammar difference from `body`: a `returns` field carries no `upstream=`
alias. There is no outgoing wire name to rename on the way out, only a
response key to keep or drop.

`raw-response` and `returns` are mutually exclusive (fail-closed at parse
time): a raw body is never decoded, so there is nothing for `returns` to
prune.

## What pruning does

A key `returns` does not name is dropped, even when the upstream response
carries it. A key it does name keeps its value, narrowed further when the
field declares a nested `object`/`array` shape, or kept as an opaque blob
when the field sets `raw=true`. An array response root, or any other
non-object value, passes through untouched - `returns` describes an object's
fields, not a list's own shape.

Pruning runs once, centrally, in `opcore.Operation.Execute`, after
`fail_when` is evaluated against the *full* response and before the result is
handed to a caller. Every transport sees the same narrowed bytes because
every transport - the generated CLI, the generated HTTP API, and umbra
serving itself as an MCP server - drives `Execute` rather than rendering the
wire response directly. No `returns` declaration means no pruning: today's
whole-response behavior is the default, unchanged.

## What this does not cover yet

`returns` is parsed today only for the inline dialect (a grant with no
upstream spec to resolve against). A spec-driven grant against a real
OpenAPI/Swagger document has no `returns` syntax yet - its generated document
keeps carrying the upstream's own declared response schema verbatim
(`http/specverb/prune.go`), and nothing prunes what the engine hands back at
request time either. Extending `returns` to override or narrow that path is
separate, larger work: tracked as a follow-up rather than folded in here.

[Emitting OpenAPI](openapigen.md) turns a declared `returns` into the
operation's `200` response schema, replacing the undifferentiated `default`
every other leaf still emits.
