package opcore

import (
	"fmt"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
)

// OwnerRepoArg is the documented `args` sugar: a single `owner/name` value
// split across the leaf's `owner` and `repo` path params.
const OwnerRepoArg = "owner-repo"

// pathParamRe pulls `{name}` tokens out of a path template.
var pathParamRe = regexp.MustCompile(`\{([^}]+)\}`)

// FillPath substitutes the i-th positional value into the i-th `{...}` slot.
// The caller has already enforced len(values) == number of slots.
func FillPath(template string, values []string) string {
	i := 0
	return pathParamRe.ReplaceAllStringFunc(template, func(string) string {
		if i >= len(values) {
			return ""
		}
		v := values[i]
		i++
		return v
	})
}

// ArgBinder routes one poll arg onto a path param (by name or owner-repo sugar),
// a query param, or a body field, keeping leaf-request assembly flat.
type ArgBinder struct {
	pathIdx    map[string]int
	PathVals   []string
	queryNames map[string]string
	flagNames  map[string]bool
	fields     map[string]Field
	Query      neturl.Values
	BodyObj    map[string]any
}

// NewArgBinder builds a binder over one leaf's path/query/body flag surface.
func NewArgBinder(leaf Descriptor) *ArgBinder {
	b := &ArgBinder{
		pathIdx:    map[string]int{},
		PathVals:   make([]string, len(leaf.PathParams)),
		queryNames: map[string]string{},
		flagNames:  map[string]bool{},
		fields:     map[string]Field{},
		Query:      neturl.Values{},
		BodyObj:    map[string]any{},
	}
	for i, p := range leaf.PathParams {
		b.pathIdx[p] = i
	}
	for _, f := range leaf.QueryFlags {
		b.queryNames[f.Name] = f.QueryName()
	}
	bodyInputs := append(append([]Field{}, leaf.BodyFlags...), bodyMappingFields(leaf.BodyMappings)...)
	for _, f := range append(append([]Field{}, leaf.QueryFlags...), append(bodyInputs, leaf.FormFlags...)...) {
		b.flagNames[f.Name] = true
		b.fields[f.Name] = f
	}
	return b
}

// Bind routes one resolved arg value; the caller has already proved every arg
// targets something, so an unmatched name here is a no-op, not an error.
func (b *ArgBinder) Bind(name, val string) error {
	switch {
	case name == OwnerRepoArg:
		owner, repo, ok := splitOwnerRepo(val)
		if !ok {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("arg %q value %q is not owner/name", OwnerRepoArg, val), "pass it as owner/name")
		}
		b.PathVals[b.pathIdx["owner"]] = owner
		b.PathVals[b.pathIdx["repo"]] = repo
	case hasKey(b.pathIdx, name):
		b.PathVals[b.pathIdx[name]] = val
	case b.queryNames[name] != "":
		b.Query.Set(b.queryNames[name], val)
	case b.flagNames[name]:
		coerced, err := CoerceScalar(b.fields[name].Type, val)
		if err != nil {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("arg %q: %w", name, err), "pass a value matching the type the upstream declares")
		}
		b.BodyObj[name] = coerced
	}
	return nil
}

// CoerceScalar lowers one CLI token to the JSON type a body field declares, the
// scalar sibling of CoerceItems. Unknown or empty types stay strings. #328.
func CoerceScalar(fieldType, token string) (any, error) {
	switch fieldType {
	case "integer":
		n, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("value %q is not an integer", token)
		}
		return n, nil
	case "number":
		f, err := strconv.ParseFloat(token, 64)
		if err != nil {
			return nil, fmt.Errorf("value %q is not a number", token)
		}
		return f, nil
	case "boolean":
		v, err := strconv.ParseBool(token)
		if err != nil {
			return nil, fmt.Errorf("value %q is not a boolean", token)
		}
		return v, nil
	default:
		return token, nil
	}
}

// ItemsAny marks an array whose swagger `items` schema is empty: the spec says
// the list carries more than one type. Forgejo labels. agentic-os#1047
const ItemsAny = "any"

// FieldFor returns the leaf's declared query/body/form field of that name.
func (b *ArgBinder) FieldFor(name string) (Field, bool) {
	f, ok := b.fields[name]
	return f, ok
}

// BindSlice routes one resolved arg whose value is a list. A query target
// repeats the parameter; a body or form target lands as a JSON array coerced to
// the element type the leaf's own schema declares, so an action shadow encodes
// exactly what the leaf it shadows would.
func (b *ArgBinder) BindSlice(name string, vals []string) error {
	switch {
	case name == OwnerRepoArg || hasKey(b.pathIdx, name):
		return exitcode.New(exitcode.UserError, "user_error",
			fmt.Errorf("arg %q binds a list to a path parameter", name),
			"a path parameter takes one value; drop `array` from the input it references")
	case b.queryNames[name] != "":
		q := b.queryNames[name]
		b.Query.Del(q)
		for _, v := range vals {
			b.Query.Add(q, v)
		}
	case b.flagNames[name]:
		coerced, err := CoerceItems(b.fields[name].Items, vals)
		if err != nil {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("arg %q: %w", name, err), "pass values matching the element type the upstream declares")
		}
		b.BodyObj[name] = coerced
	}
	return nil
}

// CoerceItems lowers CLI tokens to the JSON element type an array field
// declares. An empty `items` schema (ItemsAny) resolves each token
// independently, because that is the only encoding a both-types spec allows.
func CoerceItems(items string, tokens []string) ([]any, error) {
	out := make([]any, 0, len(tokens))
	for _, t := range tokens {
		switch items {
		case "integer":
			n, err := strconv.ParseInt(t, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("value %q is not an integer", t)
			}
			out = append(out, n)
		case "number":
			f, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return nil, fmt.Errorf("value %q is not a number", t)
			}
			out = append(out, f)
		case "boolean":
			v, err := strconv.ParseBool(t)
			if err != nil {
				return nil, fmt.Errorf("value %q is not a boolean", t)
			}
			out = append(out, v)
		case ItemsAny:
			out = append(out, AnyItem(t))
		default:
			out = append(out, t)
		}
	}
	return out, nil
}

// AnyItem lowers one token of an empty-`items` array: all digits becomes a JSON
// number, anything else stays a string.
func AnyItem(token string) any {
	if token == "" {
		return token
	}
	for _, r := range token {
		if r < '0' || r > '9' {
			return token
		}
	}
	parsed, err := strconv.ParseInt(token, 10, 64)
	if err != nil {
		return token
	}
	return parsed
}

// RequireAllPaths fails closed if any path param went unbound, so a poll tick
// never fires an under-bound URL.
func (b *ArgBinder) RequireAllPaths() error {
	for p, i := range b.pathIdx {
		if b.PathVals[i] == "" {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("path param %q was not bound by any arg", p), "add an `args { ... }` binding for it")
		}
	}
	return nil
}

// splitOwnerRepo splits an `owner/name` value into its two halves; ok=false
// unless there are exactly two non-empty parts.
func splitOwnerRepo(v string) (owner, repo string, ok bool) {
	parts := strings.Split(v, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// hasKey reports whether m contains key.
func hasKey(m map[string]int, key string) bool {
	_, ok := m[key]
	return ok
}
