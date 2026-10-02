# mcpverb

The quickstart put policy in front of a binary on your PATH. This puts the same
policy in front of an MCP server's tools, and the server is one the program
starts in its own process, so there is no upstream to find and no network to
reach. Read [primitives](primitives.md) first: the row, the gate and the exit
codes are the same three, and only the transport under them changed. You need a
Go toolchain and nothing else, and refusal text and exit codes are verbatim.

## Three tools, three fates

The server advertises three tools, and the policy mentions two.

```kdl
wrap example ops demo {
    mcp http { url "%s" }
    can call list_issue {
        deny owner matches "^admin$"
    }
    never call delete_repository
}
```

* **`list_issue`** is granted, and one of its arguments is guarded.
* **`delete_repository`** is denied by name.
* **`search`** is named by no sentence at all.

The last two end up in the same place, and that is the whole idea. `%s` is a
format verb, not a typo: the URL is unknown until the server starts.

## Build it

```sh
mkdir -p mcpdemo && cd mcpdemo
go mod init example.com/mcpdemo
go get github.com/coilyco/umbra
```

Put this in `main.go`. It is the whole example.

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/coilyco/umbra/http/mcpverb"
	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"
)

const policy = `wrap example ops demo {
    mcp http { url "%s" }
    can call list_issue {
        deny owner matches "^admin$"
    }
    never call delete_repository
}`

func main() {
	url := serve()
	gf, err := mcpverb.Parse([]byte(fmt.Sprintf(policy, url)))
	if err != nil {
		fail(err)
	}
	// Stands in for the committed lock `umbra lock` would write. A generated
	// binary reads its lock and never reaches the network to mount.
	tools, err := listTools(url)
	if err != nil {
		fail(err)
	}
	root := &cli.Command{Name: "example", Usage: "the mcp dialect, over a server this process started"}
	if err := mcpverb.Mount(root, mcpverb.Config{Guardfile: gf, Tools: tools}); err != nil {
		fail(err)
	}
	if err := root.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "refused:", err)
		os.Exit(2)
	}
}

// serve starts the upstream MCP server this example guards.
func serve() string {
	srv := mcp.NewServer(&mcp.Implementation{Name: "demo", Version: "v0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "list_issue",
		Description: "list issues in a repository",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "repository owner"},
				"state": map[string]any{"type": "string", "enum": []any{"open", "closed"}},
			},
			"required": []any{"owner"},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf(`{"issues":2,"for":%s}`, req.Params.Arguments)},
		}}, nil
	})
	for _, name := range []string{"delete_repository", "search"} {
		srv.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	return httptest.NewServer(handler).URL
}

// listTools reads the upstream surface, the input `umbra lock` would freeze.
func listTools(url string) ([]mcpclient.Tool, error) {
	ctx := context.Background()
	sess, err := mcpclient.Connect(ctx, mcpclient.Server{Name: "demo", HTTP: &mcpclient.HTTPEndpoint{URL: url}})
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return sess.ListTools(ctx)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "example:", err)
	os.Exit(1)
}
```

**Build it rather than using `go run`**, which reports its own exit code and
prints `exit status N` as body text, so every code below would be wrong.

```sh
go build -o mcpverb . && ./mcpverb ops demo --help
```

```
NAME:
   example ops demo - guarded MCP tools from http://127.0.0.1:61095

COMMANDS:
   list-issue  call list_issue
```

One leaf out of three tools, and an ephemeral port, so yours will differ.
`list_issue` became `list-issue`: an MCP tool name is a flat identifier with no
resource and verb to resolve, so it lowers to one kebab-case leaf under the
wrap group.

## The refusals

A granted call, with its guarded argument satisfied:

```sh
./mcpverb ops demo list-issue --owner coilyco
```

```
for:
    owner: coilyco
issues: 2
```

`--owner` is a flag because the server's own JSON Schema says `owner` is a
string. The policy never described that shape and could not have: it names the
tool and one rule about one argument, and the schema supplies the rest.

The same verb with that argument matching:

```sh
./mcpverb ops demo list-issue --owner admin
```

```
refused: argument owner="admin" is outside the allowed scope (deny owner matches [^admin$])
```

Exit 2, and it quotes the rule that produced it rather than saying `denied`.
Every guard here reads one direction: **matching means the call does not pass.**
`deny` refuses on a match, and `allow` is an allowlist, so an argument matching
nothing in one is refused too.

Then the two that matter:

```sh
./mcpverb ops demo delete-repository
./mcpverb ops demo search
```

```
No help topic for 'delete-repository'
No help topic for 'search'
```

**One was denied by name, the other was never mentioned, and the binary answers
identically.** `never call delete_repository` does not mount a leaf that
refuses, it declines to mount a leaf at all, which is what saying nothing does
to `search`. A denied tool that still exists costs an agent context on every
listing and invites the call anyway, so the deny is spent on absence instead.
That is the deny-is-absence rule
[descriptors](../docs/specverb-descriptors.md) states for the spec dialect.
`./mcpverb ops demo delete-issue` answers the same way and was never advertised
at all, so a reader cannot tell the three apart. That is the property working
rather than a gap in it.

## The exit code here is not umbra's

Those exit **3**, which in the taxonomy means UpstreamFailed. No upstream was
reached and nothing failed, so the code is wrong for what happened, and it is
urfave/cli's rather than umbra's. A root with no `CommandNotFound` handler falls
through to the help path, which returns `Exit(errMsg, 3)`, and this example
mounts onto a bare `cli.Command` that never installs one.

A binary `umbra build` generates does install one, which is the exit 5
[primitives](primitives.md) contrasts. The absence is real either way and only
the answer's shape differs, so if you want the taxonomy's code from your own
mount, that handler is yours to install.

## The lock you did not write

`listTools` above reaches the live server, and a real consumer never does.
`umbra lock` connects once, runs `tools/list`, prunes to the granted tools and
writes `<binary>.tools.lock.json.gz`. Mounting reads that file, so a build is
offline and a tool added upstream since the lock is **not** mounted. This
example asks at startup only because it has no committed lock, the one thing
here that is a demonstration rather than the product shape.

`umbra skew` diffs live upstream against that lock and exits 3 on drift. Nothing
else locks MCP tool schemas, so nothing else can tell you one changed.

## Where to go next

* **[mcpverb-cli](mcpverb-cli.md)** - the same dialect the product way. KDL, a
  committed lock, no Go, against a server this repository does not control.
* **[mcpapps](mcpapps.md)** - when a tool ships an interface that calls back.
* **[the dialect reference](../docs/mcpverb.md)** - every grant and guard, the
  credential rules, and what a stdio upstream costs per call.
