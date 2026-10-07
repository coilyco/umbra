# Primitives

Every umbra binary is built on three things: a row it writes, a gate every
argument passes through, and an exit code a caller can act on without reading
English. The quickstart had you build one. This is what it was doing underneath.

You need the `git` replacement from [the quickstart](quickstart.md). Nothing new
is installed and nothing new is written. Paths are shortened, and refusal text
and exit codes are verbatim.

If you arrived here by importing the packages rather than by building a
replacement, the gate and the exit codes below are package behavior and read
standalone. Only the first section needs the quickstart's binary, because it is
about a row that something has to write.

## The row

Run a granted verb and a refused one:

```sh
git status --short
git commit --no-verify -m x
```

Both wrote a row. The granted one:

```json
{
    "id": "01a08aac-c534-720f-8072-0b1d4566676a",
    "ts": 1789032973,
    "version": "v0.31.0",
    "decision": "accept",
    "verb": "example.git.status",
    "argv": ["git", "status", "--short"],
    "exit_code": 0,
    "duration_ms": 84,
    "repo_root": "./demo",
    "cwd_subprocess": "./demo"
}
```

And the refused one:

```json
{
    "id": "01a08aad-6350-7851-9fb0-1da6b7802d3d",
    "ts": 1789033014,
    "version": "v0.31.0",
    "decision": "reject",
    "verb": "example.git.commit",
    "argv": ["git", "commit", "--no-verify", "-m", "x"],
    "exit_code": 2,
    "error": "flag \"--no-verify\" is denied for `commit`",
    "repo_root": "./demo",
    "cwd_subprocess": "./demo"
}
```

A call that reaches the wrapped binary writes a row whether it succeeded or was
stopped on the way. That is the property worth having. A log that only records
what happened tells you nothing about what was attempted, and what was attempted
is the more interesting half when the caller is an agent.

`verb` is the guardfile's name for the call rather than the binary's, so
`example git` plus `commit` becomes `example.git.commit`. That is what makes
rows from different binaries comparable.

`version` is the build that wrote the row: the `-ldflags` version of a generated
binary (`dev` when unstamped), else the module version Go linked in, else
`(devel)`. Rows from before the field existed have none. Select by build with
`audit.ByVersion`, or with `audit.AtOrAfter("v0.31.0")` for every row from that
release on, applied through `audit.Filter` to what `audit.ReadAll` returns. A row
whose version is not a semver tag, `dev` and `(devel)` included, is never
selected by `AtOrAfter`.

The refused row differs from the granted one in three fields: `decision` is
`reject`, `exit_code` is 2, and `error` says why. Nothing else changes shape, so
a reader parsing these does not need two schemas. A refusal by the guardfile,
whether a gate, a guard, flag policy, a pin, a seal, a `never run` or a
`withhold`, exits 2 and writes a `reject` row, so filtering on `reject` finds all
of them. An unknown verb writes a `reject` row too, with `exit_code` 5, because a
misspelt verb and a denied one are deliberately not told apart. Read the rows you
get yourself for the full set.

## The gate

Every argument goes through `policy.ValidateArg` before `execve`, and any string
containing one of these bytes is rejected:

