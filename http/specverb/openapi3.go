// The engine IR: a resolved OpenAPI 3.x document (kin-openapi's openapi3.T) plus
// a path/method operation index the engine binds to. A Swagger 2.0 document is
// upgraded to this same model in swagger.go. See docs/specverb.md.

package specverb

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// jsonMediaTypes are the body content types read as JSON, first present wins. The
// "*/*" fallback is where an upgraded Swagger-2.0 body lands (Forgejo: no consumes).
var jsonMediaTypes = []string{"application/json", "application/hujson", "application/json5", "*/*"}

// formMediaTypes are the body content types whose schema properties lower to form
// flags; an upgraded Swagger-2.0 `in: formData` set lands here (file uploads, scalars).
var formMediaTypes = []string{"multipart/form-data", "application/x-www-form-urlencoded"}

// spec is the engine's resolved view of an API document: an operation index over
// kin-openapi's fully dereferenced model, keyed by path and lowercase method.
type spec struct {
	ops map[string]map[string]operation
}

// operation is one resolved path+method operation: the kin-openapi operation and
// the path item it inherits shared parameters from, plus its placement.
type operation struct {
	operationID string
	method      string // upper-case HTTP method
	path        string
	item        *openapi3.PathItem
	op          *openapi3.Operation
}

// rawResponseOp reports a success response offering no JSON at all.
// See docs/specverb-request.md.
func rawResponseOp(op *openapi3.Operation) bool {
	if op == nil || op.Responses == nil {
		return false
	}
	raw := false
	for _, code := range []string{"200", "206"} {
		ref := op.Responses.Value(code)
		if ref == nil || ref.Value == nil || len(ref.Value.Content) == 0 {
			continue
		}
		// Offering JSON among others is content negotiation, not a statement
		// that the body is HTML. See docs/specverb-request.md.
		if offersJSON(ref.Value.Content) {
			return false
		}
		raw = true
	}
	return raw
}

// offersJSON reports whether a response lists any JSON media type at all.
func offersJSON(content map[string]*openapi3.MediaType) bool {
	for media := range content {
		if jsonMediaType(media) {
			return true
		}
	}
	return false
}

// jsonMediaType matches the JSON family, including suffixed types such as
// application/vnd.api+json, and ignores any parameters after a semicolon.
func jsonMediaType(media string) bool {
	base, _, _ := strings.Cut(media, ";")
	base = strings.ToLower(strings.TrimSpace(base))
	return base == "application/json" || strings.HasSuffix(base, "+json")
}

// param is one promoted spec input the engine reads off the resolved model: a
// scalar query parameter, or a form-body property (a "file" upload or a scalar).
type param struct {
	Name        string
	Required    bool
	Type        string // scalar swagger type, or "file" for a binary form upload
	Description string
}

// newSpec indexes a resolved document by path and lowercase method, the form the
// engine's resolution and descriptor passes bind to.
func newSpec(doc *openapi3.T) *spec {
	s := &spec{ops: map[string]map[string]operation{}}
	for path, item := range doc.Paths.Map() {
		methods := map[string]operation{}
		for method, op := range item.Operations() {
			methods[strings.ToLower(method)] = operation{
				operationID: op.OperationID,
				method:      strings.ToUpper(method),
				path:        path,
				item:        item,
				op:          op,
			}
		}
		s.ops[path] = methods
	}
	return s
}

// loadOpenAPI3 turns a kin-openapi loader panic (GHSA-mh7x-f8wq-4jhx, held out by
// the go.mod pin, COI-3137) into an error so callers fail closed.
func loadOpenAPI3(raw []byte) (doc *openapi3.T, err error) {
	defer func() {
		if r := recover(); r != nil {
			doc, err = nil, fmt.Errorf("specverb: openapi 3.x loader panic: %v", r)
		}
	}()
	return openapi3.NewLoader().LoadFromData(raw)
}

// sniffOpenAPI3 reports whether raw loads as an OpenAPI 3.x document, the YAML
// fallback path when JSON version-sniffing did not settle the version.
func sniffOpenAPI3(raw []byte) (is3 bool, ok bool) {
	doc, err := loadOpenAPI3(raw)
	if err != nil || doc == nil {
		return false, false
	}
	return strings.HasPrefix(doc.OpenAPI, "3."), true
}

// parseOpenAPI3 loads an OpenAPI 3.x document (JSON or YAML) into the engine IR.
// kin-openapi dereferences internal $refs as it loads. See docs/specverb.md.
func parseOpenAPI3(raw []byte) (*spec, error) {
	doc, err := loadOpenAPI3(raw)
	if err != nil {
		return nil, fmt.Errorf("specverb: parse openapi 3.x: %w", err)
	}
	if doc.Paths == nil || doc.Paths.Len() == 0 {
		return nil, fmt.Errorf("specverb: spec has no paths")
	}
	return newSpec(doc), nil
}

