package openapigen

import (
	"encoding/json"
	"strings"
	"testing"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/opcore"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("emitted document is not JSON: %v", err)
	}
	return doc
}

func op(t *testing.T, doc map[string]any, path, method string) map[string]any {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	if item == nil {
		t.Fatalf("no path %q in %v", path, paths)
	}
	found, _ := item[method].(map[string]any)
	if found == nil {
		t.Fatalf("no %s on %q", method, path)
	}
	return found
}

func TestEmitRendersAGrantAsAnOperation(t *testing.T) {
	raw, skipped, err := Emit([]opcore.Descriptor{{
		VerbName: "ward.ops.forgejo.repo.get", Method: "GET",
		Path: "/repos/{owner}/{repo}", PathParams: []string{"owner", "repo"},
		Grant: "can get repo", Describe: "Read one repository.",
	}}, Config{Title: "t", Version: "1"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	doc := decode(t, raw)
	if doc["openapi"] != "3.1.0" {
		t.Errorf("openapi = %v, want 3.1.0", doc["openapi"])
	}
	got := op(t, doc, "/repos/{owner}/{repo}", "get")
	if got["operationId"] != "ward.ops.forgejo.repo.get" {
		t.Errorf("operationId = %v, want the dotted VerbName", got["operationId"])
	}
	if got["x-umbra-grant"] != "can get repo" {
		t.Errorf("x-umbra-grant = %v", got["x-umbra-grant"])
	}
	params, _ := got["parameters"].([]any)
	if len(params) != 2 {
		t.Fatalf("parameters = %d, want the two path params", len(params))
	}
}

// TestEmitSkipsALeafThatReachesNoURL is the lie this must not tell: a graphql,
// sql, mcp or proxy leaf has no HTTP path, so emitting one invents a route.
func TestEmitSkipsALeafThatReachesNoURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		desc opcore.Descriptor
	}{
		{"sql", opcore.Descriptor{VerbName: "v.sql", Path: "/x", Method: "POST", SQL: &opcore.SQL{}}},
		{"graphql", opcore.Descriptor{VerbName: "v.gql", Path: "/x", Method: "POST", GraphQL: &opcore.GraphQL{}}},
		{"no path", opcore.Descriptor{VerbName: "v.none", Method: "GET"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, skipped, err := Emit([]opcore.Descriptor{tc.desc}, Config{})
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if len(skipped) != 1 || skipped[0] != tc.desc.VerbName {
				t.Fatalf("skipped = %v, want [%s]", skipped, tc.desc.VerbName)
			}
			paths, _ := decode(t, raw)["paths"].(map[string]any)
			if len(paths) != 0 {
				t.Fatalf("paths = %v, want none: a leaf reaching no URL was emitted as a route", paths)
			}
		})
	}
}

// TestFixedBodyIsConstNotAFreeField: rendering a pin as an ordinary property
// would claim freedom the guardfile does not grant.
func TestFixedBodyIsConstNotAFreeField(t *testing.T) {
	raw, _, err := Emit([]opcore.Descriptor{{
		VerbName: "v.create", Method: "POST", Path: "/p",
		FixedBody: map[string]any{"type": "SecureString"},
		BodyFlags: []opcore.Field{{Name: "name", Type: "string", Required: true}},
	}}, Config{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body, _ := op(t, decode(t, raw), "/p", "post")["requestBody"].(map[string]any)
	content, _ := body["content"].(map[string]any)
	js, _ := content["application/json"].(map[string]any)
	schema, _ := js["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)

	pinned, _ := props["type"].(map[string]any)
	if pinned["const"] != "SecureString" {
		t.Errorf("pinned key rendered as %v, want a const", pinned)
	}
	free, _ := props["name"].(map[string]any)
	if _, isConst := free["const"]; isConst {
		t.Error("a free field rendered as const, which would deny the caller a value it may set")
	}
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "name" {
		t.Errorf("required = %v, want only the caller-supplied field", req)
	}
}

func TestUndeclaredReturnsKeepsTheDefaultResponse(t *testing.T) {
	raw, _, err := Emit([]opcore.Descriptor{{VerbName: "v.get", Method: "GET", Path: "/p"}}, Config{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := op(t, decode(t, raw), "/p", "get")["responses"].(map[string]any)
	if _, ok := got["default"]; !ok {
		t.Errorf("responses = %v, want the undifferentiated default", got)
	}
	if _, ok := got["200"]; ok {
		t.Error("a leaf with no `returns` declaration emitted a 200 schema it does not enforce")
	}
}

func TestDeclaredReturnsBecomesThe200Schema(t *testing.T) {
	raw, _, err := Emit([]opcore.Descriptor{{
		VerbName: "v.get", Method: "GET", Path: "/p",
		ReturnFields: []opcore.Field{
			{Name: "id", Type: "integer", Required: true},
			{Name: "name", Type: "string"},
		},
	}}, Config{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	responses := op(t, decode(t, raw), "/p", "get")["responses"].(map[string]any)
	if _, ok := responses["default"]; ok {
		t.Error("a leaf with a `returns` declaration still emitted the undifferentiated default")
	}
	got, ok := responses["200"].(map[string]any)
	if !ok {
		t.Fatalf("responses = %v, want a 200 entry", responses)
	}
	content, _ := got["content"].(map[string]any)
	js, _ := content["application/json"].(map[string]any)
	schema, _ := js["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["id"]; !ok {
		t.Errorf("schema properties = %v, want id", props)
	}
	if _, ok := props["name"]; !ok {
		t.Errorf("schema properties = %v, want name", props)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "id" {
		t.Errorf("required = %v, want only id", required)
	}
}

func TestEmitRefusesADuplicateOperationID(t *testing.T) {
	_, _, err := Emit([]opcore.Descriptor{
		{VerbName: "v.same", Method: "GET", Path: "/a"},
		{VerbName: "v.same", Method: "GET", Path: "/b"},
	}, Config{})
	if err == nil {
		t.Fatal("two grants shared an operationId and the document was emitted anyway")
	}
	if !strings.Contains(err.Error(), "operationId") {
		t.Errorf("error = %v, want it to name the collision", err)
	}
}

func TestQueryBoundsSurvive(t *testing.T) {
	upper := 100.0
	raw, _, err := Emit([]opcore.Descriptor{{
		VerbName: "v.list", Method: "GET", Path: "/l",
		QueryFlags: []opcore.Field{{Name: "limit", Type: "integer", Maximum: &upper}},
	}}, Config{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	params, _ := op(t, decode(t, raw), "/l", "get")["parameters"].([]any)
	first, _ := params[0].(map[string]any)
	schema, _ := first["schema"].(map[string]any)
	if schema["maximum"] != 100.0 {
		t.Errorf("maximum = %v, want the bound the guardfile enforces", schema["maximum"])
	}
}
