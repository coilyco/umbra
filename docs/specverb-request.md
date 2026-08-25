# specverb request semantics

How the generic action behind every mounted leaf assembles, previews, and fires its request. Engine and policy layers are in [specverb.md](specverb.md).

## Inputs

- **Path params** become positional args, count-validated before any wire call. **Query params** become typed flags: scalars encode once, arrays as repeated keys in input order, unset values omitted.
- **Body fields** become typed flags; an unset optional is omitted rather than sent as a zero value, and arrays repeat the flag. **`--body-file`** supplies the whole body instead. Required fields are enforced at assembly rather than in the CLI layer, so either source satisfies them.

A local input shadowing a reserved engine flag (`--dry-run`, `--query`, `--output`, `--body-file`), or a query/body collision on one leaf, refuses to build rather than shadowing silently.

## A mapped body projects strings, and only strings

A `body` block written as `map "source.path" to="target"` puts a **string** on the wire at every mapped leaf, whatever the caller supplies. The projected value comes from `mappedString`, so an upstream requiring an object, number, or boolean at a mapped parameter is unreachable through `map` in every configuration.

Nothing surfaces this at authoring time. A guardfile mapping a parameter the upstream wants as an object parses, builds, ships, registers its tool, and then fails on every call with the upstream's own error. Measured against Exa's `/search`, varying one parameter: absent is 200, `contents={"text": true}` is 200, and `contents="text"` is **HTTP 400** with `expected object, received string`. That 400 is the first and only notice, and it arrives in production against a metered API.

It is easy to reach without choosing it. `map` is the only construct that renames an input to a different upstream key, because a body field carrying an upstream alias is refused outright. So a guardfile whose upstream needs a parameter name colliding with a reserved engine flag is forced into mapped-body mode for that leaf, and mapped-body mode then forbids every non-string value on that leaf, including parameters unrelated to the collision that forced it. **Neither rule states the combination, and an author reading either alone would not predict it.**

Writing a shape onto a `map` is refused at parse time with the limit named, so the common mistake is a build error rather than a runtime 400. Typed mapped leaves are the real fix and are not built: `BodyMapping` would need a declared type and `projectMappedBody` a typed accessor. See coilyco-flight-deck/umbra#312.

## The shell-metachar gate is location-aware

`verb.Wrap` → `policy.ValidateArg` refuses shell metacharacters, but only on inputs composing into the request **URL**, the injection surface. Path params and query flags stay gated, each element of a repeated parameter independently. Body and form fields and `--body-file` are encoded into the body and never reach a shell or the URL, so they are exempt: gating them mangled legitimate free text.

## Firing

Auth resolves the secret through the value-provider registry. `--dry-run` prints the resolved request with the secret redacted and fires nothing. Live responses render through the `--query`/`--output` rail, and an empty 2xx prints an `ok:` line. The client refuses redirects for mutating methods, so a renamed target cannot silently swallow a write.

A wrap may declare `header "<name>" "<value>"`, applied to every leaf, which is how an author states the contact address some APIs ask for in an agent. `Authorization` is refused, since `auth` owns it and a second path would be an unreviewed credential surface, and so is `Content-Type`, which the runtime sets from the body and would silently overwrite. A duplicate name, an empty value, and a wrong argument count fail closed.

Absent a declared one, every request carries `opcore.DefaultUserAgent`. Naming your client is the stated etiquette for the volunteer and nonprofit APIs a Guardfile reads.

## Non-JSON responses

An operation whose success response offers no JSON writes its body to stdout byte for byte. The spec-driven path infers this from the declared media type, and an inline grant says it outright with a bare `raw-response` node. A response listing JSON beside something else is negotiating content rather than declaring bytes, so it is parsed.

Both paths choose **before** firing rather than after reading, the whole of umbra#289: decoding first fails on a plaintext or ZIP body, leaving the raw branch unreachable. Only the decode is skipped, never a gate, and `--query` is refused rather than ignored.

Fixed non-Swagger leaves live in [fetch overlays](specverb-fetch.md).
