package mcpverb

import (
	"fmt"
	"sort"
	"strings"

	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/mcpclient"
)

// maxSchemaDepth bounds nesting while lowering a tool's input schema to flags.
// A self-referential schema is legal JSON Schema, so depth is refused, not hit.
const maxSchemaDepth = 8

// FieldsFor lowers one tool's input schema into the ordered inputs a leaf binds.
func FieldsFor(tool mcpclient.Tool) ([]opcore.Field, error) {
	fields, err := fieldsFromSchema(tool.InputSchema, 0)
	if err != nil {
		return nil, fmt.Errorf("mcpverb: tool %s: %w", tool.Name, err)
	}
	return fields, nil
}

// fieldsFromSchema reads the `properties` and `required` of one object schema.
func fieldsFromSchema(schema map[string]any, depth int) ([]opcore.Field, error) {
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("input schema nests deeper than %d levels (fail-closed)", maxSchemaDepth)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, nil
	}
	required := map[string]bool{}
	if list, ok := schema["required"].([]any); ok {
		for _, r := range list {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	fields := make([]opcore.Field, 0, len(names))
	for _, name := range names {
		sub, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		f, err := fieldFromSchema(name, sub, depth)
		if err != nil {
			return nil, fmt.Errorf("property %s: %w", name, err)
		}
		f.Required = required[name]
		fields = append(fields, f)
	}
	return fields, nil
}

// fieldFromSchema lowers one property schema into one input.
func fieldFromSchema(name string, schema map[string]any, depth int) (opcore.Field, error) {
	f := opcore.Field{
		Name: name,
		Type: schemaType(schema),
		Desc: describe(schema),
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		f.Enum = values
	}
	f.Minimum = numberAt(schema, "minimum")
	f.Maximum = numberAt(schema, "maximum")
	f.MinItems = intAt(schema, "minItems")
	f.MaxItems = intAt(schema, "maxItems")

	switch f.Type {
	case "array":
		items, ok := schema["items"].(map[string]any)
		if !ok {
			f.Items = "string"
			return f, nil
		}
		itemType := schemaType(items)
		if itemType == "object" || itemType == "array" {
			nested, err := fieldFromSchema(name, items, depth+1)
			if err != nil {
				return opcore.Field{}, err
			}
			f.Item = &nested
			return f, nil
		}
		f.Items = itemType
	case "object":
		nested, err := fieldsFromSchema(schema, depth+1)
		if err != nil {
			return opcore.Field{}, err
		}
		f.Fields = nested
	}
	return f, nil
}

// schemaType reads the JSON Schema type, defaulting to string.
func schemaType(schema map[string]any) string {
	switch t := schema["type"].(type) {
	case string:
		return normalizeType(t)
	case []any:
		for _, raw := range t {
			s, ok := raw.(string)
			if ok && s != "null" {
				return normalizeType(s)
			}
		}
	}
	return "string"
}

// normalizeType maps a JSON Schema type onto the six opcore knows.
func normalizeType(t string) string {
	switch t {
	case "string", "boolean", "integer", "number", "array", "object":
		return t
	default:
		return "string"
	}
}

// describe renders the flag's help: the schema description plus an enum list,
// since an enum is the one constraint a caller cannot guess from the type.
func describe(schema map[string]any) string {
	desc, _ := schema["description"].(string)
	values, ok := schema["enum"].([]any)
	if !ok || len(values) == 0 {
		return desc
	}
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprint(v))
	}
	list := "one of: " + strings.Join(parts, ", ")
	if desc == "" {
		return list
	}
	return desc + " (" + list + ")"
}

// numberAt reads a numeric bound, absent when the key is missing or not a number.
func numberAt(schema map[string]any, key string) *float64 {
	switch v := schema[key].(type) {
	case float64:
		return &v
	case int:
		f := float64(v)
		return &f
	}
	return nil
}

// intAt reads an integer bound, absent when the key is missing or not a number.
func intAt(schema map[string]any, key string) *int {
	switch v := schema[key].(type) {
	case float64:
		i := int(v)
		return &i
	case int:
		return &v
	}
	return nil
}
