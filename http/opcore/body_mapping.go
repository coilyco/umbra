package opcore

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
	kdl "github.com/calico32/kdl-go"
)

// parseBodyMappings reads a body block containing only
// `map "source.path" to="target"` declarations.
func parseBodyMappings(nodes []*kdl.Node) ([]BodyMapping, error) {
	out := make([]BodyMapping, 0, len(nodes))
	for _, n := range nodes {
		mapping, err := parseBodyMapping(n)
		if err != nil {
			return nil, err
		}
		out = append(out, mapping)
	}
	if err := validateBodyMappings(out); err != nil {
		return nil, err
	}
	return out, nil
}

func parseBodyMapping(n *kdl.Node) (BodyMapping, error) {
	if n.Name() != "map" {
		return BodyMapping{}, fmt.Errorf("unknown node %q in mapped `body` block (want map; fail-closed)", n.Name())
	}
	args := n.Arguments()
	if len(args) != 1 {
		return BodyMapping{}, fmt.Errorf("`map` expects exactly one source path")
	}
	raw, ok := args[0].RawValue().(string)
	if !ok || raw == "" {
		return BodyMapping{}, fmt.Errorf("`map` expects one non-empty string source path")
	}
	if n.Children() != nil && len(n.Children().Nodes) > 0 {
		return BodyMapping{}, fmt.Errorf("`map` cannot have child nodes; declare a shape with `type=` (fail-closed)")
	}
	props := n.Properties()
	to, exists := props["to"]
	if !exists {
		return BodyMapping{}, fmt.Errorf("`map` needs a `to=\"...\"` property")
	}
	if extra := extraMapProps(props); len(extra) > 0 {
		return BodyMapping{}, fmt.Errorf("`map` takes to, type, and items, not %s (fail-closed)",
			strings.Join(extra, ", "))
	}
	declared, items, terr := mapDeclaredType(props)
	if terr != nil {
		return BodyMapping{}, terr
	}
	target, ok := to.RawValue().(string)
	if !ok || target == "" {
		return BodyMapping{}, fmt.Errorf("`map` needs a non-empty string target")
	}
	return BodyMapping{
		SourcePath: strings.Split(raw, "."),
		Target:     target,
		Type:       declared,
		Items:      items,
	}, nil
}

// mapDeclaredType reads the optional `type=` and `items=` properties. An absent
// type is string, which is what every mapped leaf projected before umbra#312.
func mapDeclaredType(props map[string]kdl.Value) (declared, items string, err error) {
	declared = "string"
	if v, ok := props["type"]; ok {
		declared = strings.TrimSpace(v.String())
	}
	if v, ok := props["items"]; ok {
		items = strings.TrimSpace(v.String())
	}
	switch declared {
	case "string", "integer", "number", "boolean", "object", "array":
	default:
		return "", "", fmt.Errorf("`map` type %q is not supported (want string | integer | number | boolean | object | array; fail-closed)", declared)
	}
	if items != "" && declared != "array" {
		return "", "", fmt.Errorf("`map` items= applies to an array, not %q (fail-closed)", declared)
	}
	if declared == "array" && items == "" {
		items = "string"
	}
	if items != "" {
		switch items {
		case "string", "integer", "number", "boolean", ItemsAny:
		default:
			return "", "", fmt.Errorf("`map` items %q is not supported (want string | integer | number | boolean | any; fail-closed)", items)
		}
	}
	return declared, items, nil
}

