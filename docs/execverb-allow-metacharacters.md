# allow-metacharacters

Every granted exec leaf runs the shell-metacharacter gate over the caller's argv before anything else, and the gate refuses `` ` `` `$` `;` `&` `|` `<` `>` `(` `)` `{` `}` `\` and whitespace controls. The gate exists for a wrapped binary that forwards argv into a shell. For a binary that receives argv through `execve`, it refuses ordinary input: a commit message `feat(scope): ...` fails on `(`, and a `Co-Authored-By: Name <mail>` trailer fails on `<`.

```kdl
wrap git {
    exec git
    replace
    default-allow { reason "..." }
    allow-metacharacters {
        reason "git receives argv through execve and never a shell, so the bytes are data."
    }
    can run commit { deny-flag "--no-verify" aliases="-n" }
}
```

## What it lifts, and what it does not

**It lifts one gate, for every leaf of the wrap.** The audit row stamps the skipped policy, so forensics still see it. Every other refusal binds the same call: `deny-flag`, `allow-flag`, `deny-when`, `never pass`, `never run`, and the wrap-level guards. A `commit --no-verify -m "feat(x): y"` is still refused by the flag policy.

**Forwarded calls were never gated.** Under `default-allow` an unnamed verb is forwarded with the gate skipped already, so a wrap that names `commit` to constrain it was the only place the gate fired. The declaration makes the named verbs match the unnamed ones.

**The reason is mandatory.** The guardfile has to say why this binary never hands argv to a shell. A wrap that forwards into `ssh`, `kubectl exec` or `ssm send-command` does not qualify, which is what the gate is for.

## Refused shapes

Fail-closed at parse: a bare declaration, an empty block, a `reason` that is missing, duplicated or takes more than one string, any other child, and any argument or property. The HTTP dialect's `allow-metacharacters "<param>"` names path params and is a different node on a different dialect.

## Scope

Wrap-level only. An `allow` inspect list carries it onto each funnel. An `action` step still runs its own metacharacter check on resolved args, which this does not lift.
