// Separate module so the runtime library does not pull cli-web-docs
// (and its goldmark/cli-docs deps) into the parent go.mod.
module github.com/coilyco/umbra/scripts/gen-webdocs

go 1.25.5

require (
	github.com/coilyco/umbra v0.0.0-00010101000000-000000000000
	github.com/coilysiren/cli-web-docs v0.0.0-20260513172246-d202b1f723a1
	github.com/urfave/cli/v3 v3.9.0
)

require (
	github.com/cpuguy83/go-md2man/v2 v2.0.2 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/urfave/cli-docs/v3 v3.1.0 // indirect
	github.com/yuin/goldmark v1.8.2 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)

replace github.com/coilyco/umbra => ../..
