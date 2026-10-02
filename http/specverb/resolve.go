// Convention-based operation resolution: the spec-agnostic bridge from a grant's
// verb+resource to a spec operation. See docs/specverb-resolution.md.

package specverb

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/coilyco/umbra/http/guardfile"
)

// errResolve formats a fail-closed resolution error with the package prefix.
func errResolve(format string, args ...any) error {
	return fmt.Errorf("specverb: "+format, args...)
}

// resolveShape distinguishes an item leaf (path ends in a {param} run, e.g.
// /repos/{owner}/{repo}) from a collection leaf (ends in a static segment).
type resolveShape int

const (
	shapeItem resolveShape = iota
	shapeCollection
)

// verbConvention maps a verb to the HTTP method(s) and path shape it names. The
// methods are tried in order so a unique earlier-method match wins (edit: PATCH/PUT).
type verbConvention struct {
	methods []string
	shape   resolveShape
}

// verbConventions is the closed set of fixed verb->method mappings. CRUD, the
// reversible state toggles (edit-shaped), and the collection-membership verbs.
var verbConventions = map[string]verbConvention{
	"get":    {methods: []string{"GET"}, shape: shapeItem},
	"view":   {methods: []string{"GET"}, shape: shapeItem},
	"list":   {methods: []string{"GET"}, shape: shapeCollection},
	"create": {methods: []string{"POST"}, shape: shapeCollection},
	"edit":   {methods: []string{"PATCH", "PUT"}, shape: shapeItem},
	"delete": {methods: []string{"DELETE"}, shape: shapeItem},
	// state toggles: PATCH/PUT the item, carrying a fixed `body`.
	"close":     {methods: []string{"PATCH", "PUT"}, shape: shapeItem},
	"reopen":    {methods: []string{"PATCH", "PUT"}, shape: shapeItem},
	"archive":   {methods: []string{"PATCH", "PUT"}, shape: shapeItem},
	"unarchive": {methods: []string{"PATCH", "PUT"}, shape: shapeItem},
	// collection-membership: attach/replace/detach an element of a sub-collection.
	"add":    {methods: []string{"POST"}, shape: shapeCollection},
	"set":    {methods: []string{"PUT"}, shape: shapeCollection},
	"remove": {methods: []string{"DELETE"}, shape: shapeItem},
}

// candidate is one operation that matches a plan, carrying the disambiguation keys.
type candidate struct {
	op     string
	depth  int  // path segment count; the least-nested path is the canonical one
	plural bool // resource segment is a true (plural) collection, not a singleton
	exact  bool // operationId words set-equal the grant's verb+resource (fallback only)
}

// resolvePlan is the lowered form of a verb+resource: the method set, path shape,
// the leaf resource name, its ancestor collections, and the search-suffix flag.
type resolvePlan struct {
	methods   []string
	shape     resolveShape
	leaf      string
	ancestors []string
	search    bool // GET <ancestors...>/<leaf-collection>/search
}

// opAddr addresses one spec operation: an operationId, or a method+path pair
// for a document whose operations declare none. See docs/specverb-resolution.md.
type opAddr struct {
	id     string
	method string
	path   string
}

// String renders the address for a fail-closed message, never a resolved value.
func (a opAddr) String() string {
	if a.id != "" {
		return strconv.Quote(a.id)
	}
	return a.method + " " + a.path
}

// resolveOp returns the address a grant authorizes. An explicit method+path or
// operationId wins; otherwise verb+resource resolve by convention.
func resolveOp(spec *spec, g guardfile.Grant) (opAddr, error) {
	if g.OpMethod != "" || g.OpPath != "" {
		return opAddr{method: g.OpMethod, path: g.OpPath}, nil
	}
	if g.Op != "" {
		return opAddr{id: g.Op}, nil
	}
	plan := classify(g.Verb, g.Resource)
	if plan.search {
		// A structural `<resource>/search` match decides; only a total miss (a spec
		// whose search endpoint carries no resource segment) drops to the fallback.
		if cands := searchCandidates(spec, plan); len(cands) > 0 {
			op, ok, amb := pick(cands)
			return resolveResult(g, op, ok, amb)
		}
	} else {
		// Try each method in order; the first method with any candidate decides, so a
		// unique PATCH wins over a PUT and a method's ambiguity does not fall through.
		for _, method := range plan.methods {
			cands := gather(spec, method, plan)
			if len(cands) == 0 {
				continue
			}
			op, ok, amb := pick(cands)
			return resolveResult(g, op, ok, amb)
		}
	}
	// Fallback: when no path-structure candidate matches, match verb+resource
	// against the words of each operationId (e.g. getPolicyFile for `get policy`).
	op, ok, amb := pick(operationIDCandidates(spec, g.Verb, g.Resource))
	return resolveResult(g, op, ok, amb)
}

