# sandbox: the namespace jail and its degrade path

`cli/sandbox` jails a gate-spawned child in a Linux **user + mount namespace** so
the tool and its descendants re-enter the gate (they can't reach a wrapped binary
without hitting the shim) and a seccomp denylist blocks escape syscalls. It is a
no-op off Linux. The jail is applied by `shell.Runner.Exec` when the runner
carries a non-nil `sandbox.Spec`; read-only `Capture` calls are never jailed.
When a wrapped tool is `$0`-sensitive, the jail places its `.<tool>.cliguard`
exec symlink in a private tmpfs view of the canonical directory so the host
filesystem stays clean.

## When the environment can't jail

Creating the namespace needs privileges some environments deny. Two failure
modes were hit in the field:

- **Mode 1 - clone denied.** `CLONE_NEWUSER` is blocked (a container without
  userns, some restricted agent shells), so the jailed child never starts:
  `fork/exec <bin>: operation not permitted`.
- **Mode 2 - make-rprivate denied.** The namespace is created but the in-child
  `mount --make-rprivate` is refused: `sandbox: make-rprivate: permission denied`.

Without handling, every sandboxed verb dies in these environments - even though
the environment is *already* an isolation boundary (a container) or one where the
jail adds little.

## Auto-degrade covers both denial modes

An environment that cannot host the jail degrades to an unsandboxed run on its
own. No consumer needs to declare anything for umbra to work.

**Mode 1, clone denied.** The jailed child never starts: `cmd.Run` returns
`EPERM`/`EACCES` with a nil `ProcessState`, `sandbox.SetupDenied` reports it, and
`shell.Runner` retries unsandboxed.

**Mode 2, setup denied inside the child.** The namespace is created and the child
starts, then a mount, capability, or seccomp step is refused. Because the child
ran, `ProcessState` is non-nil and `SetupDenied` cannot see it. The child instead
reports the refusal on a sentinel fd that `Wrap` attaches through `ExtraFiles`,
and `shell.Runner` takes the same retry path.

Both retries are side-effect-safe by construction: every setup step runs before
`execJailTarget`, the last statement in the jail helper, so an error means the
wrapped tool never ran and the retry cannot double an effect.

Only `EPERM`/`EACCES` degrade. A malformed spec, a missing path, or a read-only
filesystem still fails loudly rather than silently dropping the boundary. The
sentinel is marked close-on-exec before the target is exec'd, so a wrapped tool
cannot inherit the fd and forge a denial.

## The opt-out env is optional

`CLIGUARD_NO_SANDBOX=1` (also `true`/`yes`/`on`) makes `Wrap` no-op up front. It
is a **deterministic opt-out with no per-exec retry cost**, not a correctness
requirement - auto-degrade now covers the cases that used to need it.

Reach for it only where the jail is redundant by design and you want to skip the
retry entirely, e.g. `ward container`'s entrypoint, where the container itself is
the boundary.

## Choosing

- On a normal host: leave it unset; the jail applies.
- Anywhere the jail may be denied: leave it unset; auto-degrade handles it and
  warns once.
- In a **container that is already the isolation boundary**, if you want to skip
  the per-exec retry: set `CLIGUARD_NO_SANDBOX=1`.

## See also

- [docs/architecture.md](architecture.md) - where the sandbox sits in the gate.
- [docs/SECURITY.md](SECURITY.md) - the boundary's threat model.
