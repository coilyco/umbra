# Keyed maps and discriminated unions

Two additive body-field primitives beside `field | object | array`, for a shape
the frozen grammar could not describe before: an object whose keys are the
caller's, not a fixed named list, and a value whose shape depends on a sibling
field's literal value. umbra#8032, motivated by mcp-beaver's Jev integration
(agent-proxy#7987): TypeSafe's `questions` map is keyed by caller-chosen
question names, each one typed `noul`, `choice`, or `score` with its own
shape. Before this, the only option was `object "questions" raw=true`, which
skips both schema generation and request-time validation - a wrong guess
costs a round trip instead of failing locally.

## `keyed` - dynamic-key maps

`keyed=true` on an `object` changes its meaning from "these named children" to
"any caller-chosen key, each value shaped like the sole `entry` child":

```kdl
object "criteria" required=true keyed=true {
    entry type="string"
}
```

`entry` carries no name of its own - it describes every key's value, not one.
It takes either a scalar `type=...` or a single nested `variant` child, never
both. A `keyed` object with any other child, zero children, or `raw=true`
alongside `keyed=true` fails closed. `keyed` is refused on a bare `field` or
on `array` in v1.

Lowers to JSON Schema as `{"type": "object", "additionalProperties": <entry
schema>}` - no fixed `properties`/`required`, since the keys are unbounded.

## `variant` - discriminated unions

```kdl
object "questions" required=true keyed=true {
    entry {
        variant on="type" {
            case "noul" {
                field "instructions" type="string" required=true
            }
            case "choice" {
                field "instructions" type="string" required=true
                object "criteria" required=true keyed=true { entry type="string" }
            }
            case "score" {
                field "instructions" type="string" required=true
                array "criteria" items="string" min-items=2 max-items=10 required=true
            }
        }
    }
}
```

`variant on="<field>" { case "<literal>" { ... } }`: `on=` names the
discriminator (required, non-empty). Each `case` takes one bare string
argument - the literal value selecting that branch - and a nested block of
ordinary `field`/`object`/`array` nodes. The discriminator itself is implicit:
a branch declaring a field with the same name as `on=` fails closed, since it
is injected automatically as a `{"const": "<literal>"}` constraint. Zero
`case` children, a duplicate `case` literal, and a nested `variant` (a case
containing another `variant`) all fail closed.

Lowers to JSON Schema `oneOf`, one branch per `case`, each carrying the
discriminator constraint plus that branch's own `properties`/`required`.

At request time, a keyed object validates every key's value against `entry`;
a `variant` value reads the discriminator, fails closed on a missing,
non-string, or unrecognized value, then validates the rest against the
matching case's fields. A value of the wrong JSON type also fails closed: a
string, array, or JSON-encoded string where a keyed map, variant entry, or
array is declared is refused before the call, never forwarded for the upstream
to 422. A `raw=true` field is exempt.

## Array bounds, extended to the body grammar

`min-items`/`max-items` - previously typed-query-only (see [inline
operations](opcore-inline.md)) - now also apply to a body `array`, enforced
both at parse time (`min-items` greater than `max-items` fails closed) and at
request time (an out-of-bounds array is refused before it reaches the
upstream). A raw array cannot also set bounds.

## What the command line cannot do

Like a mapped body (see [body projection](opcore-body.md)), a `keyed` or
`variant` field's caller-chosen keys have no fixed CLI flag name. These
grants are reached through the MCP surface, which carries nested inputs.
