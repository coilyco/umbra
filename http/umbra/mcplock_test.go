package umbra

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coilyco/umbra/http/umbra/codegen"
	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// movingUpstream serves a real MCP server whose list_issue schema can be changed
// mid-test, which is what makes drift detection testable rather than asserted.
type movingUpstream struct {
	url     string
	widened atomic.Bool
}

func newMovingUpstream(t *testing.T) *movingUpstream {
	t.Helper()
	up := &movingUpstream{}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "v0"}, nil)
		props := map[string]any{"owner": map[string]any{"type": "string"}}
		if up.widened.Load() {
			props["state"] = map[string]any{"type": "string"}
		}
		srv.AddTool(&mcp.Tool{
			Name:        "list_issue",
			Description: "list issues",
			InputSchema: map[string]any{"type": "object", "properties": props, "required": []any{"owner"}},
			// _meta carries the MCP Apps widget address. It has to survive the
			// lock verbatim, and move the lock when it changes.
			Meta: map[string]any{"ui": map[string]any{"resourceUri": "ui://widget/issues.html"}},
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
		srv.AddTool(&mcp.Tool{
			Name:        "delete_repository",
			InputSchema: map[string]any{"type": "object"},
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "gone"}}}, nil
		})
		return srv
	}, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	up.url = ts.URL
	return up
}

// mcpProject writes a one-member .umbra project pointed at the upstream.
func mcpProject(t *testing.T, url string) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, ".umbra")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := `wrap ward-mcp ops fixture {
    mcp http { url "` + url + `" }
    can call list_issue
    never call delete_repository
}`
	if err := os.WriteFile(filepath.Join(root, "fixture.guardfile.kdl"), []byte(src), 0o600); err != nil {
		t.Fatalf("write guardfile: %v", err)
	}
	return root
}

func TestSniffTransport_ReadsTheMCPDialect(t *testing.T) {
	// `mcp` in the command path is a positional argument, not a child, so an
	// mcp-named spec member must not sniff as an mcp transport.
	spec := []byte(`wrap ward mcp forgejo { base-url "h/api"; can read repos }`)
	got, err := sniffTransport(spec)
	if err != nil {
		t.Fatalf("sniff: %v", err)
	}
	if got != codegen.TransportSpec {
		t.Errorf("transport = %q, want %q; the command path is not a transport", got, codegen.TransportSpec)
	}

	dialect := []byte(`wrap ward mcp forgejo { mcp stdio { command "npx" }; can call x }`)
	if got, err = sniffTransport(dialect); err != nil {
		t.Fatalf("sniff: %v", err)
	}
	if got != codegen.TransportMCP {
		t.Errorf("transport = %q, want %q", got, codegen.TransportMCP)
	}
}

func TestLockAndSkew_MCPMember(t *testing.T) {
	up := newMovingUpstream(t)
	root := mcpProject(t, up.url)
	opts := Options{ProjectRoot: root}

	g, err := loadGroup(opts)
	if err != nil {
		t.Fatalf("loadGroup: %v", err)
	}
	if len(g.Members) != 1 || !g.Members[0].isMCP() {
		t.Fatalf("members = %+v, want one mcp member", g.Members)
	}

	// lock is the one online step: connect, list, prune, freeze.
	specs, err := lockSpecs(g)
	if err != nil {
		t.Fatalf("lockSpecs: %v", err)
	}
	lockPath := filepath.Join(g.Dir, g.Members[0].Params.SpecLockName)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("tool lock was not written: %v", err)
	}
	locked, err := decodeTools(specs[g.Members[0].Path])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Pruned to the granted surface: the denied tool is upstream and absent
	// here, so skew never fires on a tool nobody mounts.
	if len(locked) != 1 || locked[0].Name != "list_issue" {
		t.Fatalf("locked = %+v, want only list_issue", locked)
	}
	ui, ok := locked[0].Meta["ui"].(map[string]any)
	if !ok || ui["resourceUri"] != "ui://widget/issues.html" {
		t.Errorf("_meta = %#v, want the widget address preserved verbatim", locked[0].Meta)
	}

	// A second lock against an unchanged upstream produces the same bytes, so a
	// re-lock is an empty diff rather than committed churn.
	again, err := lockSpecs(g)
	if err != nil {
		t.Fatalf("second lockSpecs: %v", err)
	}
	if string(again[g.Members[0].Path]) != string(specs[g.Members[0].Path]) {
		t.Error("locking twice produced different bytes; the lock is not deterministic")
	}

	if err := Skew(opts); err != nil {
		t.Fatalf("Skew against an unchanged upstream = %v, want nil", err)
	}

	// Move the upstream schema. This is the capability nothing else has: a
	// client can print today's schema, only a lock can say it changed.
	up.widened.Store(true)
	err = Skew(opts)
	if !errors.Is(err, ErrSkew) {
		t.Fatalf("Skew after a schema change = %v, want ErrSkew", err)
	}
}