// findOp locates the addressed operation, failing closed when it is absent
// (an undeclared op can never be mounted).
func (s *spec) findOp(a opAddr) (method, path string, op operation, err error) {
	if a.id == "" {
		return s.findOpByRoute(a)
	}
	for _, methods := range s.ops {
		for _, o := range methods {
			if o.operationID == a.id {
				return o.method, o.path, o, nil
			}
		}
	}
	return "", "", operation{}, fmt.Errorf("specverb: operationId %q not found in spec (fail-closed)", a.id)
}

// findOpByRoute locates an operation by method+path. Method and path are unique
// within an OpenAPI document by construction, so this is total, not a heuristic.
func (s *spec) findOpByRoute(a opAddr) (method, path string, op operation, err error) {
	methods, ok := s.ops[a.path]
	if !ok {
		return "", "", operation{}, fmt.Errorf("specverb: path %q not found in spec (fail-closed)", a.path)
	}
	o, ok := methods[strings.ToLower(a.method)]
	if !ok {
		return "", "", operation{}, fmt.Errorf("specverb: path %q declares no %s operation (fail-closed)", a.path, a.method)
	}
	return o.method, o.path, o, nil
}

// parameters returns the operation's effective parameters: the path-item's shared
// parameters followed by the operation's own, the order kin-openapi merges them.
func (op operation) parameters() openapi3.Parameters {
	var item openapi3.Parameters
	if op.item != nil {
		item = op.item.Parameters
	}
	var own openapi3.Parameters
	if op.op != nil {
		own = op.op.Parameters
	}
	return append(append(openapi3.Parameters{}, item...), own...)
}

// bodySchema resolves an operation's JSON request-body schema. ok is false when
// the op takes no JSON body (a normal case, e.g. a form-body or body-less op).
func (s *spec) bodySchema(op operation) (schema *openapi3.Schema, ok bool) {
	return contentSchema(op, jsonMediaTypes)
}

// contentSchema returns the first present media type's resolved schema among the
// preferred types, the shared lookup behind the JSON-body and form-body readers.
func contentSchema(op operation, mediaTypes []string) (*openapi3.Schema, bool) {
	if op.op == nil || op.op.RequestBody == nil || op.op.RequestBody.Value == nil {
		return nil, false
	}
	content := op.op.RequestBody.Value.Content
	for _, mt := range mediaTypes {
		if media, present := content[mt]; present && media.Schema != nil && media.Schema.Value != nil {
			return media.Schema.Value, true
		}
	}
	return nil, false
}

// queryParamsOf returns the operation's scalar `in: query` parameters, the ones
// that lower to a typed CLI flag; non-scalar (array/object) query params are skipped.
func queryParamsOf(op operation) []param {
	var out []param
	for _, ref := range op.parameters() {
		if ref == nil || ref.Value == nil {
			continue
		}
		p := ref.Value
		t := schemaRefType(p.Schema)
		if p.In != "query" || !isScalar(t) {
			continue
		}
		out = append(out, param{Name: p.Name, Required: p.Required, Type: t, Description: p.Description})
	}
	return out
}

// formParamsOf returns an operation's form-body inputs: the multipart/url-encoded
// requestBody's scalar and "file" (binary) properties, the asset-upload surface.
func formParamsOf(op operation) []param {
	schema, ok := contentSchema(op, formMediaTypes)
	if !ok {
		return nil
	}
	required := requiredSet(schema.Required)
	var out []param
	for _, name := range sortedSchemaNames(schema.Properties) {
		if p, ok := formParam(name, schema.Properties[name], required[name]); ok {
			out = append(out, p)
		}
	}
	return out
}

// formParam lowers one form-body property to a param: a "file" upload (the binary
// string an `in: formData` file became) or a scalar; ok=false for other shapes.
func formParam(name string, ref *openapi3.SchemaRef, required bool) (param, bool) {
	if ref == nil || ref.Value == nil {
		return param{}, false
	}
	ps := ref.Value
	switch {
	case ps.Format == "binary":
		return param{Name: name, Required: required, Type: "file", Description: ps.Description}, true
	case isScalar(schemaType(ps)):
		return param{Name: name, Required: required, Type: schemaType(ps), Description: ps.Description}, true
	}
	return param{}, false
}

// requiredSet lifts a schema's required-field list into a membership set.
func requiredSet(required []string) map[string]bool {
	set := make(map[string]bool, len(required))
	for _, r := range required {
		set[r] = true
	}
	return set
}

// sortedSchemaNames returns a schema property map's keys in stable order, so the
// promoted flag set and help stay deterministic across runs (map iteration is random).
func sortedSchemaNames(props openapi3.Schemas) []string {
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// schemaRefType returns the scalar type of a parameter's schema, "string" when
// the schema is absent so a bare param still lowers to a usable flag.
func schemaRefType(ref *openapi3.SchemaRef) string {
	if ref == nil || ref.Value == nil {
		return "string"
	}
	return schemaType(ref.Value)
}

// schemaType reads the first non-null type from a schema, collapsing the OpenAPI
// 3.1 type-list (e.g. ["string","null"]) back to the single type the engine models.
func schemaType(s *openapi3.Schema) string {
	if s == nil || s.Type == nil {
		return ""
	}
	for _, t := range s.Type.Slice() {
		if t != "null" {
			return t
		}
	}
	return ""
}
