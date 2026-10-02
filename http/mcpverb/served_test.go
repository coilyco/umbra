package mcpverb_test

import (
	"encoding/json"
	"testing"

	"github.com/coilyco/umbra/http/mcpverb"
	"github.com/coilyco/umbra/pkg/mcpclient"
)

// lockedTool is an upstream tool as `umbra lock` froze it, carrying the
// shapes a served surface has to preserve.
func lockedTool() mcpclient.Tool {
	return mcpclient.Tool{
		Name:        "list_issue",
		Description: "list issues in a repository",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "repository owner"},
				"state": map[string]any{"type": "string", "enum": []any{"open", "closed"}},
				"limit": map[string]any{"type": "integer", "minimum": float64(1), "maximum": float64(100)},
				"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []any{"owner"},
		},
		Meta: map[string]any{"ui": map[string]any{"resourceUri": "ui://widget/issues.html"}},
	}
}

func servedFixture(t *testing.T) mcpverb.ServedTool {
	t.Helper()
	gf, err := mcpverb.Parse([]byte(`wrap a b {
    mcp stdio { command "npx" }
    can call list_issue
    never call delete_repository
}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tools, _, err := mcpverb.ServedTools(mcpverb.Config{
		Guardfile: gf,
		Tools:     []mcpclient.Tool{lockedTool(), {Name: "delete_repository"}, {Name: "search"}},
	})
	if err != nil {
		t.Fatalf("ServedTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("served %d tools, want 1 (denied and unnamed are both absent)", len(tools))
	}
	return tools[0]
}

func TestServedTools_AdvertisesOnlyWhatIsGranted(t *testing.T) {
	got := servedFixture(t)
	// The protocol's own name, not the kebab-cased CLI leaf.
	if got.Name != "list_issue" {
		t.Errorf("Name = %q, want the upstream tool name", got.Name)
	}
	if got.Description != "list issues in a repository" {
		t.Errorf("Description = %q", got.Description)
	}
}

func TestServedTools_PreservesMetaVerbatim(t *testing.T) {
	got := servedFixture(t)
	ui, ok := got.Meta["ui"].(map[string]any)
	if !ok || ui["resourceUri"] != "ui://widget/issues.html" {
		t.Fatalf("Meta = %#v, want the widget address forwarded unchanged", got.Meta)
	}
}

// TestServedTools_SchemaRoundTrip is load-bearing: anything dropped turning the
// lock into flags and back is a constraint the caller silently loses.
func TestServedTools_SchemaRoundTrip(t *testing.T) {
	got := servedFixture(t)
	var doc map[string]any
	if err := json.Unmarshal(got.InputSchema, &doc); err != nil {
		t.Fatalf("served schema is not JSON: %v", err)
	}
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		t.Fatalf("served schema has no properties: %s", got.InputSchema)
	}
	for _, name := range []string{"owner", "state", "limit", "tags"} {
		if _, ok := props[name]; !ok {
			t.Errorf("property %q was lost", name)
		}
	}

	owner, _ := props["owner"].(map[string]any)
	if owner["type"] != "string" || owner["description"] != "repository owner" {
		t.Errorf("owner = %#v, want its type and description", owner)
	}

	// An enum is the constraint a caller cannot guess, so it has to survive as
	// `enum` rather than as prose in the description.
	state, _ := props["state"].(map[string]any)
	enum, ok := state["enum"].([]any)
	if !ok || len(enum) != 2 || enum[0] != "open" || enum[1] != "closed" {
		t.Errorf("state.enum = %#v, want [open closed]", state["enum"])
	}

	limit, _ := props["limit"].(map[string]any)
	if limit["type"] != "integer" || limit["minimum"] != float64(1) || limit["maximum"] != float64(100) {
		t.Errorf("limit = %#v, want a bounded integer", limit)
	}

	tags, _ := props["tags"].(map[string]any)
	items, _ := tags["items"].(map[string]any)
	if tags["type"] != "array" || items["type"] != "string" {
		t.Errorf("tags = %#v, want a string array", tags)
	}

	required, _ := doc["required"].([]any)
	if len(required) != 1 || required[0] != "owner" {
		t.Errorf("required = %#v, want [owner]", doc["required"])
	}
}

func TestServedTools_ExecutesThroughTheSameDescriptor(t *testing.T) {
	got := servedFixture(t)
	// A served call goes through opcore.Operation exactly as a CLI leaf does,
	// so the guards are not a second implementation that can drift.
	if got.Descriptor.MCP == nil || got.Descriptor.MCP.Tool != "list_issue" {
		t.Fatalf("descriptor = %+v, want the mcp leaf for list_issue", got.Descriptor)
	}
	if got.Descriptor.VerbName != "a.b.list-issue" {
		t.Errorf("VerbName = %q, want the audit name the CLI would use", got.Descriptor.VerbName)
	}
}
