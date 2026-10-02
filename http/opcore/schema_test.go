package opcore_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/coilyco/umbra/http/opcore"
)

// schemaDesc is a create-shaped leaf exercising every input location and type:
// path params, optional typed query, required body scalar, array field, form.
func schemaDesc() opcore.Descriptor {
	return opcore.Descriptor{
		VerbName:   "test.repo.issues.create",
		Method:     http.MethodPost,
		Path:       "/repos/{owner}/{repo}/issues",
		PathParams: []string{"owner", "repo"},
		QueryFlags: []opcore.Field{{Name: "state", Type: "string", Desc: "issue state"}},
		BodyFlags: []opcore.Field{
			{Name: "title", Type: "string", Required: true},
			{Name: "count", Type: "integer"},
			{Name: "labels", Type: "array", Items: "string"},
		},
		FormFlags: []opcore.Field{{Name: "attachment", Type: "string"}},
	}
}

func TestInputSchemaLocationsAndTypes(t *testing.T) {
	s := schemaDesc().InputSchema()

	want := map[string]opcore.Property{
		"owner":      {Type: "string", Location: opcore.LocationPath},
		"repo":       {Type: "string", Location: opcore.LocationPath},
		"state":      {Type: "string", Location: opcore.LocationQuery, Description: "issue state"},
		"title":      {Type: "string", Location: opcore.LocationBody},
		"count":      {Type: "integer", Location: opcore.LocationBody},
		"labels":     {Type: "array", Items: "string", Location: opcore.LocationBody},
		"attachment": {Type: "string", Location: opcore.LocationForm},
	}
	if !reflect.DeepEqual(s.Properties, want) {
		t.Errorf("properties = %#v, want %#v", s.Properties, want)
	}

	// Path params are always required; the required query/body split follows the
	// Field.Required bit - only path owner/repo and the required body title.
	wantRequired := map[string]bool{"owner": true, "repo": true, "title": true}
	got := map[string]bool{}
	for _, r := range s.Required {
		got[r] = true
	}
	if !reflect.DeepEqual(got, wantRequired) {
		t.Errorf("required = %v, want %v", s.Required, wantRequired)
	}
}

func TestInputSchemaPreservesQueryAlias(t *testing.T) {
	d := opcore.Descriptor{QueryFlags: []opcore.Field{
		{Name: "search_query", UpstreamName: "query", Type: "string", Required: true},
	}}
	s := d.InputSchema()
	prop, ok := s.Properties["search_query"]
	if !ok {
		t.Fatal("schema omitted the local query input name")
	}
	if prop.UpstreamName != "query" || prop.Location != opcore.LocationQuery {
		t.Errorf("aliased property = %+v", prop)
	}
	if _, leaked := s.Properties["query"]; leaked {
		t.Fatal("schema exposed the reserved upstream name as a local input")
	}

	var doc map[string]any
	if err := json.Unmarshal(s.JSONSchema(), &doc); err != nil {
		t.Fatalf("unmarshal JSON schema: %v", err)
	}
	props := doc["properties"].(map[string]any)
	alias := props["search_query"].(map[string]any)
	if alias["x-opcore-upstream-name"] != "query" {
		t.Errorf("upstream schema annotation = %v", alias["x-opcore-upstream-name"])
	}
}

