# emitting an OpenAPI document from a guardfile

`umbra openapi` renders the surface a guardfile grants as an OpenAPI 3.1 document, so a policy written entirely in KDL is consumable by tooling that wants a spec (umbra#1052).

```
umbra --guardfile ward-ops-forgejo.kdl openapi --out forgejo.granted.json
```

Output goes to stdout when `--out` is absent.

## What it emits, and why that is the useful part

The source is `[]opcore.Descriptor`, the same resolved set the generated binary mounts its verbs from. So the document is not a description of the upstream. It is a description of **the granted subset**, which is usually much smaller.

A guardfile granting three verbs against a 756-operation upstream emits three operations. That narrowing is the point: it is the only artifact that states what this surface actually offers, in a form a spec-driven consumer can read.

## The four mappings that needed deciding

**`operationId` is the dotted `VerbName`**, for example `ward.ops.forgejo.repo.delete`. It is already umbra's audit identity, it is already unique across a group, and it round-trips with the method+path addressing added in umbra#6824. Two grants resolving to the same `VerbName` is refused rather than emitted, because a document with duplicate operationIds is invalid and the collision is a guardfile bug worth surfacing.

**A `FixedBody` pin becomes JSON Schema `const`.** A pinned body key is supplied by umbra and a caller cannot vary it, so rendering it as an ordinary property would claim freedom the guardfile does not grant. It is deliberately not listed in `required`, because the caller does not send it at all. This is the reason the emitted version is 3.1 rather than 3.0: `const` is JSON Schema 2020-12.

**A withheld verb does not appear.** A spec describes what a caller may call, and a withheld verb is a declared refusal rather than an operation. `withhold` states itself through the CLI surface and the audit record, which is where a refusal belongs.

**umbra's own claims ride as vendor extensions**, never as invented standard fields:

- `x-umbra-grant` - the authorizing grant sentence, e.g. `can delete repo`.
- `x-umbra-destructive` - present and true only for a leaf that mutates irreversibly.

A guardfile `describe` becomes the operation `summary`, and the bounds the guardfile already enforces (`enum`, `minimum`, `maximum`, `minItems`, `maxItems`) are carried onto the schema so the document states the same limits the runtime does.

**A declared `returns` shape becomes the `200` response schema** (docs/opcore-returns.md), replacing the undifferentiated `default` every other leaf still emits. The document only ever states a response shape the engine itself enforces - it is never a claim about the upstream's own contract, which prune.go leaves untouched for the spec dialect.

## What is skipped, and why that is not a gap

A descriptor that reaches something other than a URL is left out and named on stderr. A `graphql`, `sql`, `mcp` or proxy leaf has no HTTP path, so emitting one would invent a route that does not exist. Exec-dialect and mcp-dialect members are skipped whole for the same reason: an exec member runs a subprocess and addresses no URL at all.

The skip is reported rather than silent, so a caller who expected an operation can see which one went and why.

## The inline case, which this does not reach yet

`opcore.ParseInline` resolves a guardfile that names no spec, declaring its operations inline, into the same `[]Descriptor`. That is the case where a guardfile-defined surface exists **only** inside umbra, and it is the one an emitted document would serve best.

`openapigen.Emit` takes descriptors rather than a guardfile precisely so that path works: a consumer holding inline descriptors can render them directly. What is missing is the driver wiring, because `ParseInline` has no caller inside this repository and no member transport resolves to it. The verb covers spec-driven members today.

## Validating the output

The end-to-end test drives a committed guardfile and spec through `specverb.Descriptors` into a document, and asserts the granted count rather than the shape alone: a document listing more than the granted operations would over-state the surface, which is the failure mode worth catching.