// extraMapProps lists the properties beside `to`, sorted so the message is
// stable.
func extraMapProps(props map[string]kdl.Value) []string {
	var extra []string
	for name := range props {
		switch name {
		case "to", "type", "items":
		default:
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return extra
}

func validateBodyMappings(mappings []BodyMapping) error {
	seenSources := map[string]bool{}
	seenTargets := map[string]bool{}
	for _, mapping := range mappings {
		source, err := validateBodyMapping(mapping, seenSources, seenTargets)
		if err != nil {
			return err
		}
		seenSources[source] = true
		seenTargets[mapping.Target] = true
	}
	return validateBodyMappingShapes(seenSources)
}

func validateBodyMapping(mapping BodyMapping, seenSources, seenTargets map[string]bool) (string, error) {
	if len(mapping.SourcePath) == 0 {
		return "", fmt.Errorf("body `map` source path is empty (fail-closed)")
	}
	for _, segment := range mapping.SourcePath {
		if !validMappingName(segment) {
			return "", fmt.Errorf("body `map` source path %q has an invalid segment %q (fail-closed)", strings.Join(mapping.SourcePath, "."), segment)
		}
	}
	if !validMappingName(mapping.Target) {
		return "", fmt.Errorf("body `map` target %q is not a simple top-level key (fail-closed)", mapping.Target)
	}
	source := strings.Join(mapping.SourcePath, ".")
	if seenSources[source] {
		return "", fmt.Errorf("duplicate body `map` source %q (fail-closed)", source)
	}
	if seenTargets[mapping.Target] {
		return "", fmt.Errorf("duplicate body `map` target %q (fail-closed)", mapping.Target)
	}
	return source, nil
}

func validateBodyMappingShapes(sources map[string]bool) error {
	for source := range sources {
		for other := range sources {
			if source != other && strings.HasPrefix(other, source+".") {
				return fmt.Errorf("body `map` sources %q and %q have an ambiguous parent/child shape (fail-closed)", source, other)
			}
		}
	}
	return nil
}

func validateBodyMappingMode(d Descriptor) error {
	if len(d.BodyMappings) == 0 {
		return nil
	}
	// Body fields stay refused: a caller-named key and a mapped one both come
	// from the model, so which wins at a shared name is genuinely ambiguous.
	if len(d.BodyFlags) > 0 {
		return fmt.Errorf("body fields and body mappings cannot be combined (fail-closed)")
	}
	if err := validateFixedBodyMappings(d.FixedBody, d.BodyMappings); err != nil {
		return err
	}
	return validateBodyMappings(d.BodyMappings)
}

// validateFixedBodyMappings refuses a fixed key a mapping also targets. The two
// carry different authority, so a silent winner is the one outcome to refuse.
func validateFixedBodyMappings(fixed map[string]any, mappings []BodyMapping) error {
	for _, mapping := range mappings {
		if _, clash := fixed[mapping.Target]; clash {
			return fmt.Errorf("`set` key %q is also a body `map` target: one key cannot be both pinned and mapped (fail-closed)", mapping.Target)
		}
	}
	return nil
}

func validMappingName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// bodyMappingFields projects mapping sources into the required nested input
// tree exposed to neutral-schema consumers.
func bodyMappingFields(mappings []BodyMapping) []Field {
	var out []Field
	for _, mapping := range mappings {
		if len(mapping.SourcePath) == 0 {
			continue
		}
		out = insertMappingField(out, mapping.SourcePath, mapping)
	}
	return out
}

// leafMappingType defaults an undeclared mapping to string, which is what every
// mapped leaf projected before umbra#312.
func leafMappingType(mapping BodyMapping) string {
	if mapping.Type == "" {
		return "string"
	}
	return mapping.Type
}

func insertMappingField(fields []Field, path []string, mapping BodyMapping) []Field {
	name := path[0]
	for i := range fields {
		if fields[i].Name != name {
			continue
		}
		if len(path) > 1 {
			fields[i].Fields = insertMappingField(fields[i].Fields, path[1:], mapping)
		}
		return fields
	}
	// The leaf takes the mapping's declared type, so the model-facing schema
	// says what the wire will carry rather than always saying string.
	f := Field{Name: name, Type: leafMappingType(mapping), Items: mapping.Items, Required: true}
	if len(path) > 1 {
		f.Type = "object"
		f.Items = ""
		f.Fields = insertMappingField(nil, path[1:], mapping)
	}
	return append(fields, f)
}

// projectMappedBody builds the body from the mappings, seeded with the pins. A
// target colliding with a pin is refused at validation, so neither overwrites.
func projectMappedBody(body map[string]any, mappings []BodyMapping, fixed map[string]any) ([]byte, error) {
	out := make(map[string]any, len(mappings)+len(fixed))
	for name, value := range fixed {
		out[name] = value
	}
	for _, mapping := range mappings {
		value, err := mappedValue(body, mapping)
		if err != nil {
			return nil, err
		}
		out[mapping.Target] = value
	}
	return json.Marshal(out)
}

// mappedValue walks the source path and returns the leaf as the JSON type the
// mapping declares. See docs/specverb-request.md.
func mappedValue(body map[string]any, mapping BodyMapping) (any, error) {
	current, err := walkMappedPath(body, mapping.SourcePath)
	if err != nil {
		return nil, err
	}
	return coerceMapped(current, mapping)
}

// walkMappedPath descends the dotted source path, refusing a non-object
// interior segment and an absent leaf.
func walkMappedPath(body map[string]any, path []string) (any, error) {
	var current any = body
	for i, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, mappedBodyError(path, "is not an object at %q", strings.Join(path[:i], "."))
		}
		value, exists := object[segment]
		if !exists {
			return nil, mappedBodyError(path, "is missing")
		}
		current = value
	}
	return current, nil
}

