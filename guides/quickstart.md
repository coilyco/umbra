# Quickstart

From an empty terminal to a `git` that can read your repository and cannot push
it. Nothing here assumes a checkout of this repository. You will write one
eighteen-line file and run three commands. Paths below are shortened and yours
will differ, while refusal text and exit codes are verbatim.

## What you need

* **Homebrew or Scoop**, to install umbra itself.
* **A working Go toolchain.** umbra generates a consumer binary and builds it,
  so `lock`, `build`, `run` and `install` all shell out to Go. Nothing in umbra
  demonstrates itself without a build.
* **About ten minutes.**

## 1. Install umbra

```sh
brew tap coilyco/tap
brew install coilyco/tap/umbra
```

```powershell
scoop bucket add coilyco https://github.com/coilyco/scoop-bucket
scoop install coilyco/umbra
```

Check it landed:

```sh
umbra --version
```

```
umbra version v0.212.1 (umbra ref v0.212.1)
```

A version built from source rather than from a release tag prints a
pseudo-version like `v0.212.1-0.20260910090723-0fe835358791`. Either is fine.

## 2. Write the guardfile

A guardfile is the whole project. Make a directory and put one file in it.

```sh
mkdir -p demo/.umbra && cd demo
```

`.umbra/git.guardfile.kdl`:

```kdl
description "A git that only reads, plus a commit that cannot skip the hooks."

wrap example git {
    exec git
    replace

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
}
```

Four verbs granted out of git's hundred and forty. One flag denied on one of
them. One verb withheld with a reason attached. Everything else is unmentioned,
and unmentioned is the interesting part.

## 3. Lock and install

```sh
umbra lock
```

```
umbra: locked specverb.lock (umbra v0.212.1)
```

`lock` freezes the umbra module version this project builds against. It is yours
rather than this repository's, so it is a step you run rather than a file given.

```sh
umbra install --shim-dir ./shims
```

```
umbra: installed git as shims/git
umbra: put ./shims ahead of the real git on PATH, then `umbra doctor --shim-dir ./shims`
```

## 4. Put it in front of the real one

```sh
which git
```

```
/opt/homebrew/bin/git
```

```sh
export PATH="$PWD/shims:$PATH"
which git
```

```
./demo/shims/git
```

That is the whole mechanism. A caller runs `git` and reaches the replacement,
and nothing about the call site changed.

```sh
git --help
```

```
NAME:
   git - git, occluded by umbra to the verbs example git grants

USAGE:
   git [global options] [command [command options]]

COMMANDS:
   status   exec: git status
   log      exec: git log
   diff     exec: git diff
   commit   exec: git commit
   push     NOT AVAILABLE - withheld by policy. This example occludes a local git: nothing here should reach a remote. Use `status` instead.
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --help, -h  show help
```

Four verbs, one stated refusal, and no sign of the other hundred and thirty-five.

## 5. Find out what it refuses

Each of these is a different kind of no. Every refusal exits 2, so a caller tells
a refusal from a tool failure without reading English, and the message says which
kind of refusal it was.

```sh
git status --short
```

```
?? a.txt
```

```sh
git commit --no-verify -m x
```

```
git: flag "--no-verify" is denied for `commit`
```

Exit 2. The verb is granted and the flag is not, so policy refused it.

```sh
git push
```

```
git: `push` is withheld: This example occludes a local git: nothing here should reach a remote.
```

Exit 2, and it says why, because the guardfile gave it a reason to say.

```sh
git rebase
git fetch
```

```
git: `git rebase` is not granted; run `git --help` for the verbs this binary has
git: `git fetch` is not granted; run `git --help` for the verbs this binary has
```

Both exit 2, and neither says why, because there is nothing to say. Neither was
mentioned in the guardfile, and every unmentioned verb answers identically.
**Deny is absence.** A reader of `--help` cannot work out which verbs exist
upstream, and an agent spends no context on a verb it may not call.

`fetch` is the one worth sitting with. It only reads, so a reader who has
followed the grants so far expects it to work. It does not, because the
guardfile never named it, and a guardfile grants rather than forbids. Nothing is
denied here. Four verbs were granted and everything else is not there.

## 6. Check your work

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

Three of four is the healthy state, and the fourth is not a problem you can fix
by trying harder. [replacement](replacement.md) gives it its own section. If any
check reads `warn`, `doctor` names the cause and what to do about it, which is
worth preferring over a troubleshooting table here.

## Clean up

```sh
rm -rf demo
```

Nothing else was written outside it.

## Where to go next

The guides build on each other, so this is a reading order rather than a menu.

* **[primitives](primitives.md)** - audit rows, the metacharacter gate, and the
  exit-code taxonomy the codes above come from.
* **[mcpverb](mcpverb.md)** - the same policy over MCP tools instead of a local
  binary, against a server the guide starts itself.
* **[mcpverb-cli](mcpverb-cli.md)** - that dialect the product way, with KDL and
  a committed lock against the protocol's reference server. Needs `npx`.
* **[replacement](replacement.md)** - what you built here, taken seriously: the
  audit trail, the refusal ladder, and the enforcement floor umbra does not own.
* **[mcpapps](mcpapps.md)** - the MCP Apps host bridge, and what a widget is
  refused. Read this last.
