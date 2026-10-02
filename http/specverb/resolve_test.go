package specverb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// loadProvingSpec reads and parses the proving-slice Forgejo spec from testdata.
func loadProvingSpec(t *testing.T) *spec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "forgejo.swagger.v1.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec, err := parseSwagger(raw)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	return spec
}

// TestResolveOpConvention covers the generic conventions against the proving
// slice: CRUD, toggles, compound membership, sub-collection create - no `op`.
func TestResolveOpConvention(t *testing.T) {
	spec := loadProvingSpec(t)
	cases := []struct {
		verb, resource, want string
	}{
		{"get", "repo", "repoGet"},                  // GET item
		{"delete", "repo", "repoDelete"},            // DELETE item
		{"create", "repo", "createCurrentUserRepo"}, // POST collection
		{"list", "issue", "issueListIssues"},        // GET collection
		{"create", "issue", "issueCreateIssue"},
		{"edit", "issue", "issueEditIssue"},   // PATCH item
		{"view", "repo", "repoGet"},           // view aliases get
		{"close", "issue", "issueEditIssue"},  // toggle -> edit op (PATCH item)
		{"reopen", "issue", "issueEditIssue"}, // toggle -> edit op
		{"list", "tasks", "ListActionTasks"},
		{"add", "issue-label", "issueAddLabel"},                    // membership on compound resource
		{"upload-asset", "release", "repoCreateReleaseAttachment"}, // sub-collection create (unknown verb)
	}
	for _, c := range cases {
		t.Run(c.verb+" "+c.resource, func(t *testing.T) {
			got, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: c.verb, Resource: c.resource})
			if err != nil {
				t.Fatalf("resolveOp(%s %s) errored: %v", c.verb, c.resource, err)
			}
			if got.id != c.want {
				t.Errorf("resolveOp(%s %s) = %q, want %q", c.verb, c.resource, got, c.want)
			}
		})
	}
}

// TestResolveOpExplicitOverride asserts an explicit `op` always wins.
func TestResolveOpExplicitOverride(t *testing.T) {
	spec := loadProvingSpec(t)
	got, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "get", Resource: "repo", Op: "repoDelete"})
	if err != nil {
		t.Fatalf("resolveOp with override errored: %v", err)
	}
	if got.id != "repoDelete" {
		t.Errorf("explicit op override = %q, want repoDelete", got)
	}
}

// TestResolveOpLeastNested asserts the canonical (shallowest) path wins when a
// deeper nested path shares the resource segment.
func TestResolveOpLeastNested(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/repos/{owner}/{repo}":          {"get": {operationID: "repoGet"}},
		"/teams/{id}/repos/{org}/{repo}": {"get": {operationID: "orgListTeamRepo"}},
	}}
	got, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "get", Resource: "repo"})
	if err != nil {
		t.Fatalf("least-nested resolve errored: %v", err)
	}
	if got.id != "repoGet" {
		t.Errorf("least-nested = %q, want repoGet (the shallowest path)", got)
	}
}

// TestResolveOpPreferPluralCollection asserts a true (plural) collection segment
// wins over a singular singleton at the same depth (createKey vs updateDeviceKey).
func TestResolveOpPreferPluralCollection(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/tailnet/{tailnet}/keys": {"post": {operationID: "createKey"}},
		"/device/{deviceId}/key":  {"post": {operationID: "updateDeviceKey"}},
	}}
	got, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "create", Resource: "keys"})
	if err != nil {
		t.Fatalf("plural-preference resolve errored: %v", err)
	}
	if got.id != "createKey" {
		t.Errorf("plural preference = %q, want createKey", got)
	}
}

// TestResolveOpSearch asserts the search verb matches the `<resource>/search` suffix.
func TestResolveOpSearch(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/repos/search": {"get": {operationID: "repoSearch"}},
		"/user/repos":   {"get": {operationID: "userCurrentListRepos"}},
	}}
	got, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "search", Resource: "repo"})
	if err != nil {
		t.Fatalf("search resolve errored: %v", err)
	}
	if got.id != "repoSearch" {
		t.Errorf("search repo = %q, want repoSearch", got)
	}
}

// TestResolveOpListChild asserts `list-<child>` lists a sub-collection of the resource.
func TestResolveOpListChild(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/boards/{id}/lists": {"get": {operationID: "get-boards-id-lists"}},
	}}
	got, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "list-lists", Resource: "board"})
	if err != nil {
		t.Fatalf("list-lists resolve errored: %v", err)
	}
	if got.id != "get-boards-id-lists" {
		t.Errorf("list-lists board = %q, want get-boards-id-lists", got)
	}
}

