// Spec-lock pruning: reduce a full upstream Swagger document to the operations a
// Guardfile grants + their transitive schemas. See docs/umbra-cli.md.

package specverb

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coilyco/umbra/http/guardfile"
)

// Prune returns a minimal spec doc holding only the operations gf grants and
// their reachable schemas. Dispatches on version. See docs/umbra-cli.md.
func Prune(spec []byte, gf *guardfile.Guardfile) ([]byte, error) {
	if gf == nil {
		return nil, fmt.Errorf("specverb: prune: nil guardfile")
	}
	version, err := detectSpecVersion(spec)
	if err != nil {
		return nil, err
	}
	if version == 3 {
		return pruneOpenAPI3(spec, gf)
	}
	return pruneSwagger2(spec, gf)
}

// pruneSwagger2 keeps only the granted paths/methods and the transitive closure
// of Swagger 2.0 definitions they reach, re-emitting raw-fidelity JSON.
func pruneSwagger2(spec []byte, gf *guardfile.Guardfile) ([]byte, error) {
	typed, err := parseSwagger2(spec)
	if err != nil {
		return nil, err
	}
	keep, err := grantedPathMethods(typed, gf)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("specverb: prune: parse spec json: %w", err)
	}
	rawPaths, _ := doc["paths"].(map[string]any)

	newPaths := map[string]any{}
	seed := newRefSet()
	for path, methods := range keep {
		pathObj, ok := rawPaths[path].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("specverb: prune: path %q absent from spec paths", path)
		}
		kept := map[string]any{}
		for m := range methods {
			opObj, ok := pathObj[m]
			if !ok {
				return nil, fmt.Errorf("specverb: prune: %s %s absent from spec", strings.ToUpper(m), path)
			}
			kept[m] = opObj
			collectRefs(opObj, seed)
		}
		// Path-level shared parameters apply to every method under the path.
		if params, ok := pathObj["parameters"]; ok {
			kept["parameters"] = params
			collectRefs(params, seed)
		}
		newPaths[path] = kept
	}

	doc["paths"] = newPaths
	// Close over all three shared sections, not definitions alone: passing
	// `responses`/`parameters` verbatim would dangle onto pruned definitions.
	closeSharedSections(doc, seed)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("specverb: prune: marshal pruned spec: %w", err)
	}
	return append(out, '\n'), nil
}

// grantedPathMethods resolves each `can` grant to its (path, lowercase method),
// failing closed like the engine so the lock holds exactly the buildable surface.
func grantedPathMethods(spec *spec, gf *guardfile.Guardfile) (map[string]map[string]bool, error) {
	gf, err := expandWildcards(spec, gf)
	if err != nil {
		return nil, err
	}
	denied := deniedKeys(gf)
	keep := map[string]map[string]bool{}
	for _, g := range gf.Grants {
		if g.Modal != "can" {
			continue
		}
		if _, blocked := denied[grantKey{Verb: g.Verb, Resource: g.Resource}]; blocked {
			continue // deny beats allow: keep the blocked op out of the lock too
		}
		opID, err := resolveOp(spec, g)
		if err != nil {
			return nil, err
		}
		method, path, _, err := spec.findOp(opID)
		if err != nil {
			return nil, err
		}
		if keep[path] == nil {
			keep[path] = map[string]bool{}
		}
		keep[path][strings.ToLower(method)] = true
	}
	return keep, nil
}

// sharedSections names each Swagger 2.0 top-level component bucket and its
// `$ref` prefix, so the pruner closes definitions, responses, and parameters alike.
var sharedSections = []struct{ key, prefix string }{
	{"definitions", "#/definitions/"},
	{"responses", "#/responses/"},
	{"parameters", "#/parameters/"},
}

// refSet buckets collected `$ref` names by their shared section (definitions,
// responses, parameters), so the closure can follow refs across sections.
type refSet struct{ names map[string]map[string]bool }

func newRefSet() *refSet {
	names := map[string]map[string]bool{}
	for _, s := range sharedSections {
		names[s.key] = map[string]bool{}
	}
	return &refSet{names: names}
}

// closeSharedSections replaces each shared section in doc with the transitive
// closure of seed over it (a missing ref drops); empty sections are removed.
func closeSharedSections(doc map[string]any, seed *refSet) {
	closed := map[string]map[string]any{}
	for _, s := range sharedSections {
		closed[s.key] = map[string]any{}
	}
	for moreToVisit(seed, closed) {
		for _, s := range sharedSections {
			raw, _ := doc[s.key].(map[string]any)
			visitSection(raw, seed.names[s.key], closed[s.key], seed)
		}
	}
	for _, s := range sharedSections {
		emitSection(doc, s.key, closed[s.key])
	}
}

// visitSection moves each not-yet-closed name from raw into closed (nil-marking a
// missing one) and folds its refs back into seed for the next pass.
func visitSection(raw, seedNames any, closed map[string]any, seed *refSet) {
	names, _ := seedNames.(map[string]bool)
	rawMap, _ := raw.(map[string]any)
	for name := range names {
		if _, done := closed[name]; done {
			continue
		}
		obj, ok := rawMap[name]
		if !ok {
			closed[name] = nil
			continue
		}
		closed[name] = obj
		collectRefs(obj, seed)
	}
}

// moreToVisit reports whether any seeded name across the sections is still unclosed.
func moreToVisit(seed *refSet, closed map[string]map[string]any) bool {
	for _, s := range sharedSections {
		for name := range seed.names[s.key] {
			if _, done := closed[s.key][name]; !done {
				return true
			}
		}
	}
	return false
}

// emitSection writes the non-nil closed entries back onto doc[key], or removes the
// key entirely when the closure kept nothing.
func emitSection(doc map[string]any, key string, closed map[string]any) {
	section := map[string]any{}
	for name, obj := range closed {
		if obj != nil {
			section[name] = obj
		}
	}
	if len(section) > 0 {
		doc[key] = section
	} else {
		delete(doc, key)
	}
}

// collectRefs walks an arbitrary decoded JSON value, bucketing every shared-section
// `$ref` (`#/definitions/X`, `#/responses/X`, `#/parameters/X`) into set.
func collectRefs(v any, set *refSet) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "$ref" {
				set.add(val)
			}
			collectRefs(val, set)
		}
	case []any:
		for _, e := range t {
			collectRefs(e, set)
		}
	}
}

// add buckets one `$ref` value into its shared section, ignoring non-strings and
// refs that name no shared section.
func (set *refSet) add(ref any) {
	s, ok := ref.(string)
	if !ok {
		return
	}
	for _, sec := range sharedSections {
		if name := strings.TrimPrefix(s, sec.prefix); name != s {
			set.names[sec.key][name] = true
			return
		}
	}
}
