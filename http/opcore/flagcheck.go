package opcore

import "fmt"

// ReservedFlagNames are the universal per-leaf engine flags no promoted input
// (query, body, or form field) may shadow. See docs/specverb-request.md.
var ReservedFlagNames = map[string]bool{
	"dry-run": true, "query": true, "output": true, "body-file": true,
}

// descriptorBodyInputs gathers every body-side caller input. A graphql variable
// and a sql parameter are caller inputs too, held to the same rules.
func descriptorBodyInputs(desc Descriptor) []Field {
	out := append(append([]Field{}, desc.BodyFlags...), bodyMappingFields(desc.BodyMappings)...)
	if desc.GraphQL != nil {
		out = append(out, desc.GraphQL.Variables...)
	}
	if desc.SQL != nil {
		out = append(out, desc.SQL.Params...)
	}
	return out
}

// CheckFlagCollisions rejects reserved or duplicate local inputs and duplicate
// outgoing query names. Both descriptor sources share this fail-closed check.
func CheckFlagCollisions(desc Descriptor) error {
	return checkFlagCollisions(desc, ReservedFlagNames)
}

// checkFlagCollisions is [CheckFlagCollisions] against a given reserved set, which a
// consumer with no CLI flags to protect leaves empty. Duplicates are refused either way.
func checkFlagCollisions(desc Descriptor, reserved map[string]bool) error {
	if err := validateBodyMappingMode(desc); err != nil {
		return fmt.Errorf("opcore: %s: %w", desc.VerbName, err)
	}
	seen := map[string]bool{}
	bodyInputs := descriptorBodyInputs(desc)
	all := append(append([]Field{}, desc.QueryFlags...), bodyInputs...)
	for _, f := range append(all, desc.FormFlags...) {
		if reserved[f.Name] {
			return fmt.Errorf("opcore: %s: input %q collides with a reserved engine flag (fail-closed)", desc.VerbName, f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("opcore: %s: two inputs both name %q (fail-closed)", desc.VerbName, f.Name)
		}
		seen[f.Name] = true
	}
	for _, f := range append(bodyInputs, desc.FormFlags...) {
		if f.UpstreamName != "" {
			return fmt.Errorf("opcore: %s: input %q sets an upstream name outside query parameters (fail-closed)", desc.VerbName, f.Name)
		}
	}
	wireNames := map[string]string{}
	for _, f := range desc.QueryFlags {
		if f.UpstreamName == f.Name && f.UpstreamName != "" {
			return fmt.Errorf("opcore: %s: query input %q repeats its local name as the upstream name (fail-closed)", desc.VerbName, f.Name)
		}
		wireName := f.QueryName()
		if localName, ok := wireNames[wireName]; ok {
			return fmt.Errorf("opcore: %s: query inputs %q and %q both map to upstream parameter %q (fail-closed)", desc.VerbName, localName, f.Name, wireName)
		}
		wireNames[wireName] = f.Name
	}
	return nil
}
