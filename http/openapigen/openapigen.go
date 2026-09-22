// Emit an OpenAPI 3.1 document from a guardfile's resolved descriptors, so a
// KDL-defined surface is consumable by spec-driven tools. See docs/openapigen.md.

package openapigen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/opcore"
)

// Version is the emitted document's OpenAPI version. 3.1 rather than 3.0
// because a pinned body value needs JSON Schema `const`.
const Version = "3.1.0"

// Config carries what the descriptors do not: the document's own identity.
type Config struct {
	Title   string
	Descr   string
	Version string
	BaseURL string
}

// Emit renders descs as an OpenAPI 3.1 document. Descriptors reaching something
// other than a URL are skipped; Skipped names them.
func Emit(descs []opcore.Descriptor, cfg Config) ([]byte, []string, error) {
	doc, skipped, err := build(descs, cfg)
	if err != nil {
		return nil, nil, err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("openapigen: encode: %w", err)
	}
	return append(out, '\n'), skipped, nil
}

// reachesURL reports whether a descriptor addresses an HTTP path at all. A
// graphql, sql, mcp or proxy leaf does not, and emitting one would be a lie.
func reachesURL(d opcore.Descriptor) bool {
	return d.GraphQL == nil && d.SQL == nil && d.MCP == nil && d.Proxy == nil && d.Path != ""
}

func build(descs []opcore.Descriptor, cfg Config) (map[string]any, []string, error) {
	title := cfg.Title
	if title == "" {
		title = "umbra guarded surface"
	}
	version := cfg.Version
	if version == "" {
		version = "0.0.0"
	}
	info := map[string]any{"title": title, "version": version}
	if cfg.Descr != "" {
		info["description"] = cfg.Descr
	}

	paths := map[string]any{}
	var skipped []string
	seen := map[string]string{}
	for _, d := range descs {
		if !reachesURL(d) {
			skipped = append(skipped, d.VerbName)
			continue
		}
		if prior, dup := seen[d.VerbName]; dup {
			return nil, nil, fmt.Errorf(
				"openapigen: two grants share the operationId %q (%s and %s %s); operationIds must be unique",
				d.VerbName, prior, d.Method, d.Path)
		}
		seen[d.VerbName] = d.Method + " " + d.Path

		item, _ := paths[d.Path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[d.Path] = item
		}
		method := strings.ToLower(d.Method)
		if _, clash := item[method]; clash {
			return nil, nil, fmt.Errorf("openapigen: %s %s is granted twice", d.Method, d.Path)
		}
		item[method] = operation(d)
	}

	doc := map[string]any{
		"openapi": Version,
		"info":    info,
		"paths":   paths,
	}
	if cfg.BaseURL != "" {
		doc["servers"] = []any{map[string]any{"url": cfg.BaseURL}}
	}
	sort.Strings(skipped)
	return doc, skipped, nil
}

// operation renders one descriptor. Grant and destructiveness ride as vendor
// extensions: they are umbra's claims, not OpenAPI's.
func operation(d opcore.Descriptor) map[string]any {
	op := map[string]any{
		"operationId": d.VerbName,
		"responses":   responses(d),
	}
	if d.Describe != "" {
		op["summary"] = d.Describe
	}
	if d.Grant != "" {
		op["x-umbra-grant"] = d.Grant
	}
	if d.Destructive {
		op["x-umbra-destructive"] = true
	}
	if params := parameters(d); len(params) > 0 {
		op["parameters"] = params
	}
	if body := requestBody(d); body != nil {
		op["requestBody"] = body
	}
	return op
}

// parameters renders path and query inputs. Path params are always required:
// the path template cannot be built without them.
func parameters(d opcore.Descriptor) []any {
	var out []any
	for _, name := range d.PathParams {
		out = append(out, map[string]any{
			"name": name, "in": "path", "required": true,
			"schema": map[string]any{"type": "string"},
		})
	}
	for _, f := range d.QueryFlags {
		p := map[string]any{
			"name": f.QueryName(), "in": "query",
			"required": f.Required, "schema": schemaOf(f),
		}
		if f.Desc != "" {
			p["description"] = f.Desc
		}
		out = append(out, p)
	}
	return out
}

