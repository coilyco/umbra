package mcpverb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/mcpverb"
	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"
)

// upstream stands up a real MCP server over HTTP, so a test exercises the whole
// path (guardfile, descriptor, flag binding, tools/call, response), not a mock.
func upstream(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "v0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "list_issue",
		Description: "list issues in a repository",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "repository owner"},
				"limit": map[string]any{"type": "integer"},
				"state": map[string]any{"type": "string", "enum": []any{"open", "closed"}},
			},
			"required": []any{"owner"},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		_ = json.Unmarshal(req.Params.Arguments, &args)
		owner, _ := args["owner"].(string)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(`{"owner":%q}`, owner)}},
		}, nil
	})
	srv.AddTool(&mcp.Tool{
		Name:        "delete_repository",
		InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "gone"}}}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// lockedTools reads the upstream's real surface, standing in for the committed
// lock so the test's schemas are the server's own rather than hand-copied.
func lockedTools(t *testing.T, url string) []mcpclient.Tool {
	t.Helper()
	sess, err := mcpclient.Connect(context.Background(), mcpclient.Server{
		Name: "fixture",
		HTTP: &mcpclient.HTTPEndpoint{URL: url},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = sess.Close() }()
	tools, err := sess.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	return tools
}

func guardfile(t *testing.T, src string) *mcpverb.Guardfile {
	t.Helper()
	gf, err := mcpverb.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return gf
}

func TestBuild_MountsGrantedAndOmitsDenied(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue
    never call delete_repository
}`)
	group, err := mcpverb.Build(mcpverb.Config{Guardfile: gf, Tools: lockedTools(t, url)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	names := leafNames(group)
	if len(names) != 1 || names[0] != "list-issue" {
		t.Fatalf("leaves = %v, want [list-issue]", names)
	}
	// A denied tool is absent, not mounted as a leaf that refuses. A refusing
	// leaf still costs context and still invites the call.
	for _, n := range names {
		if n == "delete-repository" {
			t.Error("a denied tool was mounted")
		}
	}
}

func TestBuild_UngrantedToolIsAbsent(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue
}`)
	group, err := mcpverb.Build(mcpverb.Config{Guardfile: gf, Tools: lockedTools(t, url)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// delete_repository is on the upstream and simply not granted. Deny by
	// absence means "not named" and "denied" reach the same surface.
	if names := leafNames(group); len(names) != 1 {
		t.Errorf("leaves = %v, want only the granted one", names)
	}
}

func TestBuild_GrantingAnUnlockedToolFailsClosed(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call not_a_tool
}`)
	_, err := mcpverb.Build(mcpverb.Config{Guardfile: gf, Tools: lockedTools(t, url)})
	if err == nil {
		t.Fatal("Build = nil, want an error naming the unlocked tool")
	}
	if !strings.Contains(err.Error(), "not_a_tool") {
		t.Errorf("error = %v, want it to name the tool", err)
	}
}

func TestBuild_FlagsComeFromTheLockedSchema(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue
}`)
	group, err := mcpverb.Build(mcpverb.Config{Guardfile: gf, Tools: lockedTools(t, url)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	leaf := group.Commands[0]
	want := map[string]bool{"owner": false, "limit": false, "state": false}
	for _, f := range leaf.Flags {
		for _, n := range f.Names() {
			if _, ok := want[n]; ok {
				want[n] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("flag --%s is missing; the schema declared it", name)
		}
	}
	// The enum reaches help, since it is the one constraint a caller cannot
	// infer from the type.
	if u := flagUsage(leaf, "state"); !strings.Contains(u, "open") {
		t.Errorf("--state usage = %q, want the enum values", u)
	}
}

func TestRun_CallsTheToolEndToEnd(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue
}`)
	out := runLeaf(t, gf, lockedTools(t, url), []string{"list-issue", "--owner", "coilyco"})
	if !strings.Contains(out, "coilyco") {
		t.Errorf("output = %q, want the tool's answer", out)
	}
}

func TestRun_MissingRequiredInputIsRefused(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue
}`)
	err := runLeafErr(t, gf, lockedTools(t, url), []string{"list-issue"})
	if err == nil {
		t.Fatal("call without a required input = nil, want an error")
	}
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("error = %v, want it to name the missing input", err)
	}
}

func TestRun_DenyGuardRefusesBeforeTheCall(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue {
        deny "owner" matches "^admin$"
    }
}`)
	err := runLeafErr(t, gf, lockedTools(t, url), []string{"list-issue", "--owner", "admin"})
	if err == nil {
		t.Fatal("a denied argument = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("error = %v, want it to name the guarded argument", err)
	}
}

func TestRun_AllowGuardIsAnAllowlist(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue {
        allow "owner" matches "^coilyco"
    }
}`)
	if err := runLeafErr(t, gf, lockedTools(t, url), []string{"list-issue", "--owner", "coilyco-flight-deck"}); err != nil {
		t.Fatalf("a permitted value was refused: %v", err)
	}
	if err := runLeafErr(t, gf, lockedTools(t, url), []string{"list-issue", "--owner", "someone-else"}); err == nil {
		t.Error("a value matching no allow pattern = nil, want a refusal")
	}
}

