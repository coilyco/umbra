package mcpverb_test

import (
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/mcpverb"
	"github.com/coilyco/umbra/pkg/mcpclient"
)

func TestParse_FailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "no wrap",
			src:     `mcp stdio { command "npx" }`,
			wantErr: "missing top-level `wrap`",
		},
		{
			name:    "no transport block",
			src:     `wrap a b { can call x }`,
			wantErr: "no `mcp` block",
		},
		{
			name:    "unknown transport",
			src:     `wrap a b { mcp carrier-pigeon { command "npx" } }`,
			wantErr: "unknown mcp transport",
		},
		{
			name:    "two transport blocks",
			src:     `wrap a b { mcp stdio { command "npx" }; mcp http { url "https://h/mcp" } }`,
			wantErr: "duplicate `mcp` block",
		},
		{
			name:    "stdio with no command",
			src:     `wrap a b { mcp stdio { argv "-y" } }`,
			wantErr: "needs a `command`",
		},
		{
			name:    "http with no url",
			src:     `wrap a b { mcp http { auth none } }`,
			wantErr: "needs a `url`",
		},
		{
			// A field of the other transport is rejected rather than ignored, so a
			// guardfile cannot look like it configured something it did not.
			name:    "stdio field on an http transport",
			src:     `wrap a b { mcp http { url "https://h/mcp"; command "npx" } }`,
			wantErr: "belongs to the stdio transport",
		},
		{
			name:    "unknown wrap node",
			src:     `wrap a b { mcp stdio { command "npx" }; base-url "https://h" }`,
			wantErr: "unknown node",
		},
		{
			name:    "unknown grant node",
			src:     `wrap a b { mcp stdio { command "npx" }; can call x { path "/y" } }`,
			wantErr: "unknown node",
		},
		{
			name:    "grant without call",
			src:     `wrap a b { mcp stdio { command "npx" }; can read x }`,
			wantErr: "needs `call <tool>`",
		},
		{
			name:    "a tool both granted and denied",
			src:     `wrap a b { mcp stdio { command "npx" }; can call x; never call x }`,
			wantErr: "both granted and denied",
		},
		{
			name:    "duplicate grant",
			src:     `wrap a b { mcp stdio { command "npx" }; can call x; can call x }`,
			wantErr: "duplicate grant",
		},
		{
			// A deny mounts nothing, so guards on it would never run. Silently
			// accepting them would read as policy that binds.
			name:    "guards on a deny",
			src:     `wrap a b { mcp stdio { command "npx" }; never call x { deny "o" matches "^a" } }`,
			wantErr: "carries no guards",
		},
		{
			name:    "rule without matches",
			src:     `wrap a b { mcp stdio { command "npx" }; can call x { deny "o" "^a" } }`,
			wantErr: "needs `<field> matches",
		},
		{
			name:    "rule with an uncompilable regex",
			src:     `wrap a b { mcp stdio { command "npx" }; can call x { deny "o" matches "^(" } }`,
			wantErr: "does not compile",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := mcpverb.Parse([]byte(c.src))
			if err == nil {
				t.Fatalf("Parse = nil, want an error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("Parse = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestParse_ReadsAStdioUpstream(t *testing.T) {
	gf, err := mcpverb.Parse([]byte(`description "the fixture upstream"
wrap aosguard ops fixture {
    mcp stdio {
        command "npx"
        argv "-y" "@example/mcp"
        env "TOKEN" { value env "FIXTURE_TOKEN" }
    }
    can call list_issue { describe "list them"; destructive }
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if gf.Description != "the fixture upstream" {
		t.Errorf("Description = %q", gf.Description)
	}
	if gf.Server.Command != "npx" || len(gf.Server.Argv) != 2 {
		t.Errorf("stdio = %q %v", gf.Server.Command, gf.Server.Argv)
	}
	if len(gf.Server.Env) != 1 || gf.Server.Env[0].Name != "TOKEN" {
		t.Fatalf("env = %+v", gf.Server.Env)
	}
	// The credential stays symbolic in the parse: it resolves per call, so a
	// rotated secret needs no rebuild.
	if got := gf.Server.Env[0].Value.String(); got != "env FIXTURE_TOKEN" {
		t.Errorf("env value = %q, want the unresolved chain", got)
	}
	if provs := gf.Providers(); len(provs) != 1 || provs[0] != "env" {
		t.Errorf("Providers = %v, want [env]", provs)
	}
	if len(gf.Grants) != 1 || !gf.Grants[0].Destructive {
		t.Errorf("grants = %+v", gf.Grants)
	}
	if got := gf.Grants[0].LeafName(); got != "list-issue" {
		t.Errorf("LeafName = %q, want list-issue", got)
	}
}

func TestDescriptors_SelectorMustBeARealArgument(t *testing.T) {
	gf, err := mcpverb.Parse([]byte(`wrap a b {
    mcp stdio { command "npx" }
    can call search { deny "owner_id" matches "^admin$" }
}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tools := []mcpclient.Tool{{
		Name: "search",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": map[string]any{"type": "string"}},
		},
	}}
	// A selector naming no real argument would otherwise compile into a rule that
	// matches nothing, which reads exactly like a guard that passed.
	_, _, err = mcpverb.Descriptors(mcpverb.Config{Guardfile: gf, Tools: tools})
	if err == nil {
		t.Fatal("Descriptors = nil, want an error naming the bad selector")
	}
	if !strings.Contains(err.Error(), "owner_id") || !strings.Contains(err.Error(), "have: owner") {
		t.Errorf("error = %v, want it to name the bad selector and the real arguments", err)
	}
}

func TestFieldsFor_LowersTheSchema(t *testing.T) {
	tool := mcpclient.Tool{
		Name: "x",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"zeta":   map[string]any{"type": "boolean"},
				"alpha":  map[string]any{"type": "string", "description": "first"},
				"count":  map[string]any{"type": "integer", "minimum": float64(1), "maximum": float64(9)},
				"tags":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"maybe":  map[string]any{"type": []any{"string", "null"}},
				"nested": map[string]any{"type": "object", "properties": map[string]any{"inner": map[string]any{"type": "string"}}},
			},
			"required": []any{"alpha"},
		},
	}
	fields, err := mcpverb.FieldsFor(tool)
	if err != nil {
		t.Fatalf("FieldsFor: %v", err)
	}
	// Sorted by name, because these reach generated help and a committed lock,
	// where map order would read as a change.
	var names []string
	byName := map[string]int{}
	for i, f := range fields {
		names = append(names, f.Name)
		byName[f.Name] = i
	}
	want := []string{"alpha", "count", "maybe", "nested", "tags", "zeta"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if f := fields[byName["alpha"]]; !f.Required || f.Desc != "first" {
		t.Errorf("alpha = %+v, want required with its description", f)
	}
	if f := fields[byName["count"]]; f.Type != "integer" || f.Minimum == nil || *f.Maximum != 9 {
		t.Errorf("count = %+v, want a bounded integer", f)
	}
	if f := fields[byName["tags"]]; f.Type != "array" || f.Items != "string" {
		t.Errorf("tags = %+v, want a string array", f)
	}
	// A union type takes its first non-null member: a flag has one type, and
	// refusing would drop a tool whose author wrote ["string","null"].
	if f := fields[byName["maybe"]]; f.Type != "string" {
		t.Errorf("maybe = %+v, want string from the union", f)
	}
	if f := fields[byName["nested"]]; f.Type != "object" || len(f.Fields) != 1 {
		t.Errorf("nested = %+v, want one nested field", f)
	}
}

func TestFieldsFor_NoPropertiesIsALeafWithNoInputs(t *testing.T) {
	fields, err := mcpverb.FieldsFor(mcpclient.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}})
	if err != nil {
		t.Fatalf("FieldsFor: %v", err)
	}
	if len(fields) != 0 {
		t.Errorf("fields = %v, want none", fields)
	}
}

func TestFieldsFor_RefusesRunawayNesting(t *testing.T) {
	// A self-referential schema is legal JSON Schema. Depth is refused rather
	// than survived, so the failure is a message instead of a stack overflow.
	schema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	for i := 0; i < 12; i++ {
		schema = map[string]any{"type": "object", "properties": map[string]any{"a": schema}}
	}
	if _, err := mcpverb.FieldsFor(mcpclient.Tool{Name: "deep", InputSchema: schema}); err == nil {
		t.Fatal("FieldsFor on a deeply nested schema = nil, want a refusal")
	}
}
