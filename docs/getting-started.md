# Getting started

Two ways in. Pick the one that matches what you are building, then run the
refusal at the bottom so you have watched the boundary hold once.

## Install umbra

`umbra` reads KDL policy plus committed locks and builds a standalone guarded
CLI, with no hand-written Go.

```sh
brew tap coilyco/tap
brew install coilyco/tap/umbra
```

```powershell
scoop bucket add coilyco https://github.com/coilyco/scoop-bucket
scoop install coilyco/umbra
```

Tagged releases also publish raw binaries and `SHA256SUMS` for Linux, macOS, and
Windows on amd64 and arm64. umbra shells out to the Go toolchain to resolve
locks and build, so Go has to be present.

## Or import the primitives

Every package stands alone if you are adding a boundary to an existing
[urfave/cli](https://github.com/urfave/cli) v3 app.

```sh
go get github.com/coilyco/umbra
```

## See a refusal

umbra rejects a shell metacharacter before `execve`. The gate section of
[`guides/primitives.md`](../guides/primitives.md) runs it and shows the refusal
verbatim.

[`guides/`](../guides/) holds one walkthrough per surface, from the minimum
useful program up to the network gate.

## Next

[The no-code driver](umbra-cli.md) covers authoring the guardfile.
[Architecture](architecture.md) covers the two surfaces underneath it.
