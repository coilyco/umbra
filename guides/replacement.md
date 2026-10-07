# replacement

An occluded replacement is a generated CLI installed onto PATH under the name of
the tool it stands in front of. A caller runs `git status` and never learns
umbra is there.

The [quickstart](quickstart.md) had you build one and watch it refuse four
different ways. This is the same artifact taken seriously: what the guardfile is
actually saying, what the trail looks like afterwards, what it does not stop,
and what else on your machine notices.

Paths are shortened. Refusal text and exit codes are verbatim.

## What `replace` changes

Without it, `umbra build` produces a binary with its own name that you call
directly. The caller knows it is calling a guarded thing.

```kdl
wrap example git {
    exec git
    replace
```

With `replace`, `umbra install` writes that binary onto a PATH directory under
the name it occludes. Nothing at the call site changes. Scripts, editors,
tooling and agents keep saying `git`, and what answers is policy.

That is the property worth having and it is also the whole risk, which is the
last two sections.

## Reading the guardfile

Eighteen lines, and every refusal you met comes from one of them.

```kdl
    can run status
    can run log
    can run diff
    can run commit {
        deny-flag "--no-verify" aliases="-n"
    }

    withhold push {
        reason "This example occludes a local git: nothing here should reach a remote."
        alternative "status"
    }
```

* **`can run`** grants a verb. Four here, out of git's hundred and forty.
* **`deny-flag`** grants the verb and refuses one way of calling it. `commit`
  works and `commit --no-verify` does not, because the point of that flag is to
  skip the checks the grant exists to preserve. `aliases="-n"` lists git's short
  spelling of it: umbra cannot read git's alias table, so a `deny-flag` that does
  not list its aliases, or say `aliases="none"`, fails to parse.
* **`withhold`** names a verb in order to refuse it out loud. The `reason` and
  the `alternative` are what a caller sees, which is why `git push` tells you to
  use `status` and `git rebase` tells you nothing.

Everything absent from those lines is absent from the binary. A guardfile grants
rather than forbids, so the length of your deny list is not what makes you safe.
Not writing a grant is.

## The trail

Every call that reaches the wrapped binary writes a row, refusals included. Over
a session that is a record of what was attempted rather than of what succeeded,
which is the more useful half when the caller is an agent.

The [primitives guide](primitives.md) covers the row's shape. Two things are
specific to a replacement.

**`verb` is the guardfile's name, not the binary's.** Rows read
`example.git.status`, not `git status`. Install two replacements from the same
guardfile family and their rows stay comparable.

**A withheld verb writes nothing.** `git push` is mounted as a stub that reaches
no binary, so there is nothing to wrap. If you are counting attempts from the
trail, withheld verbs are invisible in it. `--help` is where they are visible.

## Finding out what stands there

A replacement is invisible on purpose, so a caller who lands on one has no way
to ask what it is. One surface answers that, and it is the only one:

```sh
UMBRA_IDENTIFY=1 git
```

```
umbra replacement for "git"
  guardfile: example git
  wrapped binary: git
  driver version: dev
  real binary: /opt/homebrew/bin/git
```

It is worth knowing before you debug a host you did not install this on, because
every other surface here is busy being a convincing `git`.

## What it does not stop

The real git is one absolute path away, and this guide will not pretend
otherwise.

```sh
umbra doctor --shim-dir ./shims
```

```
ok   installed                shims/git
ok   wins on PATH             "git" resolves to the replacement
ok   real binary              /opt/homebrew/bin/git
warn still directly reachable /opt/homebrew/bin/git runs without passing through the replacement, so a PATH shim is what a caller sees rather than what a caller can reach

3 of 4 checks hold. The last one never does, and says why.
```

Three of four is the healthy state. The fourth check is not a warning you can
clear. It is the tool declining to overstate itself, and it is worth reading
once slowly.

**A replacement is what a caller sees rather than what a caller can reach.**
Anyone who types the absolute path, or who resolves `git` some other way, gets
the real one. That makes this a control against an honest caller taking a wrong
turn, and against an agent doing what its PATH suggests. It is not a control
against someone who wants around it.

If you need the stronger thing, the enforcement floor lives below umbra: a
container that does not contain the real binary, a filesystem permission, a
seccomp policy. umbra does not own that layer and does not claim to.

## What else notices

This is the part the quickstart did not tell you, and it is the real cost of
the property that makes replacements useful.

**Everything on that PATH sees your replacement, including things you did not
think about.** An editor's source-control panel, a build script, a pre-commit
hook, a language server, a test harness. They all say `git`, and after the
install they are all talking to a binary that grants four verbs.

That is not a bug. It is the feature, observed from the other side. But it means
the blast radius of `umbra install` is your whole PATH rather than your own
typing, and the failures it causes are quiet: a tool that shells out to a verb
you did not grant gets a refusal it was not written to expect, and reports
something unhelpful.

umbra's own internals hit exactly this, and the blast radius is the instructive
part. A shared package located the repository root by running `git rev-parse`,
and it is called on every guarded invocation. So one installed `git` replacement
degraded the `repo_root` field in the audit rows of **every umbra binary on that
host**, including wrappers with nothing to do with git. A wrapper for one tool
reached into the telemetry of unrelated wrappers.

Nobody noticed for a while, because `repo_root` went empty rather than erroring,
and empty is also what you get when you legitimately run outside a repository. A
failure that is indistinguishable from a normal value cannot be caught by a
test.

Resolution now walks the filesystem, so nothing on PATH can intercept it. The
interception is fixed. The property that let it go unnoticed for as long as it
did is still there by design, so the next cause of an unresolvable root will
also fail quietly. That is the shape worth remembering rather than the instance.

So install a replacement onto a directory you control and put it on PATH
deliberately, for a shell or a session or a container, rather than into a login
profile and then forgetting. `umbra doctor` is how you find out what is true on
a host you are not sure about.

## Removing it

```sh
rm -rf shims
```

Take the directory off PATH and the real tool answers again. Nothing was
modified in place and nothing needs uninstalling, because a replacement is a
file in a directory that wins a lookup.

## Where to go next

* **[mcpverb](mcpverb.md)** - the same policy against MCP tools rather than a
  local binary.
* **[mcpverb-cli](mcpverb-cli.md)** - the driver path, with a committed lock
  against a server this repository does not control.
