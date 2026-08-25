// Package opcore is the urfave/cli-free engine core of the spec-driven verb
// design: the per-operation descriptor, the request runtime, and the
// self-guarding Operation.Execute entrypoint. specverb projects this core onto
// a urfave/cli tree; a non-CLI consumer (ward-mcp) drives Operation.Execute
// directly and is still fully gated. See docs/specverb.md for the projection shape.
package opcore

// Descriptor is the tiny per-operation payload the generic action binds to,
// isolated from the static request machinery.
type Descriptor struct {
	VerbName       string         // dotted audit name, e.g. ward.ops.forgejo.repo.create
	Group          string         // CLI group noun, e.g. repo
	Leaf           string         // CLI leaf verb, e.g. create
	Method         string         // HTTP method
	Path           string         // path template, e.g. /repos/{owner}/{repo}
	PathParams     []string       // ordered positional args drawn from the path
	BodyFlags      []Field        // request-body fields, including nested object/array shapes
	BodyMappings   []BodyMapping  // required input paths projected onto fresh top-level body keys
	QueryFlags     []Field        // query params, including typed scalar-array shapes
	QueryExclusive [][]string     // local query-name groups where at most one may be supplied
	FormFlags      []Field        // formData params, where "file" types take a path
	FixedBody      map[string]any // pinned body keys, alone for a state toggle or seeding a mapped body
	Destructive    bool           // leaf mutates irreversibly (delete)
	Grant          string         // the authorizing grant sentence, e.g. "can delete repos"
	Describe       string         // optional Guardfile describe "..." note, "" if none
	FailWhen       string         // optional JMESPath response postcondition; truthy rejects a successful call
	Proxy          *Proxy         // non-nil for an inline MCP proxy grant
	GraphQL        *GraphQL       // non-nil for a `graphql` grant; owns the whole body
	SQL            *SQL           // non-nil for a `sql` grant; reaches a database, not a URL
	// RawResponse marks a success response offering no JSON, written through
	// untouched. See docs/specverb-request.md.
	RawResponse bool
	// MethodInferred marks a Method guessed from an unrecognised verb rather
	// than looked up or stated. See docs/opcore-inline.md.
	MethodInferred bool
}

// BodyMapping projects one required nested string input path onto one
// top-level outgoing JSON key. Unmapped input is never forwarded.
type BodyMapping struct {
	SourcePath []string
	Target     string
	// Type is the JSON type the leaf projects, defaulting to string. A mapped
	// leaf used to be string-only in every configuration. See umbra#312.
	Type string
	// Items is the element type when Type is "array".
	Items string
}

// Label names the leaf in operator-facing errors, satisfying stepflow.Leaf so a
// resolved Descriptor can drive a complex-action step. See pkg/stepflow.
func (d Descriptor) Label() string { return d.Leaf }

// Proxy is one inline MCP proxy grant: local tool, exact upstream mapping, and
// request/response guards. The consumer resolves the upstream schema at runtime.
type Proxy struct {
	Name     string       // local served tool name
	Upstream UpstreamTool // exact upstream MCP tool mapping
	Allow    []ProxyRule  // request-time allow rules
	Deny     []ProxyRule  // request-time deny rules
	PostCall []ProxyRule  // response-time checks after the upstream call
	Describe string       // optional human note
}

// UpstreamTool names the proxied MCP server and upstream tool exactly.
type UpstreamTool struct {
	Server string
	Tool   string
}

// ProxyRule is one simple regex guard over a string field such as url, text,
// content, target, element, key, or state.
type ProxyRule struct {
	Field    string
	Patterns []string
}

// Field is one spec input. The flat cases lower to CLI flags, while body fields
// may carry nested shape and query fields may carry numeric or array bounds.
type Field struct {
	Name         string // local input name and the default outgoing field or parameter name
	UpstreamName string // outgoing query parameter name when it differs from Name
	Type         string // swagger type: string|boolean|integer|number|array|object
	Items        string // scalar element type when Type is "array" and Item is nil
	Item         *Field // nested element schema when Type is "array" and Item is object/array
	Fields       []Field
	Raw          bool
	Required     bool // required in the schema, enforced at request assembly
	Desc         string
	Minimum      *float64 // inclusive numeric lower bound
	Maximum      *float64 // inclusive numeric upper bound
	MinItems     *int     // inclusive array-length lower bound
	MaxItems     *int     // inclusive array-length upper bound
}

// QueryName returns the outgoing query parameter name for this field. An empty
// UpstreamName preserves the historical Name-to-wire mapping.
func (f Field) QueryName() string {
	if f.UpstreamName != "" {
		return f.UpstreamName
	}
	return f.Name
}

// TypeLabel renders the flag's type for help and describe output.
func (f Field) TypeLabel() string {
	if f.Type == "array" {
		if f.Item != nil {
			return "[]" + f.Item.TypeLabel()
		}
		if f.Items != "" {
			return "[]" + f.Items
		}
		return "[]string"
	}
	return f.Type
}