// responses renders the leaf's success response: a declared `returns` shape
// becomes "200", undeclared keeps the prior undifferentiated "default".
func responses(d opcore.Descriptor) map[string]any {
	if len(d.ReturnFields) == 0 {
		return map[string]any{"default": map[string]any{"description": "upstream response"}}
	}
	return map[string]any{"200": map[string]any{
		"description": "response, narrowed to the granted `returns` shape",
		"content":     map[string]any{"application/json": map[string]any{"schema": returnsSchema(d)}},
	}}
}

// returnsSchema maps ReturnFields onto a JSON Schema object, the same
// conversion requestBody applies to BodyFlags.
func returnsSchema(d opcore.Descriptor) map[string]any {
	props := map[string]any{}
	var required []string
	for _, f := range d.ReturnFields {
		props[f.Name] = schemaOf(f)
		if f.Required {
			required = append(required, f.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	return schema
}

// requestBody renders BodyFlags plus any FixedBody pin. nil when the leaf sends
// no body at all.
func requestBody(d opcore.Descriptor) map[string]any {
	props := map[string]any{}
	var required []string
	for _, f := range d.BodyFlags {
		props[f.Name] = schemaOf(f)
		if f.Required {
			required = append(required, f.Name)
		}
	}
	for key, value := range d.FixedBody {
		// `const` rather than a plain property: the guardfile pins this value
		// and a caller cannot vary it. See docs/openapigen.md.
		props[key] = map[string]any{
			"const":       value,
			"description": "pinned by the guardfile; umbra supplies it and a caller cannot change it",
		}
	}
	if len(props) == 0 {
		return nil
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	return map[string]any{
		"content": map[string]any{"application/json": map[string]any{"schema": schema}},
	}
}

// schemaOf maps one Field onto a JSON Schema, carrying the bounds the guardfile
// already enforces so the document states the same limits.
func schemaOf(f opcore.Field) map[string]any {
	typ := f.Type
	if typ == "" {
		typ = "string"
	}
	s := map[string]any{"type": typ}
	if f.Desc != "" {
		s["description"] = f.Desc
	}
	if len(f.Enum) > 0 {
		s["enum"] = f.Enum
	}
	if f.Minimum != nil {
		s["minimum"] = *f.Minimum
	}
	if f.Maximum != nil {
		s["maximum"] = *f.Maximum
	}
	switch typ {
	case "array":
		applyArray(s, f)
	case "object":
		applyObject(s, f)
	}
	return s
}

// applyArray adds the element schema and any length bounds.
func applyArray(s map[string]any, f opcore.Field) {
	s["items"] = itemSchema(f)
	if f.MinItems != nil {
		s["minItems"] = *f.MinItems
	}
	if f.MaxItems != nil {
		s["maxItems"] = *f.MaxItems
	}
}

// applyObject adds nested properties and the subset of them that is required.
func applyObject(s map[string]any, f opcore.Field) {
	if len(f.Fields) == 0 {
		return
	}
	nested := map[string]any{}
	var required []string
	for _, sub := range f.Fields {
		nested[sub.Name] = schemaOf(sub)
		if sub.Required {
			required = append(required, sub.Name)
		}
	}
	s["properties"] = nested
	if len(required) > 0 {
		sort.Strings(required)
		s["required"] = required
	}
}

// itemSchema renders an array's element type: the nested Item when present,
// otherwise the scalar named by Items, defaulting to string.
func itemSchema(f opcore.Field) map[string]any {
	if f.Item != nil {
		return schemaOf(*f.Item)
	}
	if f.Items != "" {
		return map[string]any{"type": f.Items}
	}
	return map[string]any{"type": "string"}
}
