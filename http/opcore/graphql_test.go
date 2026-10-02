package opcore_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/valuesource"
)

// anilistDocument is the shape mcp-beaver#65 could not express: an authored
// query the caller must not influence, plus two caller-supplied variables.
const anilistDocument = `query ($search: String!, $page: Int = 1) { Page(page: $page) { media(search: $search) { id title { romaji } } } }`

func graphqlSpec(baseURL, block string) string {
	return `wrap ward mcp anilist {
    base-url "` + baseURL + `"
    auth none
    can post search {
        path "/"
` + block + `
    }
}`
}

func graphqlDesc(t *testing.T, block string) opcore.Descriptor {
	t.Helper()
	descs, _, err := opcore.ParseInline([]byte(graphqlSpec("http://127.0.0.1:1", block)))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	return descByLeaf(t, descs, "post")
}

// The ask: one request carrying a fixed document and caller variables.
func TestGraphQLSendsTheDocumentAndVariables(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"Page":{"media":[]}}}`))
	}))
	defer srv.Close()

	descs, _, err := opcore.ParseInline([]byte(graphqlSpec(srv.URL, `        graphql {
            document "`+anilistDocument+`"
        }`)))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	op := opcore.Operation{
		Desc: descByLeaf(t, descs, "post"),
		RT: opcore.NewRuntime(opcore.RuntimeConfig{
			BaseURL: srv.URL, Providers: valuesource.Merge(nil), Client: srv.Client(),
		}),
	}
	if _, err := op.Execute(context.Background(), opcore.Args{
		Body: map[string]any{"search": "cowboy bebop", "page": 2},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got["query"] != anilistDocument {
		t.Errorf("query = %v, want the authored document", got["query"])
	}
	vars, ok := got["variables"].(map[string]any)
	if !ok {
		t.Fatalf("variables = %v, want an object", got["variables"])
	}
	if vars["search"] != "cowboy bebop" {
		t.Errorf("search = %v", vars["search"])
	}
	if vars["page"] != float64(2) {
		t.Errorf("page = %v, want 2", vars["page"])
	}
}

// The document is authored, so it stays out of the schema and a body key the
// document does not declare is dropped rather than forwarded.
func TestGraphQLKeepsTheDocumentOutOfReach(t *testing.T) {
	desc := graphqlDesc(t, `        graphql {
            document "`+anilistDocument+`"
        }`)
	schema := desc.InputSchema()
	for _, reserved := range []string{"query", "operationName", "variables", "document"} {
		if _, present := schema.Properties[reserved]; present {
			t.Errorf("schema exposes %q, which the caller must not set", reserved)
		}
	}

	op := opcore.Operation{Desc: desc, RT: opcore.NewRuntime(opcore.RuntimeConfig{
		BaseURL: "http://127.0.0.1:1", Providers: valuesource.Merge(nil),
	})}
	req, err := op.Resolve(context.Background(), opcore.Args{Body: map[string]any{
		"search": "ok",
		"query":  "mutation { deleteEverything }",
	}}, true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["query"] != anilistDocument {
		t.Fatalf("a caller-supplied `query` reached the body: %v", body["query"])
	}
	vars := body["variables"].(map[string]any)
	if _, leaked := vars["query"]; leaked {
		t.Errorf("an undeclared input was forwarded as a variable: %v", vars)
	}
}

// Variables surface individually and typed, rather than as one opaque object.
// The types come from the document signature, so they cannot disagree with it.
func TestGraphQLDerivesTypedVariablesFromTheDocument(t *testing.T) {
	desc := graphqlDesc(t, `        graphql {
            document "query ($search: String!, $page: Int, $ids: [ID!], $exact: Boolean, $score: Float) { Page { media { id } } }"
            variable "page" describe="1-based page" minimum=1 maximum=100
        }`)
	schema := desc.InputSchema()

	for name, want := range map[string]string{
		"search": "string", "page": "integer", "ids": "array", "exact": "boolean", "score": "number",
	} {
		p, ok := schema.Properties[name]
		if !ok {
			t.Fatalf("schema is missing %q: %v", name, schema.Properties)
		}
		if p.Type != want {
			t.Errorf("%s type = %q, want %q", name, p.Type, want)
		}
		if p.Location != opcore.LocationBody {
			t.Errorf("%s location = %q, want body", name, p.Location)
		}
	}
	if items := schema.Properties["ids"].Items; items != "string" {
		t.Errorf("ids items = %q, want string (ID lowers to string)", items)
	}
	// `String!` is non-null with no default, so the caller must supply it.
	if !contains(schema.Required, "search") {
		t.Errorf("required = %v, want it to include search", schema.Required)
	}
	// A decoration reaches help text and bounds without restating the type.
	page := schema.Properties["page"]
	if page.Description != "1-based page" {
		t.Errorf("page description = %q", page.Description)
	}
	if page.Minimum == nil || *page.Minimum != 1 || page.Maximum == nil || *page.Maximum != 100 {
		t.Errorf("page bounds = %v/%v, want 1/100", page.Minimum, page.Maximum)
	}
}

// A non-null variable carrying a default is optional to the caller: the server
// fills it. Treating it as required would refuse calls the upstream serves.
func TestGraphQLTreatsADefaultedVariableAsOptional(t *testing.T) {
	desc := graphqlDesc(t, `        graphql {
            document "query ($page: Int! = 1, $q: String!) { Page(page: $page) { media(search: $q) { id } } }"
        }`)
	req := desc.InputSchema().Required
	if contains(req, "page") {
		t.Errorf("required = %v, want page absent because it has a default", req)
	}
	if !contains(req, "q") {
		t.Errorf("required = %v, want q present", req)
	}
}

func TestGraphQLFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct{ block, want string }{
		"no document": {`        graphql {
        }`, "needs a `document"},
		"blank document": {`        graphql {
            document "   "
        }`, "must be non-empty"},
		"unknown child": {`        graphql {
            document "query { a }"
            fragmentz "x"
        }`, "unknown `graphql` child"},
		"undeclared variable": {`        graphql {
            document "query ($a: String) { x }"
            variable "b"
        }`, "is not declared by the document"},
		"non-scalar type unstated": {`        graphql {
            document "query ($filter: MediaFilter) { x }"
        }`, "non-scalar type"},
		"two operations": {`        graphql {
            document "query A { x } query B { y }"
        }`, "more than one operation"},
		"not an operation": {`        graphql {
            document "fragment F on T { x }"
        }`, "not an operation"},
		"duplicate document": {`        graphql {
            document "query { a }"
            document "query { b }"
        }`, "duplicate `document`"},
		"duplicate block": {`        graphql {
            document "query { a }"
        }
        graphql {
            document "query { b }"
        }`, "duplicate `graphql`"},
		"combined with body": {`        graphql {
            document "query ($a: String) { x }"
        }
        body {
            field "other" type="string"
        }`, "cannot be combined with `body`"},
		"combined with set": {`        graphql {
            document "query ($a: String) { x }"
        }
        set state="open"`, "cannot be combined with `set`"},
		"variable declared twice": {`        graphql {
            document "query ($a: String, $a: Int) { x }"
        }`, "declares $a twice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := opcore.ParseInline([]byte(graphqlSpec("http://127.0.0.1:1", tc.block)))
			if err == nil {
				t.Fatalf("ParseInline accepted %q", tc.block)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// GraphQL over GET carries the document in the query string, which this node
// does not build. AniList answers one with a 404 rather than a reason.
func TestGraphQLRefusesANonPostMethod(t *testing.T) {
	src := `wrap ward mcp anilist {
    base-url "http://127.0.0.1:1"
    auth none
    can get search {
        path "/"
        graphql {
            document "query ($a: String) { x }"
        }
    }
}`
	_, _, err := opcore.ParseInline([]byte(src))
	if err == nil {
		t.Fatalf("ParseInline accepted a GET graphql grant")
	}
	if !strings.Contains(err.Error(), "POST body") {
		t.Fatalf("error = %q, want it to name the method", err)
	}
}

// A named operation reaches operationName, so a server routing on it works.
func TestGraphQLSendsTheOperationName(t *testing.T) {
	desc := graphqlDesc(t, `        graphql {
            document "query SearchMedia($a: String) { Page { media { id } } }"
        }`)
	if desc.GraphQL == nil || desc.GraphQL.Operation != "SearchMedia" {
		t.Fatalf("operation = %+v, want SearchMedia", desc.GraphQL)
	}
}

// A `$` or a brace inside a string literal is not syntax, and a document whose
// default value contains one must still parse.
func TestGraphQLScannerIgnoresStringsAndComments(t *testing.T) {
	desc := graphqlDesc(t, `        graphql {
            document "# a leading comment\nquery ($q: String = \"a } literal with $notAVariable\") { search(q: $q) { id } }"
        }`)
	names := []string{}
	for _, v := range desc.GraphQL.Variables {
		names = append(names, v.Name)
	}
	if len(names) != 1 || names[0] != "q" {
		t.Fatalf("variables = %v, want just q", names)
	}
}

// The shorthand `{ field }` is a valid anonymous query taking no variables.
func TestGraphQLAcceptsTheAnonymousShorthand(t *testing.T) {
	desc := graphqlDesc(t, `        graphql {
            document "{ viewer { id } }"
        }`)
	if len(desc.GraphQL.Variables) != 0 {
		t.Fatalf("variables = %v, want none", desc.GraphQL.Variables)
	}
	if len(desc.InputSchema().Properties) != 0 {
		t.Fatalf("schema = %v, want empty", desc.InputSchema().Properties)
	}
}

// A stated type resolves what the document could not, and an input object
// passes through as an open subtree rather than a guessed schema.
func TestGraphQLAcceptsAStatedNonScalarType(t *testing.T) {
	desc := graphqlDesc(t, `        graphql {
            document "query ($filter: MediaFilter!) { Page(filter: $filter) { id } }"
            variable "filter" type="object" describe="upstream filter object"
        }`)
	p := desc.InputSchema().Properties["filter"]
	if p.Type != "object" || !p.Raw {
		t.Fatalf("filter = %+v, want an open object", p)
	}
	if !contains(desc.InputSchema().Required, "filter") {
		t.Errorf("required = %v, want filter", desc.InputSchema().Required)
	}
}

// A required variable the caller omitted is refused before the request fires.
func TestGraphQLEnforcesRequiredVariables(t *testing.T) {
	desc := graphqlDesc(t, `        graphql {
            document "`+anilistDocument+`"
        }`)
	op := opcore.Operation{Desc: desc, RT: opcore.NewRuntime(opcore.RuntimeConfig{
		BaseURL: "http://127.0.0.1:1", Providers: valuesource.Merge(nil),
	})}
	_, err := op.Resolve(context.Background(), opcore.Args{Body: map[string]any{"page": 1}}, true)
	if err == nil {
		t.Fatalf("Resolve accepted a call missing a required variable")
	}
	if !strings.Contains(err.Error(), "search") {
		t.Fatalf("error = %q, want it to name search", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