func TestInputSchemaPreservesQueryBoundsAndExclusions(t *testing.T) {
	minimum, maximum := float64(1), float64(100)
	minItems, maxItems := 1, 25
	d := opcore.Descriptor{
		QueryFlags: []opcore.Field{
			{Name: "limit", Type: "integer", Minimum: &minimum, Maximum: &maximum},
			{Name: "author_id", Type: "array", Items: "string", MinItems: &minItems, MaxItems: &maxItems},
			{Name: "before", Type: "string"},
			{Name: "after", Type: "string"},
			{Name: "around", Type: "string"},
		},
		QueryExclusive: [][]string{{"before", "after", "around"}},
	}
	s := d.InputSchema()
	if got := s.Properties["limit"]; got.Minimum == nil || *got.Minimum != 1 || got.Maximum == nil || *got.Maximum != 100 {
		t.Fatalf("numeric bounds = %+v", got)
	}
	if got := s.Properties["author_id"]; got.MinItems == nil || *got.MinItems != 1 || got.MaxItems == nil || *got.MaxItems != 25 {
		t.Fatalf("array bounds = %+v", got)
	}
	if want := [][]string{{"before", "after", "around"}}; !reflect.DeepEqual(s.MutuallyExclusive, want) {
		t.Fatalf("mutually exclusive = %v, want %v", s.MutuallyExclusive, want)
	}

	var doc map[string]any
	if err := json.Unmarshal(s.JSONSchema(), &doc); err != nil {
		t.Fatalf("unmarshal JSON schema: %v", err)
	}
	props := doc["properties"].(map[string]any)
	limit := props["limit"].(map[string]any)
	if limit["minimum"] != float64(1) || limit["maximum"] != float64(100) {
		t.Errorf("limit schema = %v", limit)
	}
	authors := props["author_id"].(map[string]any)
	if authors["minItems"] != float64(1) || authors["maxItems"] != float64(25) {
		t.Errorf("author array schema = %v", authors)
	}
	allOf := doc["allOf"].([]any)
	if len(allOf) != 3 {
		t.Fatalf("pairwise exclusion constraints = %d, want 3", len(allOf))
	}
	pairs := map[string]bool{}
	for _, raw := range allOf {
		not := raw.(map[string]any)["not"].(map[string]any)
		required := not["required"].([]any)
		pairs[required[0].(string)+"/"+required[1].(string)] = true
	}
	wantPairs := map[string]bool{"before/after": true, "before/around": true, "after/around": true}
	if !reflect.DeepEqual(pairs, wantPairs) {
		t.Errorf("exclusion pairs = %v, want %v", pairs, wantPairs)
	}
}

func TestInputSchemaFixedBodyOmitted(t *testing.T) {
	// A fixed-body leaf mounts no body flags on the CLI, so its input schema is
	// path params only - nothing the caller supplies for the body.
	d := opcore.Descriptor{
		Method:     http.MethodPatch,
		Path:       "/repos/{owner}/{repo}",
		PathParams: []string{"owner", "repo"},
		FixedBody:  map[string]any{"archived": true},
	}
	s := d.InputSchema()
	if len(s.Properties) != 2 {
		t.Fatalf("properties = %#v, want only owner+repo", s.Properties)
	}
	if _, ok := s.Properties["archived"]; ok {
		t.Errorf("fixed-body key must not appear as an input")
	}
}

func TestInputSchemaBodyMappingsExposeRequiredNestedSources(t *testing.T) {
	d := opcore.Descriptor{BodyMappings: []opcore.BodyMapping{
		{SourcePath: []string{"commonAnnotations", "summary"}, Target: "text"},
		{SourcePath: []string{"commonAnnotations", "description"}, Target: "description"},
	}}
	s := d.InputSchema()
	annotations := s.Properties["commonAnnotations"]
	if annotations.Type != "object" || annotations.Location != opcore.LocationBody {
		t.Fatalf("commonAnnotations = %#v", annotations)
	}
	wantChildren := map[string]opcore.Property{
		"summary":     {Type: "string"},
		"description": {Type: "string"},
	}
	if !reflect.DeepEqual(annotations.Properties, wantChildren) {
		t.Fatalf("nested properties = %#v, want %#v", annotations.Properties, wantChildren)
	}
	if want := []string{"summary", "description"}; !reflect.DeepEqual(annotations.Required, want) {
		t.Fatalf("nested required = %v, want %v", annotations.Required, want)
	}
	if want := []string{"commonAnnotations"}; !reflect.DeepEqual(s.Required, want) {
		t.Fatalf("top-level required = %v, want %v", s.Required, want)
	}
}