// TestResolveOpOperationIDFallback asserts that when no path-structure candidate
// matches, verb+resource match the operationId words (`get policy` -> getPolicyFile).
func TestResolveOpOperationIDFallback(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/tailnet/{tailnet}/acl":          {"get": {operationID: "getPolicyFile"}, "post": {operationID: "setPolicyFile"}},
		"/tailnet/{tailnet}/acl/validate": {"post": {operationID: "validateAndTestPolicyFile"}},
	}}
	cases := []struct{ verb, want string }{{"get", "getPolicyFile"}, {"set", "setPolicyFile"}}
	for _, c := range cases {
		got, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: c.verb, Resource: "policy"})
		if err != nil {
			t.Fatalf("resolveOp(%s policy) errored: %v", c.verb, err)
		}
		if got.id != c.want {
			t.Errorf("resolveOp(%s policy) = %q, want %q", c.verb, got, c.want)
		}
	}
}

// TestResolveOpOperationIDExactBeatsSuperset asserts the skillsmp shape (/search,
// /ai-search): exact word-set beats superset and a multi-word verb resolves, no `op`.
func TestResolveOpOperationIDExactBeatsSuperset(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/search":    {"get": {operationID: "searchSkills"}},
		"/ai-search": {"get": {operationID: "aiSearchSkills"}},
	}}
	cases := []struct{ verb, want string }{
		{"search", "searchSkills"},      // exact {search,skill} beats superset {ai,search,skill}
		{"ai-search", "aiSearchSkills"}, // multi-word verb, exact match
	}
	for _, c := range cases {
		got, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: c.verb, Resource: "skills"})
		if err != nil {
			t.Fatalf("resolveOp(%s skills) errored: %v", c.verb, err)
		}
		if got.id != c.want {
			t.Errorf("resolveOp(%s skills) = %q, want %q", c.verb, got, c.want)
		}
	}
}

// TestResolveOpOperationIDTwoSupersetsFailClosed asserts that with no exact match,
// two equal-distance supersets remain a genuine tie and still fail closed.
func TestResolveOpOperationIDTwoSupersetsFailClosed(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/ai-search":   {"get": {operationID: "aiSearchSkills"}},
		"/beta-search": {"get": {operationID: "betaSearchSkills"}},
	}}
	_, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "search", Resource: "skills"})
	if err == nil {
		t.Fatal("two-superset resolveOp succeeded, want error")
	}
	for _, want := range []string{"aiSearchSkills", "betaSearchSkills", "operations match"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error %q missing %q", err.Error(), want)
		}
	}
}

// TestResolveOpOperationIDTwoExactFailClosed asserts two operationIds with the same
// word set (searchSkill vs skillSearch, order aside) are a genuine tie, fail closed.
func TestResolveOpOperationIDTwoExactFailClosed(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/search":      {"get": {operationID: "searchSkill"}},
		"/find-skills": {"get": {operationID: "skillSearch"}},
	}}
	_, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "search", Resource: "skills"})
	if err == nil {
		t.Fatal("two-exact resolveOp succeeded, want error")
	}
	for _, want := range []string{"searchSkill", "skillSearch", "operations match"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error %q missing %q", err.Error(), want)
		}
	}
}

// TestResolveOpFailsClosed asserts resolution is deny-by-default: a verb+resource
// with no matching operation is an error, never a silent guess.
func TestResolveOpFailsClosed(t *testing.T) {
	spec := loadProvingSpec(t)
	// no GET .../issues/{index} in the slice -> no item op for "get issue"
	_, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "get", Resource: "issue"})
	if err == nil {
		t.Fatal("resolveOp(get issue) succeeded, want error")
	}
	if !strings.Contains(err.Error(), "no operation matches") {
		t.Errorf("error %q does not name the deny-by-default reason", err.Error())
	}
}

// TestResolveOpAmbiguousFailsClosed asserts a genuine depth tie (two equally-shallow
// paths) fails closed, naming both candidates.
func TestResolveOpAmbiguousFailsClosed(t *testing.T) {
	spec := &spec{ops: map[string]map[string]operation{
		"/orgs/{org}/repos": {"post": {operationID: "createOrgRepo"}},
		"/teams/{id}/repos": {"post": {operationID: "createTeamRepo"}},
	}}
	_, err := resolveOp(spec, guardfile.Grant{Modal: "can", Verb: "create", Resource: "repo"})
	if err == nil {
		t.Fatal("ambiguous resolveOp succeeded, want error")
	}
	for _, want := range []string{"createOrgRepo", "createTeamRepo", "operations match"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error %q missing %q", err.Error(), want)
		}
	}
}
