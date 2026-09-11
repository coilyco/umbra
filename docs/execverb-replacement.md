# occluded replacement binaries

The [occlusion primitives](execverb-occlusion.md) shape one call. A replacement shapes the tool. `umbra install` writes a generated binary onto a PATH directory under the wrapped tool's own name, so a caller types `git commit -m x` and the guardfile's grants are the whole `git` they can see. What was not granted is not refused-looking. It is not there.

Everything else in the exec dialect is unchanged: the grants, guards, flag policy, gates, actions, and audit rows are the ones [execverb.md](execverb.md) describes. Only the shape of the binary moves.

## Declaring one

```kdl
wrap aosguard git {
    exec git
    replace
    can run status
    can run commit { deny-flag "--no-verify" }
    withhold rebase {
        reason "A rewritten history cannot be reconstructed from this audit log."
        alternative "commit"
    }
}
```

`replace` takes the name from the wrapped binary's basename, because a replacement stands in front of a binary on PATH rather than in front of a verb group. `replace "<name>"` overrides it, for the case where `exec` names a path or a different spelling.

Three things fail closed at parse or plan:

- **`replace` beside an `allow` inspect list.** That list mounts one funnel per binary and names no single tool to stand in for.
- **A name carrying a path separator.** The name becomes a filename on a PATH directory, so a separator would install somewhere the consumer did not name.
- **A replacement merged with a second member.** A replacement binary *is* the tool, and a merged member would mount verbs that tool does not have. `--binary` disagreeing with the occluded name is refused for the same reason: the name is policy, not a publishing choice.

## The part that is load-bearing

A replacement wins PATH under the wrapped tool's own name, and the exec path resolves a bare name through that same PATH. Left alone, a binary named `git` execing `git` finds itself, which is a fork bomb rather than a refusal.

Two guards, and both are needed:

- **Resolution excludes the wrapper.** `ResolveReal` walks PATH itself, skipping the replacement's own directory and any candidate that is the running executable by inode, following symlinks. When nothing survives it refuses rather than running, as an `internal` exit rather than a policy one: nothing about the call was denied, the host holds no copy of the tool.
- **The child's PATH loses the shim directory.** Resolution alone is not enough, because the wrapped tool, or another wrapper in front of it, re-resolves its own name and lands back on the replacement. That is not hypothetical. Installing a `git` replacement on a host already carrying a second `git` shim, one that strips only its own directory, hangs: each wrapper skips itself, finds the other, and loops.

The second guard has a stated cost. Everything a replacement spawns sees the shim directory gone, so a sibling replacement installed beside it is absent from that subprocess tree's PATH. A wrapper a wrapped tool re-enters is a hang rather than a boundary, and a shim was never the boundary anyway (below).

## What the caller sees

- **`--help` renders the granted subset.** This is the occlusion, not a summary of it. A withheld verb still appears, leading with `NOT AVAILABLE`, because a stated refusal is the point of `withhold`.
- **An ungranted verb is refused, naming umbra.** Exit 2, `policy_denied`, pointing at `--help`. Under occlusion the wrapper cannot tell a denied verb from a misspelt one, so it says what it does know: this name is not granted here.
- **A flag before the verb is refused.** A replacement occupies a name whose entire flag namespace belongs to the tool, so it registers none of its own, `--version` included. A flag goes after the verb that takes it.
- **`UMBRA_IDENTIFY=1 git` states what stands there**, and is the only surface that does. It cannot be a flag, for the reason above.

## Installing and checking

```sh
umbra install --shim-dir ~/.local/umbra/shims
umbra doctor  --shim-dir ~/.local/umbra/shims
```

`install` is `build` with the destination filename fixed by the guardfile. `doctor` changes nothing and reports four things: whether the replacement is installed, whether the tool's name resolves to it on this PATH, which binary a granted call would reach, and that the same binary is still directly reachable without the replacement.

That last finding never passes, deliberately.

## What this is not

**A replacement is UX over an enforcement floor umbra does not own.** A PATH shim is recoverable by spelling the real binary's path, and for a same-user agent that is one string away. Unix permissions provide no parent-process authorization for a same-user invocation, so nothing here contains a caller who declines to be occluded.

The enforcement floor is ownership plus the absence of passwordless sudo: the real binary not executable by the agent's user, and no way for that user to elevate. Both belong to the consumer's host convergence rather than to umbra. This mirrors the README's refusal to call umbra a sandbox, one layer down, and it is the doctrine the deleted `cli/shim` deny-shim generator carried before it (`teable:coilyco-flight-deck/umbra#1264`).

What a replacement does buy is real and worth having: every granted call is validated and audited, the ungranted surface is invisible rather than merely refused, and the caller needs to know nothing about umbra to get all of it.

## Limits

Both of these are limits of the closed default rather than of replacements:
[`default-allow`](execverb-default-allow.md) forwards a pre-verb flag and every
unnamed verb, so a replacement can name only its boundary.

- **Pre-verb global flags are not mounted.** `git -C /elsewhere status` is refused, because the flag arrives before any grant claims it. Tracked at `teable:coilyco-flight-deck/umbra#7324`.
- **One tool per binary.** The busybox shape, one binary dispatching on `argv[0]` behind a symlink farm, is not what this builds.
