# default-allow

Every other shape in the exec dialect is closed: a verb the guardfile does not name is not there. `default-allow` inverts that one wrap. An unnamed verb is forwarded to the wrapped binary, and the guardfile names only what it refuses or constrains.

```kdl
wrap gh {
    exec gh
    replace
    default-allow {
        reason "gh is a read tool with one boundary on it, so enumerating the rest is upkeep that buys nothing"
    }
    can run api { allow-flag "--cache" }
    withhold pr create {
        reason "pull requests go through Forgejo, which is canonical; GitHub is a read-only mirror"
    }
}
```

`gh pr create` is refused with its reason. `gh api --method POST` is refused by the grant's flag policy. `gh workflow run`, `gh repo clone`, and everything else the tool has reach the real binary untouched.

## Why the reason is mandatory

It is the only node here that a `can run` grant does not already have an equivalent of, and it is required for the same argument `withhold` requires one: this is a decision, and an undefended decision reads as an accident to whoever opens the file next. A wrap inverting the fail-closed default without saying why is worse than one that never inverted it, because the reader cannot tell which it was.

## What it does not open

**The wrap-level guards still bind.** A forwarded call faces `only pass`/`never pass` and the same env injections a granted leaf faces, resolved before anything is spawned. Anything else would make declaring a host gate decorative the moment this keyword appeared, which is the failure mode a default-allow keyword invites.

**A named grant is still a tightening.** `can run api { allow-flag ... }` under default-allow means `api` is reachable **and** constrained, where an unnamed sibling is reachable and unconstrained. Naming a verb is how you take something away here, not how you add it.

**The audit still records every granted call.** A forwarded call is not a granted one, so it carries no grant name.

## Refused shapes

Three, all fail-closed at parse:

* **Beside `can run *`.** The funnel already forwards the whole binary and answers first, so the declaration would name a default nothing reads.
* **Beside an `allow` inspect list.** Same reason, once per binary.
* **Naming neither a grant nor a stub.** That is `passthrough <bin>` spelled the long way. Declare that instead and the reader knows at a glance that nothing is policed.

## Under `replace`

This is where it earns most. A [replacement](execverb-replacement.md) occupies the tool's own name, so without default-allow the guardfile has to enumerate every verb anyone might use or the tool loses them, and that inventory rots against the upstream tool's releases rather than against your policy.

Two behaviors change under the pair:

* A pre-verb flag forwards rather than being refused, so `gh --version` works. Refusing it is a stated limit of a closed replacement (`teable:coilyco-flight-deck/umbra#7324`), and it is a limit of the closed default rather than of replacements.
* A flag belonging to an unnamed verb under a **named** group forwards. A group parses its own flags before reaching the unmatched-name path, so `gh pr view 12 --json title` would otherwise die on `--json` purely because a sibling `pr create` is withheld.

## A lint note

`default-allow` gave gosec's taint analysis a path it could follow from the caller's argv to `exec.CommandContext`, so G702 started firing on the shared runner every granted leaf already reached. It is excluded beside G204, which this repo has always excluded for the same reason: both findings say "execs a variable binary with caller arguments", which is the definition of a guarded wrapper rather than a defect in one. The binary is fixed at parse, and the arguments are policed before the sink.

## The honest limit

Default-allow makes the boundary the thing you wrote down, rather than the thing you remembered to enumerate. That is a readability and maintenance win, and it is also strictly less containment: a verb upstream adds tomorrow is reachable tomorrow. Reach for it where the tool is broadly safe and the boundary is narrow. Where the tool is broadly dangerous, enumerate, and take the upkeep.
