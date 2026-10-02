package specverb

import (
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// umbra#6824: operationId is OPTIONAL in the OpenAPI Specification, and a
// conformant document may omit it on every operation. Teable's does, on all 756.
const noOperationIDSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "unnamed", "version": "1"},
  "paths": {
    "/table/{tableId}/field": {
      "post": {
        "summary": "Create field",
        "tags": ["field"],
        "parameters": [{"name": "tableId", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {"content": {"application/json": {"schema": {"type": "object",
          "properties": {"name": {"type": "string"}, "type": {"type": "string"}}}}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object"}}}}}
      },
      "get": {
        "summary": "List fields",
        "parameters": [{"name": "tableId", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object"}}}}}
      }
    }
  }
}`

func noIDSpec(t *testing.T) *spec {
	t.Helper()
	s, err := parseOpenAPI3([]byte(noOperationIDSpec))
	if err != nil {
		t.Fatalf("parseOpenAPI3: %v", err)
	}
	for path, methods := range s.ops {
		for m, o := range methods {
			if o.operationID != "" {
				t.Fatalf("fixture is not operationId-free: %s %s has %q", m, path, o.operationID)
			}
		}
	}
	return s
}

func TestOpAddrByRouteResolvesAnOperationIDFreeSpec(t *testing.T) {
	s := noIDSpec(t)
	g := guardfile.Grant{Verb: "create", Resource: "field", OpMethod: "POST", OpPath: "/table/{tableId}/field"}
	addr, err := resolveOp(s, g)
	if err != nil {
		t.Fatalf("resolveOp: %v", err)
	}
	method, path, op, err := s.findOp(addr)
	if err != nil {
		t.Fatalf("findOp: %v", err)
	}
	if method != "POST" || path != "/table/{tableId}/field" {
		t.Fatalf("resolved %s %s", method, path)
	}
	// The operation resolves fully, not just by name: its body schema is reachable.
	if _, ok := s.bodySchema(op); !ok {
		t.Error("route-addressed op resolved without its request body")
	}
}

// Method disambiguates within one path, so the address is total rather than a
// best guess: the same path with a different method is a different operation.
func TestOpAddrByRouteDistinguishesMethodsOnOnePath(t *testing.T) {
	s := noIDSpec(t)
	post, _, _, err := s.findOp(opAddr{method: "POST", path: "/table/{tableId}/field"})
	if err != nil {
		t.Fatal(err)
	}
	get, _, _, err := s.findOp(opAddr{method: "GET", path: "/table/{tableId}/field"})
	if err != nil {
		t.Fatal(err)
	}
	if post != "POST" || get != "GET" {
		t.Errorf("post=%q get=%q, want the two methods kept apart", post, get)
	}
}

func TestOpAddrByRouteFailsClosed(t *testing.T) {
	s := noIDSpec(t)
	if _, _, _, err := s.findOp(opAddr{method: "POST", path: "/table/{tableId}/nope"}); err == nil {
		t.Error("an unknown path must fail closed")
	}
	_, _, _, err := s.findOp(opAddr{method: "DELETE", path: "/table/{tableId}/field"})
	if err == nil {
		t.Fatal("a method the path does not declare must fail closed")
	}
	if !strings.Contains(err.Error(), "DELETE") {
		t.Errorf("error should name the method it tried: %v", err)
	}
}

// An operationId grant keeps resolving exactly as before: this form is additive.
func TestOpAddrOperationIDPathIsUnchanged(t *testing.T) {
	s, err := parseOpenAPI3(readSpec(t, "trello.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := resolveOp(s, guardfile.Grant{Verb: "create", Resource: "card", Op: "post-cards"})
	if err != nil {
		t.Fatal(err)
	}
	if addr.id != "post-cards" || addr.method != "" || addr.path != "" {
		t.Fatalf("addr = %+v, want an id-only address", addr)
	}
	if _, path, _, err := s.findOp(addr); err != nil || path != "/cards" {
		t.Errorf("path = %q, err = %v", path, err)
	}
}

// wrapAround builds a minimal parseable guardfile around one grant body, so a
// refusal in these cases is the `op` node's and not a missing sibling's.
func wrapAround(body string) []byte {
	return []byte("wrap \"demo\" {\n    spec \"x.json\"\n    auth none\n    can create field {\n        " + body + "\n    }\n}\n")
}

func TestOpNodeRejectsAMixedOrIncompleteForm(t *testing.T) {
	// The control for the control: the wrapper alone must parse, or every case
	// below would pass on the wrapper's own error rather than the op node's.
	if _, err := guardfile.Parse(wrapAround(`op "post-cards"`)); err != nil {
		t.Fatalf("baseline wrapper must parse, else these cases prove nothing: %v", err)
	}
	for _, body := range []string{
		`op "post-cards" method="POST"`,
		`op method="POST"`,
		`op path="/cards"`,
		`op method="POST" path="/cards" extra="x"`,
	} {
		_, err := guardfile.Parse(wrapAround(body))
		if err == nil {
			t.Errorf("%q must fail closed at parse", body)
			continue
		}
		if !strings.Contains(err.Error(), "`op`") {
			t.Errorf("%q refused for the wrong reason: %v", body, err)
		}
	}
}

func TestOpNodeParsesTheMethodPathForm(t *testing.T) {
	gf, err := guardfile.Parse(wrapAround(`op method="post" path="/table/{tableId}/field"`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := gf.Grants[0]
	if g.OpMethod != "POST" {
		t.Errorf("OpMethod = %q, want it upper-cased so the guardfile may write it either way", g.OpMethod)
	}
	if g.OpPath != "/table/{tableId}/field" {
		t.Errorf("OpPath = %q", g.OpPath)
	}
	if g.Op != "" {
		t.Errorf("Op = %q, want empty on the route form", g.Op)
	}
}