// idTokenRe inserts a split point between a lower/digit and an uppercase letter,
// so camelCase operationIds tokenize on word boundaries.
var idTokenRe = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// idTokens lowercases an operationId and splits it into words on case boundaries
// and `-`/`_` separators: getPolicyFile -> [get policy file].
func idTokens(id string) []string {
	s := strings.NewReplacer("-", " ", "_", " ").Replace(id)
	return strings.Fields(strings.ToLower(idTokenRe.ReplaceAllString(s, "$1 $2")))
}

// operationIDCandidates returns ops whose operationId words contain every verb word
// (`ai-search` -> [ai search]) and the resource; exact beats superset (keepExact).
func operationIDCandidates(spec *spec, verb, resource string) []candidate {
	verbWords := idTokens(verb)
	want := singularize(resource)
	grant := wordSet(append(singularizeAll(verbWords), want)...)
	var out []candidate
	for path, methods := range spec.ops {
		for _, op := range methods {
			if op.operationID == "" {
				continue
			}
			toks := idTokens(op.operationID)
			if tokensHaveAll(toks, verbWords) && tokensHaveSingular(toks, want) {
				out = append(out, candidate{
					op:     op.operationID,
					depth:  len(splitPath(path)),
					plural: true,
					exact:  sameSet(singularTokenSet(toks), grant),
				})
			}
		}
	}
	return keepExact(out)
}

// wordSet builds a set from singular words, for exact grant/operationId comparison.
func wordSet(words ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(words))
	for _, w := range words {
		set[w] = struct{}{}
	}
	return set
}

// tokensHaveAll reports whether every word appears verbatim among toks.
func tokensHaveAll(toks, words []string) bool {
	for _, w := range words {
		if !tokensHave(toks, w) {
			return false
		}
	}
	return true
}

// singularTokenSet is the set of an operationId's singularized tokens, so an
// exact word-set match ignores plural/singular and word-order differences.
func singularTokenSet(toks []string) map[string]struct{} {
	set := make(map[string]struct{}, len(toks))
	for _, t := range toks {
		set[singularize(t)] = struct{}{}
	}
	return set
}

// sameSet reports whether two word sets hold the same members.
func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// keepExact drops superset matches when any operationId is set-equal to the grant;
// a remaining tie among exact matches still falls through to pick's fail-closed rule.
func keepExact(cands []candidate) []candidate {
	hasExact := false
	for _, c := range cands {
		if c.exact {
			hasExact = true
			break
		}
	}
	if !hasExact {
		return cands
	}
	out := cands[:0:0]
	for _, c := range cands {
		if c.exact {
			out = append(out, c)
		}
	}
	return out
}

// tokensHave reports whether word appears verbatim among toks.
func tokensHave(toks []string, word string) bool {
	for _, t := range toks {
		if t == word {
			return true
		}
	}
	return false
}

// tokensHaveSingular reports whether any token, singularized, equals want.
func tokensHaveSingular(toks []string, want string) bool {
	for _, t := range toks {
		if singularize(t) == want {
			return true
		}
	}
	return false
}

// resolveResult turns a pick into either the operationId or a fail-closed error
// that teaches the author to pin an `op`.
func resolveResult(g guardfile.Grant, op string, ok bool, ambiguous []string) (opAddr, error) {
	if ok {
		return opAddr{id: op}, nil
	}
	if len(ambiguous) > 1 {
		sort.Strings(ambiguous)
		return opAddr{}, errResolve("cannot resolve %q %q: %d operations match (%s); add `op \"<operationId>\"` to pin one",
			g.Verb, g.Resource, len(ambiguous), strings.Join(ambiguous, ", "))
	}
	return opAddr{}, errResolve("cannot resolve %q %q: no operation matches by convention; add `op \"<operationId>\"`, "+
		"or `op method=\"GET\" path=\"/x/{id}\"` when the spec declares no operationIds",
		g.Verb, g.Resource)
}

// classify lowers a verb+resource into a resolvePlan, dispatching the verb
// through CRUD, the list-/create-on- prefixes, then sub-collection-create.
func classify(verb, resource string) resolvePlan {
	parts := strings.Split(resource, "-")
	leaf := singularize(parts[len(parts)-1])
	ancestors := singularizeAll(parts[:len(parts)-1])
	switch {
	case verb == "search":
		return resolvePlan{methods: []string{"GET"}, leaf: leaf, ancestors: ancestors, search: true}
	case strings.HasPrefix(verb, "list-"):
		child := singularize(strings.TrimPrefix(verb, "list-"))
		return resolvePlan{methods: []string{"GET"}, shape: shapeCollection, leaf: child, ancestors: append(ancestors, leaf)}
	case strings.HasPrefix(verb, "create-on-"):
		parent := singularize(strings.TrimPrefix(verb, "create-on-"))
		return resolvePlan{methods: []string{"POST"}, shape: shapeCollection, leaf: leaf, ancestors: append([]string{parent}, ancestors...)}
	}
	if conv, ok := verbConventions[verb]; ok {
		return resolvePlan{methods: conv.methods, shape: conv.shape, leaf: leaf, ancestors: ancestors}
	}
	// Unknown verb: treat its trailing noun as a child sub-collection to create on
	// the resource (e.g. `comment issue` -> POST .../issues/{i}/comments).
	vparts := strings.Split(verb, "-")
	child := singularize(vparts[len(vparts)-1])
	return resolvePlan{methods: []string{"POST"}, shape: shapeCollection, leaf: child, ancestors: append(ancestors, leaf)}
}