func TestSkew_MCPMemberWithoutALockSaysSo(t *testing.T) {
	up := newMovingUpstream(t)
	root := mcpProject(t, up.url)
	err := Skew(Options{ProjectRoot: root})
	if !errors.Is(err, ErrNoLock) {
		t.Fatalf("Skew with no committed lock = %v, want ErrNoLock", err)
	}
}

func TestDiffTools(t *testing.T) {
	base := []toolFixture{{name: "a", schema: map[string]any{"type": "object"}}}
	cases := []struct {
		name string
		live []toolFixture
		want string
	}{
		{name: "unchanged", live: base, want: ""},
		{name: "removed", live: nil, want: "tool removed upstream: a"},
		{
			name: "added",
			live: append(append([]toolFixture{}, base...), toolFixture{name: "b", schema: map[string]any{"type": "object"}}),
			want: "tool added upstream: b",
		},
		{
			name: "schema moved",
			live: []toolFixture{{name: "a", schema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}}},
			want: "tool a changed: input schema",
		},
		{
			// An MCP Apps widget that silently repoints is exactly the drift
			// class the lock exists to catch.
			name: "meta moved",
			live: []toolFixture{{name: "a", schema: map[string]any{"type": "object"}, meta: map[string]any{"ui": "ui://new"}}},
			want: "tool a changed: _meta",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drift, err := diffTools(toolsOf(base), toolsOf(c.live))
			if err != nil {
				t.Fatalf("diffTools: %v", err)
			}
			got := strings.Join(drift, "; ")
			if got != c.want {
				t.Errorf("drift = %q, want %q", got, c.want)
			}
		})
	}
}

// toolFixture is a compact way to state a tool in a diff table.
type toolFixture struct {
	name   string
	schema map[string]any
	meta   map[string]any
}

func toolsOf(fixtures []toolFixture) []mcpclient.Tool {
	out := make([]mcpclient.Tool, 0, len(fixtures))
	for _, f := range fixtures {
		out = append(out, mcpclient.Tool{Name: f.name, InputSchema: f.schema, Meta: f.meta})
	}
	return out
}

// TestRender_MergesAllThreeDialects proves the generated binary is one binary:
// an HTTP API, a wrapped tool, and an MCP server mounted side by side.
func TestRender_MergesAllThreeDialects(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".umbra")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	members := map[string]string{
		"forgejo.guardfile.kdl": guardfileFixture,
		"aws.guardfile.kdl":     execFixture,
		"fixture.guardfile.kdl": `wrap ward-kdl ops fixture {
    mcp stdio { command "npx"; argv "-y" "@example/mcp" }
    can call list_issue
}`,
	}
	for name, src := range members {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	g, err := loadGroup(Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("loadGroup: %v", err)
	}
	if len(g.Members) != 3 {
		t.Fatalf("want 3 merged members, got %d", len(g.Members))
	}
	main, err := g.render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "main.go", main, parser.AllErrors); err != nil {
		t.Fatalf("merged main.go does not parse: %v\n%s", err, main)
	}
	src := string(main)
	for _, want := range []string{
		"specverb.Mount(app",
		"execverb.Mount(app",
		"mcpverb.Mount(app",
		"//go:embed fixture.tools.lock.json.gz",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("merged main.go is missing %q", want)
		}
	}
}