func TestJSONSchemaDraft07(t *testing.T) {
	raw := schemaDesc().InputSchema().JSONSchema()

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("emitted JSON does not parse: %v\n%s", err, raw)
	}
	if doc["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("$schema = %v", doc["$schema"])
	}
	if doc["type"] != "object" {
		t.Errorf("type = %v, want object", doc["type"])
	}

	props, ok := doc["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is not an object: %v", doc["properties"])
	}

	// A typed scalar carries its JSON-schema type.
	if p := props["count"].(map[string]any); p["type"] != "integer" {
		t.Errorf("count type = %v, want integer", p["type"])
	}

	// A query field's description survives into the schema.
	if p := props["state"].(map[string]any); p["description"] != "issue state" {
		t.Errorf("state description = %v", p["description"])
	}

	// An array field emits an items sub-schema with the element type.
	labels := props["labels"].(map[string]any)
	if labels["type"] != "array" {
		t.Errorf("labels type = %v, want array", labels["type"])
	}
	items, ok := labels["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Errorf("labels items = %v, want {type:string}", labels["items"])
	}

	// The neutral Location hint has no JSON-schema equivalent and is omitted.
	if _, leaked := props["owner"].(map[string]any)["location"]; leaked {
		t.Errorf("location hint must not leak into the JSON schema")
	}

	// required lists exactly the path params plus the required body field.
	req, ok := doc["required"].([]any)
	if !ok {
		t.Fatalf("required is not an array: %v", doc["required"])
	}
	gotReq := map[string]bool{}
	for _, r := range req {
		gotReq[r.(string)] = true
	}
	wantReq := map[string]bool{"owner": true, "repo": true, "title": true}
	if !reflect.DeepEqual(gotReq, wantReq) {
		t.Errorf("required = %v, want %v", req, wantReq)
	}
}

func TestJSONSchemaNoRequiredOmitsKey(t *testing.T) {
	// A leaf with only optional query flags emits no `required` key at all.
	d := opcore.Descriptor{
		Method:     http.MethodGet,
		Path:       "/search",
		QueryFlags: []opcore.Field{{Name: "q", Type: "string"}},
	}
	var doc map[string]any
	if err := json.Unmarshal(d.InputSchema().JSONSchema(), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := doc["required"]; ok {
		t.Errorf("required key should be absent when nothing is required")
	}
}

func TestJSONSchemaNestedBodyShape(t *testing.T) {
	d := opcore.Descriptor{
		Method: http.MethodPost,
		Path:   "/query",
		BodyFlags: []opcore.Field{
			{Name: "start", Type: "integer", Required: true},
			{Name: "requestType", Type: "string", Required: true},
			{Name: "variables", Type: "object", Raw: true},
			{Name: "formatOptions", Type: "object", Raw: true},
			{Name: "compositeQuery", Type: "object", Raw: true, Required: true},
			{Name: "filters", Type: "object", Fields: []opcore.Field{
				{Name: "min", Type: "number", Required: true},
				{Name: "max", Type: "number"},
			}},
			{Name: "labels", Type: "array", Items: "string"},
			{Name: "payloads", Type: "array", Raw: true},
		},
	}
	var doc map[string]any
	if err := json.Unmarshal(d.InputSchema().JSONSchema(), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	props := doc["properties"].(map[string]any)
	filters := props["filters"].(map[string]any)
	nested := filters["properties"].(map[string]any)
	if nested["min"].(map[string]any)["type"] != "number" {
		t.Fatalf("nested min type = %v", nested["min"])
	}
	req := filters["required"].([]any)
	if len(req) != 1 || req[0].(string) != "min" {
		t.Fatalf("nested required = %v, want [min]", req)
	}
	if got := props["compositeQuery"].(map[string]any)["x-opcore-raw"]; got != true {
		t.Fatalf("raw marker = %v, want true", got)
	}
	if got := props["payloads"].(map[string]any)["x-opcore-raw"]; got != true {
		t.Fatalf("raw array marker = %v, want true", got)
	}
	if _, ok := props["payloads"].(map[string]any)["items"]; ok {
		t.Fatalf("raw array should not emit items: %v", props["payloads"])
	}
	wantReq := map[string]bool{"start": true, "requestType": true, "compositeQuery": true}
	gotReq := map[string]bool{}
	for _, r := range doc["required"].([]any) {
		gotReq[r.(string)] = true
	}
	if !reflect.DeepEqual(gotReq, wantReq) {
		t.Fatalf("required = %v, want %v", gotReq, wantReq)
	}
}
