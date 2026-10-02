// Spec-lock pruning for OpenAPI 3.x: reduce a full document (JSON or YAML) to
// the operations a Guardfile grants + their reachable components. Emits JSON.

package specverb

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/getkin/kin-openapi/openapi3"
)

// componentRefPrefix is the JSON-pointer root every OpenAPI 3.x component $ref
// shares, e.g. #/components/schemas/Card or #/components/parameters/idBoard.
const componentRefPrefix = "#/components/"

// pruneOpenAPI3 keeps only the granted paths/methods and the transitive closure
// of components they reach, re-emitting canonical JSON regardless of input format.
func pruneOpenAPI3(spec []byte, gf *guardfile.Guardfile) ([]byte, error) {
	typed, err := parseOpenAPI3(spec)
	if err != nil {
		return nil, err
	}
	keep, err := grantedPathMethods(typed, gf)
	if err != nil {
		return nil, err
	}
	doc, err := openapi3.NewLoader().LoadFromData(spec)
	if err != nil {
		return nil, fmt.Errorf("specverb: prune: load openapi 3.x: %w", err)
	}
	full, err := doc.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("specverb: prune: marshal full doc: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(full, &root); err != nil {
		return nil, fmt.Errorf("specverb: prune: re-read doc json: %w", err)
	}
	rawPaths, _ := root["paths"].(map[string]any)

	newPaths := map[string]any{}
	seedRefs := map[string]bool{}
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
			collectComponentRefs(opObj, seedRefs)
		}
		if params, ok := pathObj["parameters"]; ok {
			kept["parameters"] = params
			collectComponentRefs(params, seedRefs)
		}
		newPaths[path] = kept
	}
	root["paths"] = newPaths
	root["components"] = closeComponents(root["components"], seedRefs)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("specverb: prune: marshal pruned spec: %w", err)
	}
	return append(out, '\n'), nil
}

// closeComponents returns the transitive closure of seed component refs over the
// document's components, regrouped by bucket (schemas, parameters, ...).
func closeComponents(rawComponents any, seed map[string]bool) map[string]any {
	components, _ := rawComponents.(map[string]any)
	closed := map[string]map[string]any{} // bucket -> name -> object
	queue := make([]string, 0, len(seed))
	for ref := range seed {
		queue = append(queue, ref)
	}
	sort.Strings(queue)
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		bucket, name, ok := splitComponentRef(ref)
		if !ok {
			continue
		}
		if closed[bucket] != nil {
			if _, done := closed[bucket][name]; done {
				continue
			}
		}
		obj := lookupComponent(components, bucket, name)
		if obj == nil {
			continue // a $ref to a missing component: the engine fails closed later
		}
		if closed[bucket] == nil {
			closed[bucket] = map[string]any{}
		}
		closed[bucket][name] = obj
		more := map[string]bool{}
		collectComponentRefs(obj, more)
		for r := range more {
			queue = append(queue, r)
		}
	}
	out := map[string]any{}
	for bucket, names := range closed {
		out[bucket] = names
	}
	return out
}

// collectComponentRefs walks a decoded JSON value, adding every
// `#/components/<bucket>/<name>` pointer it finds to set.
func collectComponentRefs(v any, set map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "$ref" {
				if s, ok := val.(string); ok && strings.HasPrefix(s, componentRefPrefix) {
					set[s] = true
				}
			}
			collectComponentRefs(val, set)
		}
	case []any:
		for _, e := range t {
			collectComponentRefs(e, set)
		}
	}
}

// splitComponentRef breaks `#/components/schemas/Card` into ("schemas", "Card").
func splitComponentRef(ref string) (bucket, name string, ok bool) {
	rest := strings.TrimPrefix(ref, componentRefPrefix)
	if rest == ref {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// lookupComponent resolves components[bucket][name] from the decoded doc.
func lookupComponent(components map[string]any, bucket, name string) any {
	b, ok := components[bucket].(map[string]any)
	if !ok {
		return nil
	}
	return b[name]
}
