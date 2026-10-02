package specverb

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// TestPruneKeepsOnlyGrantedSurface prunes to the repo trio and asserts only the
// granted paths/methods + reachable defs survive, smaller and still mounting.
func TestPruneKeepsOnlyGrantedSurface(t *testing.T) {
	gf, full := loadFixtures(t)

	pruned, err := Prune(full, gf)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(pruned) >= len(full) {
		t.Errorf("pruned spec (%d) not smaller than full (%d)", len(pruned), len(full))
	}

	var doc struct {
		Paths       map[string]map[string]json.RawMessage `json:"paths"`
		Definitions map[string]json.RawMessage            `json:"definitions"`
	}
	if err := json.Unmarshal(pruned, &doc); err != nil {
		t.Fatalf("pruned spec is not valid json: %v", err)
	}

	wantPaths := []string{"/repos/{owner}/{repo}", "/user/repos"}
	gotPaths := keysOf(doc.Paths)
	if !equalSorted(gotPaths, wantPaths) {
		t.Errorf("pruned paths = %v, want %v", gotPaths, wantPaths)
	}
	// The repo path carries both granted methods; nothing else.
	repoMethods := keysOf(doc.Paths["/repos/{owner}/{repo}"])
	if !equalSorted(repoMethods, []string{"delete", "get"}) {
		t.Errorf("repo path methods = %v, want [delete get]", repoMethods)
	}
	// Only the body schema reachable from create survives; issue/label defs go.
	gotDefs := keysOf(doc.Definitions)
	if !equalSorted(gotDefs, []string{"CreateRepoOption"}) {
		t.Errorf("pruned definitions = %v, want [CreateRepoOption]", gotDefs)
	}

	// The pruned lock must still parse and mount identically.
	if _, err := parseSwagger(pruned); err != nil {
		t.Fatalf("pruned spec failed to parse: %v", err)
	}
	if _, err := Build(Config{Guardfile: gf, Spec: pruned}); err != nil {
		t.Fatalf("Build on pruned spec: %v", err)
	}
}

// TestPruneIsIdempotent re-pruning a pruned spec yields the same bytes, so a
// committed (already-pruned) lock compares cleanly against a freshly pruned one.
func TestPruneIsIdempotent(t *testing.T) {
	gf, full := loadFixtures(t)
	once, err := Prune(full, gf)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	twice, err := Prune(once, gf)
	if err != nil {
		t.Fatalf("Prune (second): %v", err)
	}
	if string(once) != string(twice) {
		t.Error("Prune is not idempotent: re-pruning changed the bytes")
	}
}

// TestPruneClosesSharedResponses proves the pruner closes the shared `responses`
// section (no dangling ref into pruned defs).
func TestPruneClosesSharedResponses(t *testing.T) {
	spec := []byte(`{
	  "swagger": "2.0",
	  "info": {"title": "t", "version": "1"},
	  "basePath": "/api",
	  "paths": {
	    "/comments": {"get": {"operationId": "commentList",
	      "responses": {"200": {"$ref": "#/responses/CommentList"}}}},
	    "/tokens": {"get": {"operationId": "tokenList",
	      "responses": {"200": {"$ref": "#/responses/AccessTokenList"}}}}
	  },
	  "responses": {
	    "CommentList": {"description": "c", "schema": {"type": "array", "items": {"$ref": "#/definitions/Comment"}}},
	    "AccessTokenList": {"description": "a", "schema": {"type": "array", "items": {"$ref": "#/definitions/AccessToken"}}}
	  },
	  "definitions": {
	    "Comment": {"type": "object", "properties": {"body": {"type": "string"}, "user": {"$ref": "#/definitions/User"}}},
	    "User": {"type": "object", "properties": {"name": {"type": "string"}}},
	    "AccessToken": {"type": "object", "properties": {"token": {"type": "string"}}}
	  }
	}`)
	gf, err := guardfile.Parse([]byte(`wrap ward ops demo {
		spec s
		base-url "https://example.test/api"
		auth header-token { header H; value ssm S }
		can list comment { op "commentList" }
	}`))
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}

	pruned, err := Prune(spec, gf)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	var doc struct {
		Responses   map[string]json.RawMessage `json:"responses"`
		Definitions map[string]json.RawMessage `json:"definitions"`
	}
	if err := json.Unmarshal(pruned, &doc); err != nil {
		t.Fatalf("pruned spec is not valid json: %v", err)
	}
	if !equalSorted(keysOf(doc.Responses), []string{"CommentList"}) {
		t.Errorf("pruned responses = %v, want [CommentList] (AccessTokenList is ungranted)", keysOf(doc.Responses))
	}
	if !equalSorted(keysOf(doc.Definitions), []string{"Comment", "User"}) {
		t.Errorf("pruned definitions = %v, want [Comment User] (AccessToken is unreachable)", keysOf(doc.Definitions))
	}
	// The regression itself: a dangling shared-response ref breaks the upgrade.
	if _, err := Build(Config{Guardfile: gf, Spec: pruned}); err != nil {
		t.Fatalf("Build on pruned spec (dangling shared response would fail the openapi3 upgrade): %v", err)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