func TestRun_RestrictScopesTheArguments(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    restrict owner matches "coilyco-*"
    can call list_issue
}`)
	if err := runLeafErr(t, gf, lockedTools(t, url), []string{"list-issue", "--owner", "coilyco-flight-deck"}); err != nil {
		t.Fatalf("an in-scope owner was refused: %v", err)
	}
	if err := runLeafErr(t, gf, lockedTools(t, url), []string{"list-issue", "--owner", "someone-else"}); err == nil {
		t.Error("an out-of-scope owner = nil, want a refusal")
	}
}

func TestRun_FailWhenRejectsASuccessfulCall(t *testing.T) {
	url := upstream(t)
	// The call succeeds and the postcondition rejects the value it returned.
	// This is the guard floor applying above the transport, unchanged from HTTP.
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue {
        fail-when "owner == 'coilyco'"
    }
}`)
	err := runLeafErr(t, gf, lockedTools(t, url), []string{"list-issue", "--owner", "coilyco"})
	if err == nil {
		t.Fatal("a response matching fail-when = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "fail-when") {
		t.Errorf("error = %v, want it to name the postcondition", err)
	}
}

func TestRun_DryRunFiresNothing(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call list_issue
}`)
	out := runLeaf(t, gf, lockedTools(t, url), []string{"list-issue", "--owner", "coilyco", "--dry-run", "--output", "json"})
	if !strings.Contains(out, "list_issue") {
		t.Errorf("dry run = %q, want the upstream tool name", out)
	}
	if !strings.Contains(out, "coilyco") {
		t.Errorf("dry run = %q, want the resolved arguments", out)
	}
}

func TestWildcardExpandsOverTheLockAndHonoursDeny(t *testing.T) {
	url := upstream(t)
	gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    can call *
    never call delete_repository
}`)
	group, err := mcpverb.Build(mcpverb.Config{Guardfile: gf, Tools: lockedTools(t, url)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	names := leafNames(group)
	if len(names) != 1 || names[0] != "list-issue" {
		t.Errorf("leaves = %v, want the wildcard minus the denied tool", names)
	}
}

func TestExplicitGrantBeatsTheWildcardWhicheverIsAuthoredFirst(t *testing.T) {
	url := upstream(t)
	// Order-dependence here would let the narrower policy be lost silently, so
	// both authoring orders must produce the same guarded leaf.
	for _, src := range []string{
		`can call * ` + "\n" + `    can call list_issue { deny "owner" matches "^admin$" }`,
		`can call list_issue { deny "owner" matches "^admin$" }` + "\n" + `    can call *`,
	} {
		gf := guardfile(t, `wrap aosguard ops fixture {
    mcp http { url "`+url+`" }
    `+src+`
}`)
		tools := lockedTools(t, url)
		if err := runLeafErr(t, gf, tools, []string{"list-issue", "--owner", "admin"}); err == nil {
			t.Errorf("src %q: the explicit grant's deny did not bind", src)
		}
	}
}

// runLeaf runs one command line against a built tree and returns stdout.
func runLeaf(t *testing.T, gf *mcpverb.Guardfile, tools []mcpclient.Tool, argv []string) string {
	t.Helper()
	out, err := captureStdout(t, func() error { return runLeafErr(t, gf, tools, argv) })
	if err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return out
}

// runLeafErr runs one leaf invocation and returns only its error. argv is the
// leaf and its flags, and the wrap path in front of it comes from the guardfile.
func runLeafErr(t *testing.T, gf *mcpverb.Guardfile, tools []mcpclient.Tool, argv []string) error {
	t.Helper()
	root := &cli.Command{Name: gf.Group[0]}
	if err := mcpverb.Mount(root, mcpverb.Config{Guardfile: gf, Tools: tools}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return root.Run(context.Background(), append(append([]string{}, gf.Group...), argv...))
}

func leafNames(group *cli.Command) []string {
	names := make([]string, 0, len(group.Commands))
	for _, c := range group.Commands {
		names = append(names, c.Name)
	}
	return names
}

func flagUsage(cmd *cli.Command, name string) string {
	for _, f := range cmd.Flags {
		sf, ok := f.(*cli.StringFlag)
		if !ok || sf.Name != name {
			continue
		}
		return sf.Usage
	}
	return ""
}

// captureStdout runs fn with stdout redirected to a pipe and returns what it
// wrote. The engine prints the rendered result rather than returning it.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	runErr := fn()
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, runErr
}