```
`  $  ;  &  |  <  >  (  )  {  }  \  \n  \r  \t
```

The rejection is stable and parseable:

```
policy: shell metacharacter rejected: arg positional[0] contains ';' at index 3
```

Exit 2.

**The reason this exists is not the one you would guess.** umbra always builds
an explicit argv slice and never invokes `/bin/sh`, so there is no shell here to
inject into. The gate is about the next hop. A non-trivial fraction of
downstream tools hand their last positional argument to a remote shell:

```sh
ssh user@host '<remote-command>'
kubectl exec pod -- sh -c '<command>'
git config --global core.editor '<editor-cmd>'
```

If the thing driving umbra never sanitises its inputs and the wrapped tool
unsplats argv into a shell on the other side, one semicolon turns a benign verb
into a chained command two hops away. Rejecting the metacharacters at this
boundary keeps a one-layer leak from becoming an execution surprise somewhere
you are not looking.

**Do not retry a rejection with a quoted or escaped variant.** The input is
hostile by definition, and the escape is the attack. Surface it instead.

## The exit code

The codes are a stable contract. Orchestrators, CI steps, watchdogs and retry
loops match on them to choose between retry, abort and handoff without parsing
stderr.

* **0, Success** - the verb ran and the underlying tool returned without error.
* **1, Generic** - catch-all. Prefer a typed code over this.
* **2, PolicyDenied** - the pre-flight rejected the call, whether a
  metacharacter, a missing required argument, or a deny rule. **The underlying
  tool was never invoked.**
* **3, UpstreamFailed** - the wrapped tool ran and returned non-zero. Its stdout
  and stderr flow through.
* **4, Internal** - a consumer-internal failure such as a config load, a
  manifest miss, or a failed audit write. Distinct from PolicyDenied because the
  user cannot fix it.
* **5, UserError** - wrong input that was not a metacharacter
  rejection, such as a missing flag or an unknown verb. Distinct from
  PolicyDenied so a caller can tell "you typed it wrong" from "policy says no".

The full statement, with the retry behavior attached to each code, is published
in the CLI reference under `site/cli/exitcode/`. Read that rather than this list
when you are implementing against it, because that one is generated from the
code and this one is prose.

### What a caller should do with each

This is the half that makes the codes worth having.

* **2 is never retryable.** The argv is hostile or malformed in a way the gate
  refuses to forward. Surface it to an operator. Do not escape and try again.
* **3 is sometimes retryable**, with the same argv, and only if the underlying
  tool's own exit suggests something transient like a network blip or lock
  contention. umbra does not retry for you. Read stderr and decide.
* **4 is not retryable on this host.** Report a bug, try another host, or wait.
* **5 is not retryable from automation.** A person needs to fix the input.

A code earns its place only when a caller can act differently on it. That is why
there are six rather than sixteen. A single rejection class with a structured
error is more useful than a fan-out of codes nobody branches on.

## Two refusals that look identical and are not

You met one in the quickstart:

```
git: `git rebase` is not granted; run `git --help` for the verbs this binary has
```

Exit 2. The verb was never granted, so policy refused it.

The `mcpverb-cli` guide has the other, on a generated binary:

```
unknown verb "get-env" under "everything"; run --help for the verbs this binary grants
```

Exit 5. Same fact underneath both: the guardfile did not grant that name.
Nothing consults the real binary before refusing, so neither answer depends on
whether the verb exists upstream.

The codes differ because the two binaries are answering different questions.

* **A generated binary exits 5** because it refuses to guess. A denied verb and
  a misspelt one land in the same place, and it deliberately does not say which.
  5 is UserError: you supplied a name this binary does not have. A flag the
  guardfile denies on a verb it does grant is different. The binary knows the
  verb, so that refusal is policy and exits 2.
* **A replacement exits 2** because a caller under occlusion has no other view
  of the tool. Absence cannot read as a typo when the replacement is
  the only `git` you can see, so it is stated as policy instead.

**For a caller deciding what to do next, this distinction does not matter.** Two
and five are both non-retryable. Two says surface it to an operator and do not
escape and retry. Five says automation cannot fix it and a person must correct
the input. What differs is who is expected to act, not whether to try again.

One trap worth knowing on a replacement. Exit 2 covers both an ungranted verb
and a withheld one, and only the message tells them apart. `git push` carries a
stated reason because the guardfile gave it one. `git rebase` carries none
because there is nothing to state. Branching on the code alone will merge them.

## Where to go next

* **[mcpverb](mcpverb.md)** - the same three primitives applied to MCP tools
  rather than a local binary.
* **[replacement](replacement.md)** - the quickstart's `git`, taken seriously.
