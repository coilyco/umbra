# paths-within

A `gate` that refuses a call whose path operands leave a set of directories known only at run time. A glob can list paths to deny, but not "anywhere except the session shadow", because the shadow's location arrives in the environment. `paths-within` reads the environment when the call happens, so the guardfile states the rule and the launcher supplies the roots.

```kdl
wrap rm {
    exec rm
    replace
    can run "*" {
        gate paths-within {
            operands "positional"
            root-parent-env "AOS_NATIVE_SESSION_PROJECTS"
            root-env "AOS_FS_GUARD_ROOTS"
            root "/private/tmp/claude-{uid}"
            reason "work on a copy inside the shadow, or hand the operation to Kai"
        }
    }
}
```

## Options

* **`operands`** (required) - how to read the paths out of argv. `positional` is every word that is not a flag, `--` aware, for `rm`. `positional-and-target` adds the value of `-t` and `--target-directory` for `mv`, in each spelling a getopt parser accepts (`-t DIR`, `-tDIR`, `-vtDIR`, `--target-directory=DIR`, a unique prefix such as `--target=DIR`) and skips the value of `-S`/`--suffix`. `find` judges a call only when it carries `-delete`, `-exec`, `-execdir`, `-ok`, `-okdir` or an `-fprint`, `-fprint0`, `-fprintf` or `-fls` predicate. It then checks every start path (after any leading `-H`, `-L`, `-P`, `-D` and `-O`), `.` when there is none, and each file an `-fprint` family predicate names. A `find` that only reads passes anywhere.
* **`root-env`** - a variable holding a `:`-separated list of allowed directories. Repeatable.
* **`root-parent-env`** - a variable holding one directory, whose parent is allowed. A shadow holds `projects/`, `home/` and `seat/` side by side, and the variable names only `projects/`. Repeatable.
* **`root`** - a fixed directory, counted only when it exists. `{uid}` expands to the numeric uid. Repeatable.
* **`reason`** - appended to the refusal, so the recovery rides with the message.

At least one of `root-env` and `root-parent-env` is required, and anything unknown fails the build. A gate that no variable could switch on would never fire.

## What it does

* **Inert while every named variable is empty.** One binary can sit on a PATH shared with callers that never set them, such as CI jobs in the same image. A fixed `root` does not switch it on.
* **A root of `/` or the home directory is dropped**, so a wrongly set variable cannot turn "inside" into "anywhere". When every root is dropped, nothing is inside and every operand is refused.
* **A path is resolved the way `rm` and `mv` act on it.** Symlinks in the parent are followed and the leaf is left alone, because those tools act on a link rather than its target. A trailing slash follows the link. A relative path resolves against the working directory, and a path that does not exist yet resolves as far as it does.
* **Refusal is `policy_denied`**, exit 2, audited as `reject`, and reaches no binary. The refusal names the offending operand.

## What it does not do

* **It reads argv, and argv is the only thing it sees.** Shell redirection, `cp`, `tee`, `dd of=`, `rsync --delete`, `git clean`, and an interpreter calling `unlink` never pass through it. The `find` arguments after `-exec` run a command the gate cannot read.
* **It is a replacement's gate, so it is UX over an enforcement floor umbra does not own.** `/bin/rm` walks around a shim, as [the replacement doc](execverb-replacement.md) says.
* **[Negative controls](negative-controls.md) do not cover it.** `umbra controls` invokes `never`, `cannot` and `withhold` rules, and a gate's answer depends on the environment it runs in, so the tests beside the gate carry that proof.