// coerceMapped checks the leaf against its declared type. A caller supplying
// the wrong shape is refused here rather than by the upstream, which is the
// whole point: the 400 used to be the first and only notice (umbra#312).
func coerceMapped(value any, mapping BodyMapping) (any, error) {
	switch mapping.Type {
	case "", "string":
		s, ok := value.(string)
		if !ok {
			return nil, mappedBodyError(mapping.SourcePath, "must be a string")
		}
		return s, nil
	case "boolean":
		b, ok := value.(bool)
		if !ok {
			return nil, mappedBodyError(mapping.SourcePath, "must be a boolean")
		}
		return b, nil
	case "integer", "number":
		return mappedNumber(value, mapping)
	case "object":
		o, ok := value.(map[string]any)
		if !ok {
			return nil, mappedBodyError(mapping.SourcePath, "must be an object")
		}
		return o, nil
	case "array":
		return mappedArray(value, mapping)
	}
	return nil, mappedBodyError(mapping.SourcePath, "declares unsupported type %q", mapping.Type)
}

// mappedNumber accepts a JSON number, and refuses an integer leaf given a
// fraction so a declared integer cannot arrive as one.
func mappedNumber(value any, mapping BodyMapping) (any, error) {
	f, ok := value.(float64)
	if !ok {
		return nil, mappedBodyError(mapping.SourcePath, "must be a %s", mapping.Type)
	}
	if mapping.Type == "integer" {
		if f != math.Trunc(f) {
			return nil, mappedBodyError(mapping.SourcePath, "must be an integer, not a fraction")
		}
		return int64(f), nil
	}
	return f, nil
}

// mappedArray checks each element against the declared item type, reusing the
// same coercion the flag path uses so the two cannot disagree.
func mappedArray(value any, mapping BodyMapping) (any, error) {
	raw, ok := value.([]any)
	if !ok {
		return nil, mappedBodyError(mapping.SourcePath, "must be an array")
	}
	out := make([]any, 0, len(raw))
	for _, element := range raw {
		item := BodyMapping{SourcePath: mapping.SourcePath, Type: mapping.Items}
		if mapping.Items == ItemsAny {
			out = append(out, element)
			continue
		}
		coerced, err := coerceMapped(element, item)
		if err != nil {
			return nil, err
		}
		out = append(out, coerced)
	}
	return out, nil
}

func mappedBodyError(path []string, format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	return exitcode.New(exitcode.UserError, "user_error",
		fmt.Errorf("mapped body field %q %s", strings.Join(path, "."), detail),
		"supply every mapped source path as a string")
}
