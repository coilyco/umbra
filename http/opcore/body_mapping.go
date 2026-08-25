package opcore

import (
	"encoding/json"
	"fmt"
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
		return BodyMapping{}, fmt.Errorf("`map` cannot have child nodes: %s", mappedStringLimit)
	}
	props := n.Properties()
	to, exists := props["to"]
	if !exists {
		return BodyMapping{}, fmt.Errorf("`map` needs a `to=\"...\"` property")
	}
	if extra := extraMapProps(props); len(extra) > 0 {
		return BodyMapping{}, fmt.Errorf("`map` takes only `to=\"...\"`, not %s: %s",
			strings.Join(extra, ", "), mappedStringLimit)
	}
	target, ok := to.RawValue().(string)
	if !ok || target == "" {
		return BodyMapping{}, fmt.Errorf("`map` needs a non-empty string target")
	}
	return BodyMapping{SourcePath: strings.Split(raw, "."), Target: target}, nil
}

// mappedStringLimit names the limit behind every shape a `map` refuses. An
// author reaching for one wants a non-string leaf, which mapped bodies cannot
// carry. See docs/specverb-request.md.
const mappedStringLimit = "a mapped leaf projects a string, so it cannot declare a type or a nested shape (umbra#312)"

// extraMapProps lists the properties beside `to`, sorted so the message is
// stable.
func extraMapProps(props map[string]kdl.Value) []string {
	var extra []string
	for name := range props {
		if name != "to" {
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
		out = insertMappingField(out, mapping.SourcePath)
	}
	return out
}

func insertMappingField(fields []Field, path []string) []Field {
	name := path[0]
	for i := range fields {
		if fields[i].Name != name {
			continue
		}
		if len(path) > 1 {
			fields[i].Fields = insertMappingField(fields[i].Fields, path[1:])
		}
		return fields
	}
	f := Field{Name: name, Type: "string", Required: true}
	if len(path) > 1 {
		f.Type = "object"
		f.Fields = insertMappingField(nil, path[1:])
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
		value, err := mappedString(body, mapping.SourcePath)
		if err != nil {
			return nil, err
		}
		out[mapping.Target] = value
	}
	return json.Marshal(out)
}

func mappedString(body map[string]any, path []string) (string, error) {
	var current any = body
	for i, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return "", mappedBodyError(path, "is not an object at %q", strings.Join(path[:i], "."))
		}
		value, exists := object[segment]
		if !exists {
			return "", mappedBodyError(path, "is missing")
		}
		current = value
	}
	value, ok := current.(string)
	if !ok {
		return "", mappedBodyError(path, "must be a string")
	}
	return value, nil
}

func mappedBodyError(path []string, format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	return exitcode.New(exitcode.UserError, "user_error",
		fmt.Errorf("mapped body field %q %s", strings.Join(path, "."), detail),
		"supply every mapped source path as a string")
}
