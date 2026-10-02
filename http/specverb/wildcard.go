// Wildcard resource expansion: a `"*"` grant applies its verb across every spec
// resource exposing it, expanded per resource. See docs/specverb-resolution.md.

package specverb

import (
	"sort"
	"strings"

	"github.com/coilyco/umbra/http/guardfile"
)

// wildcardResource is the grant-resource sentinel meaning "every resource in the
// spec that exposes this verb". Authored as `can <verb> "*"`.
const wildcardResource = "*"

// hasWildcard reports whether any grant uses the `"*"` resource sentinel, the
// cheap guard that lets a wildcard-free Guardfile skip expansion entirely.
func hasWildcard(gf *guardfile.Guardfile) bool {
	for _, g := range gf.Grants {
		if g.Resource == wildcardResource {
			return true
		}
	}
	return false
}

// isDenyModal reports whether a modal denies (cannot/never) rather than grants.
func isDenyModal(modal string) bool {
	return modal == "cannot" || modal == "never"
}

// expandWildcards returns gf with every `"*"` grant replaced by a concrete grant
// per spec resource exposing the verb. See docs/specverb-resolution.md.
func expandWildcards(spec *spec, gf *guardfile.Guardfile) (*guardfile.Guardfile, error) {
	if !hasWildcard(gf) {
		return gf, nil
	}
	// An explicit grant pins its (deny?, verb, resource): a wildcard skips it so an
	// authored op/message override stands and no duplicate leaf mounts.
	type placement struct {
		deny     bool
		verb     string
		resource string
	}
	explicit := map[placement]bool{}
	for _, g := range gf.Grants {
		if g.Resource != wildcardResource {
			explicit[placement{isDenyModal(g.Modal), g.Verb, g.Resource}] = true
		}
	}
	out := *gf
	out.Grants = make([]guardfile.Grant, 0, len(gf.Grants))
	emitted := map[placement]bool{}
	for _, g := range gf.Grants {
		if g.Resource != wildcardResource {
			out.Grants = append(out.Grants, g)
			continue
		}
		resources, err := wildcardResources(spec, g.Verb)
		if err != nil {
			return nil, err
		}
		for _, r := range resources {
			k := placement{isDenyModal(g.Modal), g.Verb, r}
			if explicit[k] || emitted[k] {
				continue // an explicit grant (or an earlier wildcard) already owns it
			}
			emitted[k] = true
			cg := g
			cg.Resource = r // concrete placement; Wildcard stays set so help reads "*"
			out.Grants = append(out.Grants, cg)
		}
	}
	return &out, nil
}

// wildcardResources returns every distinct resource the spec exposes for verb under
// its built-in convention, sorted; only ones that resolve unambiguously to one op.
func wildcardResources(spec *spec, verb string) ([]string, error) {
	conv, ok := verbConventions[verb]
	if !ok {
		return nil, errResolve("wildcard grant: verb %q has no built-in convention, so `*` cannot enumerate its resources; use a built-in verb (%s) or name the resource",
			verb, knownConventionVerbs())
	}
	seen := map[string]bool{}
	var names []string
	for path := range spec.ops {
		name, ok := resourceNameFromPath(path, conv.shape)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		if _, err := resolveOp(spec, guardfile.Grant{Verb: verb, Resource: name}); err != nil {
			continue // unresolvable/ambiguous by convention: skip, like an unlisted op
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errResolve("wildcard grant: no resource in the spec exposes verb %q", verb)
	}
	sort.Strings(names)
	return names, nil
}

// resourceNameFromPath returns the bare singular resource name a path exposes under
// shape, so a wildcard expansion string-matches a hand-written `can <verb> <leaf>`.
func resourceNameFromPath(path string, shape resolveShape) (string, bool) {
	seg, _, ok := resourceSegment(path, shape)
	if !ok {
		return "", false
	}
	return singularize(seg), true
}

// knownConventionVerbs lists the built-in convention verbs in sorted order, for the
// fail-closed message a wildcard over an unknown verb prints.
func knownConventionVerbs() string {
	verbs := make([]string, 0, len(verbConventions))
	for v := range verbConventions {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	return strings.Join(verbs, ", ")
}