// gather returns the candidates whose method, path resource segment, and ancestor
// chain match the plan.
func gather(spec *spec, method string, plan resolvePlan) []candidate {
	var out []candidate
	for path, methods := range spec.ops {
		op, ok := methods[strings.ToLower(method)]
		if !ok || op.operationID == "" {
			continue
		}
		seg, idx, ok := resourceSegment(path, plan.shape)
		if !ok || singularize(seg) != plan.leaf {
			continue
		}
		segs := splitPath(path)
		if !ancestorsMatch(segs[:idx], plan.ancestors) {
			continue
		}
		out = append(out, candidate{op: op.operationID, depth: len(segs), plural: isPlural(seg)})
	}
	return out
}

// searchCandidates matches the search-suffix shape: GET a path ending in
// `<leaf-collection>/search`, with the plan's ancestors appearing before it.
func searchCandidates(spec *spec, plan resolvePlan) []candidate {
	var out []candidate
	for path, methods := range spec.ops {
		op, ok := methods["get"]
		if !ok || op.operationID == "" {
			continue
		}
		segs := splitPath(path)
		n := len(segs)
		if n < 2 || segs[n-1] != "search" || isParam(segs[n-2]) || singularize(segs[n-2]) != plan.leaf {
			continue
		}
		if !ancestorsMatch(segs[:n-2], plan.ancestors) {
			continue
		}
		out = append(out, candidate{op: op.operationID, depth: n, plural: true})
	}
	return out
}

// pick selects the winning candidate: prefer true (plural) collections over
// singletons, then the least-nested path; a remaining tie fails closed.
func pick(cands []candidate) (op string, ok bool, ambiguous []string) {
	if len(cands) == 0 {
		return "", false, nil
	}
	if hasPlural(cands) {
		cands = keepPlural(cands)
	}
	cands = keepShallowest(cands)
	if len(cands) == 1 {
		return cands[0].op, true, nil
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.op)
	}
	return "", false, ids
}

// hasPlural reports whether any candidate's resource segment is a true collection.
func hasPlural(cands []candidate) bool {
	for _, c := range cands {
		if c.plural {
			return true
		}
	}
	return false
}

// keepPlural drops singleton-segment candidates when a true collection exists.
func keepPlural(cands []candidate) []candidate {
	var out []candidate
	for _, c := range cands {
		if c.plural {
			out = append(out, c)
		}
	}
	return out
}

// keepShallowest keeps only the least-nested candidates (the canonical resource
// lives at the shallowest path; deeper paths are nested views of it).
func keepShallowest(cands []candidate) []candidate {
	shallow := cands[0].depth
	for _, c := range cands {
		if c.depth < shallow {
			shallow = c.depth
		}
	}
	var out []candidate
	for _, c := range cands {
		if c.depth == shallow {
			out = append(out, c)
		}
	}
	return out
}

// resourceSegment returns the static segment naming the resource and its index:
// the trailing static (collection), or the last static before the {param} run (item).
func resourceSegment(path string, shape resolveShape) (string, int, bool) {
	segs := splitPath(path)
	if len(segs) == 0 {
		return "", 0, false
	}
	switch shape {
	case shapeCollection:
		if last := len(segs) - 1; !isParam(segs[last]) {
			return segs[last], last, true
		}
		return "", 0, false
	case shapeItem:
		i := len(segs) - 1
		if !isParam(segs[i]) {
			return "", 0, false // an item path ends in {param}
		}
		for i >= 0 && isParam(segs[i]) {
			i-- // walk back over the trailing {param} run to the naming segment
		}
		if i < 0 {
			return "", 0, false
		}
		return segs[i], i, true
	}
	return "", 0, false
}

// ancestorsMatch reports whether each ancestor (singular) appears as a static
// segment of segs, in order; nested resources must sit under their parents.
func ancestorsMatch(segs []string, ancestors []string) bool {
	i := 0
	for _, a := range ancestors {
		found := false
		for i < len(segs) {
			seg := segs[i]
			i++
			if !isParam(seg) && singularize(seg) == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// splitPath splits a URL path template into its non-empty segments.
func splitPath(path string) []string {
	var out []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isParam reports whether a path segment is a `{name}` template parameter.
func isParam(seg string) bool {
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")
}

// isPlural reports whether a path segment is a plural collection (`repos`), not a
// singular singleton (`acl`, the device `key`).
func isPlural(seg string) bool {
	s := strings.ToLower(seg)
	return strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss")
}

// singularize lowercases and strips a trailing plural "s" (not "ss"), matching a
// path segment (`repos`) to a grant's singular resource (`repo`).
func singularize(s string) string {
	s = strings.ToLower(s)
	if strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss") {
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// singularizeAll singularizes each element of parts.
func singularizeAll(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, singularize(p))
	}
	return out
}
