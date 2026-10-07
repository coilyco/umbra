// Package guardfile parses a KDL Guardfile, the human authoring layer (L2) of
// the specverb engine, into a typed model. KDL is parsed, never evaluated.
package guardfile

import (
	"fmt"
	"strings"

	kdl "github.com/calico32/kdl-go"
)

// ValueSource names where a config value is read at request time: a Provider
// (ssm, tailscale, env, ...) and the Address it interprets. See specverb-policy.md.
type ValueSource struct {
	Provider string
	Address  string
}

// IsZero reports whether the source is unset (no provider named).
func (v ValueSource) IsZero() bool { return v.Provider == "" }

// ValueChain is an ordered fallback list of value sources: resolution takes the
// first yielding a non-empty value. See value-providers.md.
type ValueChain []ValueSource

// IsZero reports whether the chain names no source.
func (c ValueChain) IsZero() bool { return len(c) == 0 }

// String renders the chain as `provider address` sources joined by " | " (the
// symbolic form describe/--dry-run show; "" when zero, never a resolved value).
func (c ValueChain) String() string {
	parts := make([]string, 0, len(c))
	for _, vs := range c {
		parts = append(parts, vs.Provider+" "+vs.Address)
	}
	return strings.Join(parts, " | ")
}

// AuthSchemeNone marks a deliberately credential-free upstream. Distinct from
// an unset scheme, which is a spec that forgot. See docs/specverb-policy.md.
const AuthSchemeNone = "none"

// Auth describes how the engine authenticates to the target API. Four schemes:
// header-token, bearer, query-param (dual-secret), none. See docs/specverb.md.
type Auth struct {
	Scheme string
	Header string
	Prefix string // trailing space is significant, e.g. "token "
	Value  ValueChain

	// Params are the query-param scheme's ordered secrets, each injected as a
	// query parameter (Trello's ?key=&token=). Empty for the header schemes.
	Params []QueryAuthParam
}

// QueryAuthParam is one secret of the query-param scheme: a query parameter Name
// whose value is read from the named value source.
type QueryAuthParam struct {
	Name  string
	Value ValueChain
}

// Grant is one policy sentence: modal verb resource [qualifiers...] [key=value...].
// Resource is the CLI group, Verb the leaf, Op the operationId. See docs/specverb.md.
type Grant struct {
	Modal      string
	Verb       string
	Resource   string
	Qualifiers []string

	// Props are KDL key=value node properties: structured scoping constraints
	// like `org=acme`. Positional bareword qualifiers stay in Qualifiers.
	Props map[string]string

	// Op is the spec operationId (grant-body `op "..."`). Optional: an empty Op is
	// resolved by convention (specverb.resolveOp); set it to override. Deny ignores it.
	Op string

	// OpMethod and OpPath address an operation by method+path, for a spec whose
	// operations carry no operationId. See docs/specverb-resolution.md.
	OpMethod string
	OpPath   string

	// FixedBody is the grant-body `body key=value...` map: a state-toggle leaf that
	// always sends this exact JSON and mounts no body flags. Keeps KDL-native types.
	FixedBody map[string]any

	// Message is the grant-body `message "..."` shown when a deny blocks an
	// invocation - the teaching error. Only meaningful on cannot/never.
	Message string

	// Describe is the optional grant-body `describe "..."` note that enriches
	// the thin upstream spec; it flows into help and the describe verb.
	Describe string

	// Wildcard is set when the resource was authored as the `"*"` sentinel: the
	// engine expands it to one concrete grant per spec resource exposing the verb.
	Wildcard bool

	// Override is set for an `override can <verb> <resource>` grant: the sole
	// construct that crosses an inherited `never`. It carries Modal "can".
	Override bool
}

// Restriction is a wrap-level `restrict <param> matches "<glob>"...` allowlist:
// a {Param}-carrying leaf must match a Glob or fail closed. See docs/specverb.md.
type Restriction struct {
	Param string
	Globs []string
}

// Input is one parameter an Action declares (a positional arg or a flag). Its
// name doubles as the JMESPath `$name` variable in `until`/`fail-when`.
type Input struct {
	Name       string
	Positional bool   // true: a positional arg; false: a --flag
	Required   bool   // enforced at invocation
	Help       string // one-line help, "" if none
	// Default is a JMESPath pre-flight binding for an absent input (poll actions
	// only); mutually exclusive with Required. See specverb-actions.md.
	Default string
	// Array makes the input repeatable, projected as a JSON array coerced to the
	// element type the bound leaf field declares. Flags only. See specverb-actions.md.
	Array bool
	// Matches constrains the bound value: on an `array` each entry demands at
	// least one matching element. See specverb-actions.md.
	Matches []InputMatch
}

// InputMatch is one `matches "<glob...>" message="<why>"` constraint on an Input.
// Globs are alternatives; Message names what is missing, "" taking a generated one.
type InputMatch struct {
	Globs   []string
	Message string
}

// ArgBind is one `args { <name> <value> }` binding for the polled leaf. A
// `$input` value references an Input; else it is a literal. See specverb-actions.md.
type ArgBind struct {
	Name  string
	Value string // `$input` reference or a literal
}

// Poll re-fires a granted leaf (Verb+Resource) until Until settles or Timeout
// elapses, sampling Every; the last response binds to As. See specverb-actions.md.
type Poll struct {
	Verb     string
	Resource string
	Args     []ArgBind
	Until    string // JMESPath; truthy ends the loop
	Every    string // sample interval, e.g. "10s" (quoted: KDL rejects bare 10s)
	Timeout  string // wall-clock bound, e.g. "30m"
	As       string // binding name for the final response
}

// Call is one step of a multi-call action: fires a granted leaf with Args, binds
// the response to As for `$As.field` data-flow. See specverb-actions.md.
type Call struct {
	Verb     string
	Resource string
	Args     []ArgBind
	As       string // binding name for this call's response
}

// Collect walks a paginated granted leaf, incrementing PageParam and appending
// each array response until the page returns fewer than Limit (specverb-actions.md).
type Collect struct {
	Verb         string
	Resource     string
	Args         []ArgBind
	PageParam    string
	LimitParam   string
	Limit        string
	DefaultLimit string
	As           string
	// Cache, when set, is the TTL string (e.g. "10m") serving the accumulated
	// array from the on-disk ttl cache. Collect-only (see docs/specverb-actions.md).
	Cache string
}

// Action is a named composite verb: exactly one of Poll, Calls, or Collect, plus
// an optional FailWhen exit. See specverb-actions.md.
type Action struct {
	Name     string
	Describe string
	Inputs   []Input
	Poll     *Poll
	Calls    []Call // ordered multi-call sequence; mutually exclusive with Poll
	Collect  *Collect
	FailWhen string // JMESPath over the bindings; truthy => non-zero exit

	// RejectEmpty refuses an action that answered with nothing. Exec dialect
	// only, refused at build time over http. See docs/execverb-occlusion.md.
	RejectEmpty bool

	// MountVerb/MountResource: the two-arg `action <verb> <resource>` mount form
	// shadows that leaf path. Empty for `action <name>`. See specverb-actions.md.
	MountVerb     string
	MountResource string
}

// IsMount reports whether the action shadows a leaf path (two-arg form).
func (a Action) IsMount() bool { return a.MountResource != "" }

// Fetch is one HTTP overlay leaf: a fixed method/path pair plus env-backed
// headers and simple glob guards on the positional path inputs.
type Fetch struct {
	Name     string
	Leaf     string
	Describe string
	Method   string
	Path     string
	Output   string
	Env      []FetchEnv
	Headers  []FetchHeader
	Whens    []FetchWhen
}

// FetchEnv is one named value source used to fill fetch header templates.
type FetchEnv struct {
	Name  string
	Value ValueChain
}

// FetchHeader is one literal header name plus a template value.
type FetchHeader struct {
	Name  string
	Value string
}

// FetchWhen is one positional guard: the selector and globs it must match.
type FetchWhen struct {
	Selector string
	Globs    []string
}

// Guardfile is the parsed form of one wrap block.
type Guardfile struct {
	// Description is the optional top-level `description "..."` prose (sibling of
	// `wrap`): standing context in describe + the ref doc. See docs/value-providers.md.
	Description string

	Group   []string // command path, e.g. ["ward", "ops", "forgejo"]
	Spec    string
	BaseURL string
	// BaseURLValue resolves the base-url at request time (block form
	// `base-url { value ... }`), exclusive with BaseURL. See specverb-policy.md.
	BaseURLValue ValueChain
	Auth         Auth
	Grants       []Grant
	Restrict     []Restriction
	Actions      []Action
	Fetches      []Fetch

	// AllowMeta names the path params opted out of the shell-metacharacter
	// gate by `allow-metacharacters <param>...`. See docs/specverb-request.md.
	AllowMeta []string

	// Providers are consumer-declared value resolvers: `provider <name> { exec ... }`.
	// umbra ships no store SDK, so a store-backed source is an exec contract.
	ProviderDecls []ProviderDecl
}

// ProviderDecl is one `provider <name> { exec <argv...> }` declaration. The
// address being resolved is appended to Exec as the final argument.
type ProviderDecl struct {
	Name string
	Exec []string
}

// modals is the closed set of grant verbs; anything else fails closed.
var modals = map[string]bool{"can": true, "cannot": true, "never": true}

// overrideNode is the escalation directive's node name: `override can <verb>
// <resource>` re-grants a single denied class by name. See specverb-policy.md.
const overrideNode = "override"

// isDenyModal reports whether a modal denies (cannot/never) rather than grants.
func isDenyModal(modal string) bool { return modal == "cannot" || modal == "never" }

// Parse turns Guardfile source into a Guardfile. It fails closed: an unknown
// node, a missing required field, or a malformed sentence is an error.
func Parse(src []byte) (*Guardfile, error) {
	doc, err := kdl.ParseString(string(src))
	if err != nil {
		return nil, fmt.Errorf("guardfile: parse KDL: %w", err)
	}
	wrap := doc.GetNode("wrap")
	if wrap == nil {
		return nil, fmt.Errorf("guardfile: missing top-level `wrap` node")
	}
	// GetNode returns the first, so a second wrap would drop silently (umbra#8163).
	if n := countWrap(doc); n > 1 {
		return nil, fmt.Errorf("guardfile: %d top-level `wrap` nodes; a guardfile holds one, so split the rest into their own files (fail-closed)", n)
	}
	gf := &Guardfile{}
	if err := gf.applyDescription(doc); err != nil {
		return nil, err
	}
	for _, a := range wrap.Arguments() {
		gf.Group = append(gf.Group, a.String())
	}
	if len(gf.Group) == 0 {
		return nil, fmt.Errorf("guardfile: `wrap` needs a command path, e.g. `wrap ward ops forgejo`")
	}
	for _, n := range wrap.Children().Nodes {
		if n.Name() == inheritNode {
			// inherit is resolved textually by Flatten/ParseFile before this parse;
			// reaching it here means Parse was handed unflattened source.
			return nil, fmt.Errorf("guardfile: `inherit` needs file-path resolution; load with guardfile.ParseFile (fail-closed)")
		}
		if err := gf.applyNode(n); err != nil {
			return nil, err
		}
	}
	return gf, gf.validate()
}

// applyDescription reads the optional top-level `description "..."` node (a
// sibling of `wrap`), fail-closing on a bad shape or an empty string.
func (gf *Guardfile) applyDescription(doc *kdl.Document) error {
	n := doc.GetNode(DescriptionNode)
	if n == nil {
		return nil
	}
	v, err := singleArg(n)
	if err != nil {
		return fmt.Errorf("guardfile: `description`: %w", err)
	}
	if v == "" {
		return fmt.Errorf("guardfile: `description` must be a non-empty string (fail-closed)")
	}
	gf.Description = v
	return nil
}

// applyNode dispatches one child of the wrap block onto gf.
func (gf *Guardfile) applyNode(n *kdl.Node) error {
	name := n.Name()
	if isGrantNode(name) {
		g, err := parseGrantOrOverride(n)
		if err != nil {
			return err
		}
		gf.Grants = append(gf.Grants, g)
		return nil
	}
	switch name {
	case "spec":
		v, err := singleArg(n)
		gf.Spec = v
		return err
	case "base-url":
		return gf.applyBaseURL(n)
	case "auth":
		a, err := parseAuth(n)
		gf.Auth = a
		return err
	default:
		return gf.applyListNode(n, name)
	}
}

// applyListNode handles repeatable wrap-body nodes and the fail-closed unknown
// fallback, split off to hold the cyclo cap.
func (gf *Guardfile) applyListNode(n *kdl.Node, name string) error {
	switch name {
	case "restrict":
		r, err := parseRestrict(n)
		if err != nil {
			return err
		}
		gf.Restrict = append(gf.Restrict, r)
		return nil
	case "allow-metacharacters":
		params, err := parseAllowMeta(n)
		if err != nil {
			return err
		}
		gf.AllowMeta = append(gf.AllowMeta, params...)
		return nil
	case "action":
		act, err := parseAction(n)
		if err != nil {
			return err
		}
		gf.Actions = append(gf.Actions, act)
		return nil
	case "fetch":
		fetch, err := parseFetch(n)
		if err != nil {
			return err
		}
		gf.Fetches = append(gf.Fetches, fetch)
		return nil
	case "provider":
		pd, err := parseProvider(n)
		if err != nil {
			return err
		}
		gf.ProviderDecls = append(gf.ProviderDecls, pd)
		return nil
	default:
		return unknownWrapNode(name)
	}
}

// unknownWrapNode is the fail-closed error for a wrap-body node applyNode does
// not handle, distinguishing a reserved-for-future keyword from a plain unknown.
func unknownWrapNode(name string) error {
	if reservedActionKeywords[name] {
		return fmt.Errorf("guardfile: %q is reserved for a future version and is not implemented in v1 (fail-closed)", name)
	}
	return fmt.Errorf("guardfile: unknown node %q in wrap body (fail-closed)", name)
}

// applyBaseURL reads the base-url node onto gf via the shared ParseBaseURL.
func (gf *Guardfile) applyBaseURL(n *kdl.Node) error {
	raw, chain, err := ParseBaseURL(n)
	if err != nil {
		return err
	}
	// Assign only the form this node carried, so a second base-url node in the
	// other form accumulates into both fields and validate catches the conflict.
	if raw != "" {
		gf.BaseURL = raw
	}
	if !chain.IsZero() {
		gf.BaseURLValue = chain
	}
	return nil
}

// ParseBaseURL reads a base-url node: a bare string (raw) or a `{ value ... }`
// block (chain). Exactly one is non-zero. Shared with the opcore inline source.
func ParseBaseURL(n *kdl.Node) (raw string, chain ValueChain, err error) {
	children := n.Children().Nodes
	if len(children) == 0 {
		v, serr := singleArg(n)
		return v, nil, serr
	}
	for _, c := range children {
		if c.Name() != "value" {
			return "", nil, fmt.Errorf("guardfile: base-url: unknown field %q (want value; fail-closed)", c.Name())
		}
		vc, verr := parseValueChain(c)
		if verr != nil {
			return "", nil, fmt.Errorf("guardfile: base-url: %w", verr)
		}
		chain = vc
	}
	if chain.IsZero() {
		return "", nil, fmt.Errorf("guardfile: base-url block requires `value <provider> \"...\"`")
	}
	return "", chain, nil
}

// ParseValueBlock reads a `{ value ... }` block off a node that carries one,
// so a new wrap node can take a credential chain without restating the shape.
func ParseValueBlock(n *kdl.Node, node string) (ValueChain, error) {
	var chain ValueChain
	for _, c := range n.Children().Nodes {
		if c.Name() != "value" {
			return nil, fmt.Errorf("guardfile: %s: unknown field %q (want value; fail-closed)", node, c.Name())
		}
		vc, err := parseValueChain(c)
		if err != nil {
			return nil, fmt.Errorf("guardfile: %s: %w", node, err)
		}
		chain = vc
	}
	if chain.IsZero() {
		return nil, fmt.Errorf("guardfile: %s requires `value <provider> \"...\"`", node)
	}
	return chain, nil
}

// parseValueChain reads a `value` node in either form (inline `value <p> "<a>"`
// or a `{ ... }` fallback block) into a ValueChain. See value-providers.md.
func parseValueChain(n *kdl.Node) (ValueChain, error) {
	_, hasBlock := n.ChildrenInline()
	args := n.Arguments()
	switch {
	case hasBlock && len(args) > 0:
		return nil, fmt.Errorf("value takes either an inline `value <provider> \"<address>\"` or a `{ ... }` fallback block, not both (fail-closed)")
	case hasBlock:
		children := n.Children().Nodes
		if len(children) == 0 {
			return nil, fmt.Errorf("value block is empty; list at least one `<provider> \"<address>\"` fallback source")
		}
		chain := make(ValueChain, 0, len(children))
		for _, c := range children {
			vs, err := valueSourceFromChild(c)
			if err != nil {
				return nil, err
			}
			chain = append(chain, vs)
		}
		return chain, nil
	default:
		vs, err := valueSourceInline(args)
		if err != nil {
			return nil, err
		}
		return ValueChain{vs}, nil
	}
}

// valueSourceInline reads the inline `value <provider> "<address>"` args: exactly
// two, the provider name then the address, both non-empty.
func valueSourceInline(args []kdl.Value) (ValueSource, error) {
	if len(args) != 2 {
		return ValueSource{}, fmt.Errorf("value needs a provider and an address, e.g. `value ssm \"/forgejo/api-token\"` (got %d arg(s))", len(args))
	}
	vs := ValueSource{Provider: args[0].String(), Address: args[1].String()}
	if vs.Provider == "" || vs.Address == "" {
		return ValueSource{}, fmt.Errorf("value needs a non-empty provider and address")
	}
	return vs, nil
}

// valueSourceFromChild reads one `<provider> "<address>"` fallback source inside a
// value block: the node name is the provider, its single argument the address.
func valueSourceFromChild(c *kdl.Node) (ValueSource, error) {
	provider := c.Name()
	args := c.Arguments()
	if len(args) != 1 {
		return ValueSource{}, fmt.Errorf("value source %q needs an address, e.g. `%s \"/forgejo/api-token\"` (got %d arg(s))", provider, provider, len(args))
	}
	if len(c.Children().Nodes) > 0 || len(c.Properties()) > 0 {
		return ValueSource{}, fmt.Errorf("value source %q takes a single address argument, no children or properties (fail-closed)", provider)
	}
	vs := ValueSource{Provider: provider, Address: args[0].String()}
	if vs.Provider == "" || vs.Address == "" {
		return ValueSource{}, fmt.Errorf("value source needs a non-empty provider and address")
	}
	return vs, nil
}

// Providers returns the distinct provider names every value source in gf names,
// so a consumer (or the codegen) can wire exactly the resolvers in use.
func (gf *Guardfile) Providers() []string {
	seen := map[string]bool{}
	var out []string
	add := func(chain ValueChain) {
		for _, vs := range chain {
			if vs.Provider == "" || seen[vs.Provider] {
				continue
			}
			seen[vs.Provider] = true
			out = append(out, vs.Provider)
		}
	}
	add(gf.Auth.Value)
	for _, p := range gf.Auth.Params {
		add(p.Value)
	}
	add(gf.BaseURLValue)
	for _, f := range gf.Fetches {
		for _, e := range f.Env {
			add(e.Value)
		}
	}
	return out
}

// reservedActionKeywords are the forward-design slots v1 does not implement;
// parsing one is a fail-closed error, not a silent no-op. See specverb-actions.md.
var reservedActionKeywords = map[string]bool{
	"read": true,                 // non-poll single-leaf read (the leaf seam)
	"emit": true, "cursor": true, // per-tick streaming-delta slots on poll
	"each": true, "yield": true, // fan-out body (deferred v2)
	"follow": true, "stream": true, "tail": true, // live log-tail keywords
}

// validate enforces the required header fields.
func (gf *Guardfile) validate() error {
	hasSpecDriven := gf.Spec != "" || len(gf.Grants) > 0 || len(gf.Actions) > 0 || len(gf.Restrict) > 0
	if hasSpecDriven {
		if gf.Spec == "" {
			return fmt.Errorf("guardfile: `spec` is required")
		}
		if gf.Auth.Scheme == "" {
			return fmt.Errorf("guardfile: `auth` block is required")
		}
	}
	if gf.BaseURL != "" && !gf.BaseURLValue.IsZero() {
		return fmt.Errorf("guardfile: base-url set both as a string and a `{ value }` block; pick one")
	}
	return gf.validateOverrides()
}

// validateOverrides fails closed on a no-op `override` (one crossing no deny):
// silently it is a plain `can`. See docs/specverb-policy.md.
func (gf *Guardfile) validateOverrides() error {
	for _, g := range gf.Grants {
		if !g.Override {
			continue
		}
		if !gf.denyCovers(g.Verb, g.Resource) {
			return fmt.Errorf("guardfile: `override can %s %s` lifts no `never`/`cannot` (it would be a silent no-op `can`); deny the class first or drop the override",
				g.Verb, g.Resource)
		}
	}
	return nil
}

// denyCovers reports whether some deny grant blocks (verb, resource): a matching
// `never`/`cannot` for the exact resource or for the verb-global `"*"`.
func (gf *Guardfile) denyCovers(verb, resource string) bool {
	for _, d := range gf.Grants {
		if isDenyModal(d.Modal) && d.Verb == verb && (d.Resource == resource || d.Resource == wildcardSentinel) {
			return true
		}
	}
	return false
}

// wildcardSentinel is the verb-global resource sentinel a `*` grant carries.
const wildcardSentinel = "*"

// ParseAuthNode parses one `auth` KDL node into an Auth: the entry the opcore
// inline source shares, so both sources speak one auth grammar.
func ParseAuthNode(n *kdl.Node) (Auth, error) { return parseAuth(n) }

// ParseRestrictNode parses one `restrict` KDL node into a Restriction, shared
// with the opcore inline source so both sources speak one restrict grammar.
func ParseRestrictNode(n *kdl.Node) (Restriction, error) { return parseRestrict(n) }

// ParseAllowMetaNode parses one `allow-metacharacters` KDL node into its param
// names, the entry the inline dialect shares with the guardfile dialect.
func ParseAllowMetaNode(n *kdl.Node) ([]string, error) { return parseAllowMeta(n) }

// parseAuth reads the auth block, dispatching on the named scheme. Four are
// supported: header-token, bearer, query-param, none. See docs/specverb.md.
func parseAuth(n *kdl.Node) (Auth, error) {
	scheme, err := singleArg(n)
	if err != nil {
		return Auth{}, fmt.Errorf("guardfile: auth: %w", err)
	}
	switch scheme {
	case "header-token":
		return parseHeaderTokenAuth(n)
	case "bearer":
		return parseBearerAuth(n)
	case "query-param":
		return parseQueryParamAuth(n)
	case "none":
		return parseNoneAuth(n)
	default:
		return Auth{}, fmt.Errorf("guardfile: auth scheme %q unsupported (want header-token | bearer | query-param | none)", scheme)
	}
}

// parseNoneAuth reads `auth none`, for a genuinely credential-free upstream.
// See docs/specverb-policy.md.
func parseNoneAuth(n *kdl.Node) (Auth, error) {
	if len(n.Children().Nodes) > 0 {
		return Auth{}, fmt.Errorf("guardfile: auth none takes no block (a credential-free upstream resolves nothing)")
	}
	return Auth{Scheme: AuthSchemeNone}, nil
}

// parseHeaderTokenAuth reads `header-token { header H; prefix "..."; value P A }`.
func parseHeaderTokenAuth(n *kdl.Node) (Auth, error) {
	a := Auth{Scheme: "header-token"}
	for _, c := range n.Children().Nodes {
		switch c.Name() {
		case "header":
			v, ferr := singleArg(c)
			if ferr != nil {
				return Auth{}, fmt.Errorf("guardfile: auth header: %w", ferr)
			}
			a.Header = v
		case "prefix":
			v, ferr := singleArg(c)
			if ferr != nil {
				return Auth{}, fmt.Errorf("guardfile: auth prefix: %w", ferr)
			}
			a.Prefix = v
		case "value":
			vc, ferr := parseValueChain(c)
			if ferr != nil {
				return Auth{}, fmt.Errorf("guardfile: auth %w", ferr)
			}
			a.Value = vc
		default:
			return Auth{}, fmt.Errorf("guardfile: auth: unknown field %q (fail-closed)", c.Name())
		}
	}
	if a.Header == "" || a.Value.IsZero() {
		return Auth{}, fmt.Errorf("guardfile: auth header-token requires `header` and `value <provider> \"...\"`")
	}
	return a, nil
}

// parseBearerAuth reads `bearer { value <provider> A }`: shorthand for the
// Authorization header with a "Bearer " prefix (Tailscale).
func parseBearerAuth(n *kdl.Node) (Auth, error) {
	a := Auth{Scheme: "bearer", Header: "Authorization", Prefix: "Bearer "}
	for _, c := range n.Children().Nodes {
		if c.Name() != "value" {
			return Auth{}, fmt.Errorf("guardfile: auth bearer: unknown field %q (want value; fail-closed)", c.Name())
		}
		vc, ferr := parseValueChain(c)
		if ferr != nil {
			return Auth{}, fmt.Errorf("guardfile: auth bearer %w", ferr)
		}
		a.Value = vc
	}
	if a.Value.IsZero() {
		return Auth{}, fmt.Errorf("guardfile: auth bearer requires `value <provider> \"...\"`")
	}
	return a, nil
}

// parseQueryParamAuth reads `query-param { param <name> { value <provider> A } ... }`:
// one or more secrets injected as query parameters (Trello's ?key=&token=).
func parseQueryParamAuth(n *kdl.Node) (Auth, error) {
	a := Auth{Scheme: "query-param"}
	for _, c := range n.Children().Nodes {
		if c.Name() != "param" {
			return Auth{}, fmt.Errorf("guardfile: auth query-param: unknown field %q (want param; fail-closed)", c.Name())
		}
		name, err := singleArg(c)
		if err != nil {
			return Auth{}, fmt.Errorf("guardfile: auth query-param: %w (name it: `param key { value <provider> \"...\" }`)", err)
		}
		p := QueryAuthParam{Name: name}
		for _, cc := range c.Children().Nodes {
			if cc.Name() != "value" {
				return Auth{}, fmt.Errorf("guardfile: auth query-param %s: unknown field %q (want value)", name, cc.Name())
			}
			vc, ferr := parseValueChain(cc)
			if ferr != nil {
				return Auth{}, fmt.Errorf("guardfile: auth query-param %s: %w", name, ferr)
			}
			p.Value = vc
		}
		if p.Value.IsZero() {
			return Auth{}, fmt.Errorf("guardfile: auth query-param %q requires `value <provider> \"...\"`", name)
		}
		a.Params = append(a.Params, p)
	}
	if len(a.Params) == 0 {
		return Auth{}, fmt.Errorf("guardfile: auth query-param requires at least one `param`")
	}
	return a, nil
}

// parseGrant reads one policy sentence: modal verb resource [qualifiers...].
func parseGrant(n *kdl.Node) (Grant, error) {
	args := n.Arguments()
	if len(args) < 2 {
		return Grant{}, fmt.Errorf("guardfile: %q needs a verb and a resource, e.g. `%s create repos`", n.Name(), n.Name())
	}
	g := Grant{Modal: n.Name(), Verb: args[0].String(), Resource: args[1].String()}
	if g.Resource == "*" {
		g.Wildcard = true // verb-global: expanded per resource by the engine
	}
	for _, q := range args[2:] {
		g.Qualifiers = append(g.Qualifiers, q.String())
	}
	for k, v := range n.Properties() {
		if g.Props == nil {
			g.Props = map[string]string{}
		}
		g.Props[k] = v.String()
	}
	for _, c := range n.Children().Nodes {
		if err := applyGrantChild(&g, n.Name(), c); err != nil {
			return Grant{}, err
		}
	}
	return g, nil
}

// parseGrantOrOverride lowers a grant sentence: a modal `can`/`cannot`/`never`,
// or the `override can …` escalation form.
func parseGrantOrOverride(n *kdl.Node) (Grant, error) {
	if n.Name() == overrideNode {
		return parseOverride(n)
	}
	return parseGrant(n)
}

// parseOverride reads `override can <verb> <resource>`: only the `can` form is
// valid and the resource must be named (`*` rejected). See specverb-policy.md.
func parseOverride(n *kdl.Node) (Grant, error) {
	args := n.Arguments()
	if len(args) < 3 || args[0].String() != "can" {
		return Grant{}, fmt.Errorf("guardfile: `override` must read `override can <verb> <resource>` (the only escalation form)")
	}
	g := Grant{Modal: "can", Override: true, Verb: args[1].String(), Resource: args[2].String()}
	if g.Resource == "*" {
		return Grant{}, fmt.Errorf("guardfile: `override can %s \"*\"` is not allowed: an override names one resource so every escalation is reviewable by name", g.Verb)
	}
	for _, q := range args[3:] {
		g.Qualifiers = append(g.Qualifiers, q.String())
	}
	for k, v := range n.Properties() {
		if g.Props == nil {
			g.Props = map[string]string{}
		}
		g.Props[k] = v.String()
	}
	for _, c := range n.Children().Nodes {
		if err := applyGrantChild(&g, n.Name(), c); err != nil {
			return Grant{}, err
		}
	}
	return g, nil
}

// applyGrantChild dispatches one grant-body child onto g. modal is the grant's
// node name, used only to enrich error messages.
func applyGrantChild(g *Grant, modal string, c *kdl.Node) error {
	switch c.Name() {
	case "op":
		if err := applyOpNode(g, c); err != nil {
			return fmt.Errorf("guardfile: grant %q: %w", modal, err)
		}
	case "body":
		// A fixed-body toggle: `body state="closed"` -> always send that JSON.
		// Properties keep their KDL-native type so booleans stay booleans.
		if len(c.Properties()) == 0 {
			return fmt.Errorf("guardfile: grant %q: `body` needs at least one key=value (e.g. `body state=\"closed\"`)", modal)
		}
		g.FixedBody = map[string]any{}
		for k, val := range c.Properties() {
			g.FixedBody[k] = val.RawValue()
		}
	case "message":
		v, err := singleArg(c)
		if err != nil {
			return fmt.Errorf("guardfile: grant %q: %w", modal, err)
		}
		g.Message = v
	case "describe":
		v, err := singleArg(c)
		if err != nil {
			return fmt.Errorf("guardfile: grant %q: %w", modal, err)
		}
		g.Describe = v
	default:
		return fmt.Errorf("guardfile: grant body: unknown node %q (want op | body | message | describe; fail-closed)", c.Name())
	}
	return nil
}

// applyOpNode reads `op "<operationId>"` or the method+path form
// `op method="POST" path="/x/{id}"`, which addresses a spec that names no ops.
func applyOpNode(g *Grant, c *kdl.Node) error {
	props := c.Properties()
	if len(props) == 0 {
		v, err := singleArg(c)
		if err != nil {
			return err
		}
		g.Op = v
		return nil
	}
	if len(c.Arguments()) > 0 {
		return fmt.Errorf("`op` takes an operationId argument or method= and path= properties, never both")
	}
	for key, v := range props {
		switch key {
		case "method":
			g.OpMethod = strings.ToUpper(v.String())
		case "path":
			g.OpPath = v.String()
		default:
			return fmt.Errorf("`op` property %q is unknown (want method | path; fail-closed)", key)
		}
	}
	if g.OpMethod == "" || g.OpPath == "" {
		return fmt.Errorf("`op` in method+path form needs both `method=` and `path=`")
	}
	return nil
}

// parseAllowMeta reads an `allow-metacharacters <param>...` clause: the named
// path params skip the gate. Per-param, never global; docs/specverb-request.md.
func parseAllowMeta(n *kdl.Node) ([]string, error) {
	args := n.Arguments()
	if len(args) == 0 {
		return nil, fmt.Errorf("guardfile: allow-metacharacters needs at least one param name: `allow-metacharacters \"<param>\"...`")
	}
	params := make([]string, 0, len(args))
	for _, a := range args {
		name := a.String()
		if name == "" {
			return nil, fmt.Errorf("guardfile: allow-metacharacters: empty param name")
		}
		params = append(params, name)
	}
	return params, nil
}

// parseRestrict reads a `restrict <param> matches "<glob>"...` allowlist clause.
func parseRestrict(n *kdl.Node) (Restriction, error) {
	args := n.Arguments()
	// shape: restrict <param> matches <glob> [<glob>...]
	if len(args) < 3 || args[1].String() != "matches" {
		return Restriction{}, fmt.Errorf("guardfile: restrict needs `restrict <param> matches \"<glob>\"...`")
	}
	r := Restriction{Param: args[0].String()}
	for _, g := range args[2:] {
		r.Globs = append(r.Globs, g.String())
	}
	return r, nil
}

// ParseActionNode parses one `action` KDL node into an Action: the entry the
// exec dialect shares, so both dialects speak one action grammar.
func ParseActionNode(n *kdl.Node) (Action, error) { return parseAction(n) }

// parseAction reads one `action <name> { ... }` block into an Action. It fails
// closed: an unknown body node, a missing poll, or a reserved keyword is an error.
func parseAction(n *kdl.Node) (Action, error) {
	act, err := actionHeader(n)
	if err != nil {
		return Action{}, err
	}
	for _, c := range n.Children().Nodes {
		if err := applyActionChild(&act, c); err != nil {
			return Action{}, fmt.Errorf("guardfile: action %q: %w", act.Name, err)
		}
	}
	switch {
	case act.Poll == nil && len(act.Calls) == 0 && act.Collect == nil:
		return Action{}, fmt.Errorf("guardfile: action %q: needs a `poll`, `collect`, or at least one `call` step", act.Name)
	case countActionKinds(act) > 1:
		return Action{}, fmt.Errorf("guardfile: action %q: `poll`, `collect`, and `call` are mutually exclusive. "+
			"Combining them would let one action emit an unbounded number of writes, which is the property that "+
			"makes an action reviewable by reading it. Split it into two actions, or see docs/specverb-actions.md", act.Name)
	}
	return act, nil
}

func countActionKinds(act Action) int {
	var n int
	if act.Poll != nil {
		n++
	}
	if len(act.Calls) > 0 {
		n++
	}
	if act.Collect != nil {
		n++
	}
	return n
}

// actionHeader reads an action's header: one arg is `action <name>` (mounts under
// the `action` noun), two is the `action <verb> <resource>` mount form.
func actionHeader(n *kdl.Node) (Action, error) {
	args := n.Arguments()
	switch len(args) {
	case 1:
		return Action{Name: args[0].String()}, nil
	case 2:
		verb, resource := args[0].String(), args[1].String()
		if verb == "" || resource == "" {
			return Action{}, fmt.Errorf("guardfile: action: mount form needs a non-empty verb and resource (`action view issue { ... }`)")
		}
		return Action{Name: verb + "-" + resource, MountVerb: verb, MountResource: resource}, nil
	default:
		return Action{}, fmt.Errorf("guardfile: action: name it `action ci-watch { ... }` or mount it `action view issue { ... }` (got %d header arg(s))", len(args))
	}
}

// applyActionChild dispatches one child node of an action body onto act; the
// step kinds (poll/collect/call) split off to applyActionStep.
func applyActionChild(act *Action, c *kdl.Node) error {
	switch c.Name() {
	case "describe":
		v, err := singleArg(c)
		act.Describe = v
		return err
	case "input":
		in, err := parseInput(c)
		if err != nil {
			return err
		}
		act.Inputs = append(act.Inputs, in)
		return nil
	case "fail-when":
		v, err := singleArg(c)
		if err != nil {
			return fmt.Errorf("fail-when: %w", err)
		}
		act.FailWhen = v
		return nil
	case "reject-empty":
		return applyRejectEmpty(act, c)
	default:
		return applyActionStep(act, c)
	}
}

// applyRejectEmpty reads the bare `reject-empty` marker: no argument, because
// the action it sits in names itself. See docs/execverb-occlusion.md.
func applyRejectEmpty(act *Action, c *kdl.Node) error {
	if len(c.Arguments()) != 0 || len(c.Properties()) != 0 {
		return fmt.Errorf(
			"guardfile: action %q: `reject-empty` takes no argument or property (fail-closed)", act.Name)
	}
	if act.RejectEmpty {
		return fmt.Errorf("guardfile: action %q: duplicate `reject-empty`", act.Name)
	}
	act.RejectEmpty = true
	return nil
}

// applyActionStep dispatches the step-kind children of an action body: the
// poll/collect/call primitives, fail-closed on the rest.
func applyActionStep(act *Action, c *kdl.Node) error {
	switch c.Name() {
	case "poll":
		return addPoll(act, c)
	case "collect":
		return addCollect(act, c)
	case "call":
		call, err := parseCall(c)
		if err != nil {
			return err
		}
		act.Calls = append(act.Calls, call)
		return nil
	case "canary":
		return fmt.Errorf("`canary` is no longer supported: umbra sequences authorized calls but does not own health policy (fail-closed)")
	default:
		if reservedActionKeywords[c.Name()] {
			return fmt.Errorf("%q is reserved for a future version, not implemented in v1 (fail-closed)", c.Name())
		}
		return fmt.Errorf("unknown body node %q (fail-closed)", c.Name())
	}
}

// parseFetch reads a `fetch <name> { method; path; output; env; header; when }`
// overlay leaf. It fails closed on missing required fields or unknown nodes.
func parseFetch(n *kdl.Node) (Fetch, error) {
	name, err := singleArg(n)
	if err != nil {
		return Fetch{}, fmt.Errorf("fetch: %w (name it: `fetch \"actions logs\" { ... }`)", err)
	}
	if name == "" {
		return Fetch{}, fmt.Errorf("guardfile: fetch needs a non-empty name")
	}
	f := Fetch{Name: name, Leaf: fetchLeafName(name)}
	for _, c := range n.Children().Nodes {
		if err := applyFetchChild(&f, c); err != nil {
			return Fetch{}, err
		}
	}
	seenEnv := map[string]bool{}
	for _, env := range f.Env {
		if seenEnv[env.Name] {
			return Fetch{}, fmt.Errorf("guardfile: fetch %q: duplicate env %q (fail-closed)", name, env.Name)
		}
		seenEnv[env.Name] = true
	}
	switch {
	case f.Method == "":
		return Fetch{}, fmt.Errorf("guardfile: fetch %q: `method` is required", name)
	case f.Path == "":
		return Fetch{}, fmt.Errorf("guardfile: fetch %q: `path` is required", name)
	case f.Output == "":
		return Fetch{}, fmt.Errorf("guardfile: fetch %q: `output` is required (use `output \"raw\"`)", name)
	case f.Output != "raw":
		return Fetch{}, fmt.Errorf("guardfile: fetch %q: unsupported output %q (want raw; fail-closed)", name, f.Output)
	}
	if err := validateFetchTemplates(&f); err != nil {
		return Fetch{}, err
	}
	return f, nil
}

func applyFetchChild(f *Fetch, c *kdl.Node) error {
	switch c.Name() {
	case "describe":
		return assignFetchString(f.Name, c, func(v string) { f.Describe = v })
	case "method":
		return assignFetchString(f.Name, c, func(v string) { f.Method = strings.ToUpper(v) })
	case "path":
		return assignFetchString(f.Name, c, func(v string) { f.Path = v })
	case "output":
		return assignFetchString(f.Name, c, func(v string) { f.Output = v })
	case "env":
		return appendFetchEnv(f, c)
	case "header":
		return appendFetchHeader(f, c)
	case "when":
		return appendFetchWhen(f, c)
	default:
		return fmt.Errorf("guardfile: fetch %q: unknown node %q (want describe | method | path | output | env | header | when; fail-closed)", f.Name, c.Name())
	}
}

func assignFetchString(name string, c *kdl.Node, set func(string)) error {
	v, err := singleArg(c)
	if err != nil {
		return fmt.Errorf("guardfile: fetch %q: %w", name, err)
	}
	set(v)
	return nil
}

func appendFetchEnv(f *Fetch, c *kdl.Node) error {
	env, err := parseFetchEnv(c)
	if err != nil {
		return err
	}
	f.Env = append(f.Env, env)
	return nil
}

func appendFetchHeader(f *Fetch, c *kdl.Node) error {
	h, err := parseFetchHeader(c)
	if err != nil {
		return err
	}
	f.Headers = append(f.Headers, h)
	return nil
}

func appendFetchWhen(f *Fetch, c *kdl.Node) error {
	when, err := parseFetchWhen(c)
	if err != nil {
		return err
	}
	f.Whens = append(f.Whens, when)
	return nil
}

func parseFetchEnv(n *kdl.Node) (FetchEnv, error) {
	name, err := singleArg(n)
	if err != nil {
		return FetchEnv{}, fmt.Errorf("guardfile: fetch env: %w (name it: `env FORGEJO_TOKEN { value ssm \"/path\" }`)", err)
	}
	if name == "" {
		return FetchEnv{}, fmt.Errorf("guardfile: fetch env needs a non-empty name")
	}
	env := FetchEnv{Name: name}
	for _, c := range n.Children().Nodes {
		if c.Name() != "value" {
			return FetchEnv{}, fmt.Errorf("guardfile: fetch env %q: unknown node %q (want value; fail-closed)", name, c.Name())
		}
		if !env.Value.IsZero() {
			return FetchEnv{}, fmt.Errorf("guardfile: fetch env %q: duplicate `value` (fail-closed)", name)
		}
		vc, err := parseValueChain(c)
		if err != nil {
			return FetchEnv{}, fmt.Errorf("guardfile: fetch env %q: %w", name, err)
		}
		env.Value = vc
	}
	if env.Value.IsZero() {
		return FetchEnv{}, fmt.Errorf("guardfile: fetch env %q requires `value <provider> \"...\"`", name)
	}
	return env, nil
}

func parseFetchHeader(n *kdl.Node) (FetchHeader, error) {
	args := n.Arguments()
	if len(args) != 2 || args[0].String() == "" {
		return FetchHeader{}, fmt.Errorf("guardfile: fetch header needs `header \"Name\" \"value\"` (got %d arg(s))", len(args))
	}
	if args[1].String() == "" {
		return FetchHeader{}, fmt.Errorf("guardfile: fetch header %q needs a non-empty value", args[0].String())
	}
	if len(n.Children().Nodes) > 0 || len(n.Properties()) > 0 {
		return FetchHeader{}, fmt.Errorf("guardfile: fetch header %q takes only positional args, no children or properties (fail-closed)", args[0].String())
	}
	return FetchHeader{Name: args[0].String(), Value: args[1].String()}, nil
}

func parseFetchWhen(n *kdl.Node) (FetchWhen, error) {
	args := n.Arguments()
	if len(args) < 3 {
		return FetchWhen{}, fmt.Errorf("guardfile: fetch when needs `when <selector> matches <glob...>`")
	}
	selector, globs, err := parseFetchWhenArgs(args)
	if err != nil {
		return FetchWhen{}, err
	}
	if selector == "" {
		return FetchWhen{}, fmt.Errorf("guardfile: fetch when needs a non-empty selector")
	}
	if !validFetchSelector(selector) {
		return FetchWhen{}, fmt.Errorf("guardfile: fetch when selector %q unsupported (want argN or first input; fail-closed)", selector)
	}
	if len(globs) == 0 {
		return FetchWhen{}, fmt.Errorf("guardfile: fetch when %q needs at least one glob", selector)
	}
	for _, g := range globs {
		if g == "" {
			return FetchWhen{}, fmt.Errorf("guardfile: fetch when %q has an empty glob (fail-closed)", selector)
		}
	}
	return FetchWhen{Selector: selector, Globs: globs}, nil
}

func parseFetchWhenArgs(args []kdl.Value) (string, []string, error) {
	if len(args) >= 4 && args[0].String() == "first" && args[1].String() == "input" && args[2].String() == "matches" {
		return "arg0", valueStrings(args[3:]), nil
	}
	if args[1].String() != "matches" {
		return "", nil, fmt.Errorf("guardfile: fetch when needs `when <selector> matches <glob...>`")
	}
	return args[0].String(), valueStrings(args[2:]), nil
}

func validFetchSelector(selector string) bool {
	if selector == "arg0" {
		return true
	}
	if len(selector) < 4 || !strings.HasPrefix(selector, "arg") {
		return false
	}
	for _, r := range selector[3:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func valueStrings(vals []kdl.Value) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, v.String())
	}
	return out
}

func validateFetchTemplates(f *Fetch) error {
	envNames := map[string]bool{}
	for _, e := range f.Env {
		envNames[e.Name] = true
	}
	for _, h := range f.Headers {
		if strings.Contains(h.Value, "${") && !strings.Contains(h.Value, "}") {
			return fmt.Errorf("guardfile: fetch %q: header %q has an unterminated ${...} placeholder", f.Name, h.Name)
		}
		for _, name := range fetchTemplateNames(h.Value) {
			if !envNames[name] {
				return fmt.Errorf("guardfile: fetch %q: header %q references undeclared env %q (fail-closed)", f.Name, h.Name, name)
			}
		}
	}
	return nil
}

func fetchTemplateNames(tpl string) []string {
	var out []string
	for start := 0; start < len(tpl); {
		i := strings.Index(tpl[start:], "${")
		if i < 0 {
			break
		}
		i += start
		j := strings.IndexByte(tpl[i+2:], '}')
		if j < 0 {
			break
		}
		j += i + 2
		name := tpl[i+2 : j]
		if name != "" {
			out = append(out, name)
		}
		start = j + 1
	}
	return out
}

func fetchLeafName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	leaf := strings.Trim(b.String(), "-")
	if leaf == "" {
		return "fetch"
	}
	return leaf
}

// addPoll parses a poll child and attaches it to act, rejecting a second poll.
func addPoll(act *Action, c *kdl.Node) error {
	if act.Poll != nil {
		return fmt.Errorf("v1 allows exactly one `poll` per action")
	}
	p, err := parsePoll(c)
	if err != nil {
		return err
	}
	act.Poll = &p
	return nil
}

// addCollect parses a collect child and attaches it to act, rejecting a second
// collect block.
func addCollect(act *Action, c *kdl.Node) error {
	if act.Collect != nil {
		return fmt.Errorf("v1 allows exactly one `collect` per action")
	}
	col, err := parseCollect(c)
	if err != nil {
		return err
	}
	act.Collect = &col
	return nil
}

// parseInput reads one `input <name> { ... }` child: exactly one of
// positional/flag is required; required and default are mutually exclusive.
func parseInput(n *kdl.Node) (Input, error) {
	name, err := singleArg(n)
	if err != nil {
		return Input{}, fmt.Errorf("input: %w (name it: `input repo { positional }`)", err)
	}
	in := Input{Name: name}
	kindSet := false
	for _, c := range n.Children().Nodes {
		set, ferr := applyInputField(&in, c)
		if ferr != nil {
			return Input{}, ferr
		}
		kindSet = kindSet || set
	}
	if !kindSet {
		return Input{}, fmt.Errorf("input %q: declare exactly one of `positional` or `flag`", name)
	}
	if in.Required && in.Default != "" {
		return Input{}, fmt.Errorf("input %q: `required` and `default` are mutually exclusive (a default only resolves when the input is absent)", name)
	}
	if in.Array && in.Positional {
		return Input{}, fmt.Errorf("input %q: `array` needs `flag` (a positional list cannot be told from the arguments after it)", name)
	}
	if in.Array && in.Default != "" {
		return Input{}, fmt.Errorf("input %q: `array` and `default` are mutually exclusive (a default resolves one scalar)", name)
	}
	return in, nil
}

// applyInputField applies one child field of an input block to in, reporting
// whether it declared the positional/flag kind. An unknown field fails closed.
func applyInputField(in *Input, c *kdl.Node) (kindSet bool, err error) {
	switch c.Name() {
	case "positional":
		in.Positional = true
		return true, nil
	case "flag":
		in.Positional = false
		return true, nil
	case "required":
		in.Required = true
		return false, nil
	case "help":
		v, herr := singleArg(c)
		if herr != nil {
			return false, fmt.Errorf("input %q: %w", in.Name, herr)
		}
		in.Help = v
		return false, nil
	case "default":
		v, derr := singleArg(c)
		if derr != nil {
			return false, fmt.Errorf("input %q: %w", in.Name, derr)
		}
		in.Default = v
		return false, nil
	case "array":
		in.Array = true
		return false, nil
	case "matches":
		m, merr := parseInputMatch(in.Name, c)
		if merr != nil {
			return false, merr
		}
		in.Matches = append(in.Matches, m)
		return false, nil
	default:
		return false, fmt.Errorf("input %q: unknown field %q (want positional | flag | required | help | default | array | matches; fail-closed)", in.Name, c.Name())
	}
}

// parseInputMatch reads one `matches "<glob...>" message="<why>"` constraint,
// variadic like `restrict`. `message` is the only property; any other fails closed.
func parseInputMatch(name string, c *kdl.Node) (InputMatch, error) {
	args := c.Arguments()
	if len(args) == 0 {
		return InputMatch{}, fmt.Errorf("input %q: matches: name at least one glob, e.g. `matches \"priority/*\"` (fail-closed)", name)
	}
	m := InputMatch{}
	for _, a := range args {
		g := a.String()
		if g == "" {
			return InputMatch{}, fmt.Errorf("input %q: matches: a glob cannot be empty (fail-closed)", name)
		}
		m.Globs = append(m.Globs, g)
	}
	for k, v := range c.Properties() {
		if k != "message" {
			return InputMatch{}, fmt.Errorf("input %q: matches %v: unknown property %q (want message; fail-closed)", name, m.Globs, k)
		}
		m.Message = v.String()
	}
	return m, nil
}

// parseCall reads a `call <verb> <resource> { args {...}; as <name> }` step of a
// multi-call action. Args and As are both optional. See specverb-actions.md.
func parseCall(n *kdl.Node) (Call, error) {
	args := n.Arguments()
	if len(args) != 2 {
		return Call{}, fmt.Errorf("call needs a verb and a resource, e.g. `call view issues { ... }`")
	}
	cl := Call{Verb: args[0].String(), Resource: args[1].String()}
	for _, c := range n.Children().Nodes {
		switch c.Name() {
		case "args":
			binds, err := parseArgBinds(c, "call")
			if err != nil {
				return Call{}, err
			}
			cl.Args = binds
		case "as":
			v, err := singleArg(c)
			if err != nil {
				return Call{}, fmt.Errorf("call as: %w", err)
			}
			cl.As = v
		case "compensate":
			return Call{}, fmt.Errorf("`compensate` is no longer supported: umbra does not perform automatic rollback (fail-closed)")
		default:
			if reservedActionKeywords[c.Name()] {
				return Call{}, fmt.Errorf("call: %q is reserved for a future version (fail-closed)", c.Name())
			}
			return Call{}, fmt.Errorf("call: unknown body node %q (want args | as; fail-closed)", c.Name())
		}
	}
	return cl, nil
}

// parseArgBinds reads an `args` block, two forms: named children (spec dialect)
// or positional values (exec dialect, Name empty). Mixing them fails closed.
func parseArgBinds(n *kdl.Node, label string) ([]ArgBind, error) {
	var binds []ArgBind
	if args := n.Arguments(); len(args) > 0 {
		if len(n.Children().Nodes) > 0 {
			return nil, fmt.Errorf("%s args: positional values and named children cannot mix (fail-closed)", label)
		}
		for _, a := range args {
			binds = append(binds, ArgBind{Value: a.String()})
		}
		return binds, nil
	}
	for _, a := range n.Children().Nodes {
		v, err := singleArg(a)
		if err != nil {
			return nil, fmt.Errorf("%s args %q: %w", label, a.Name(), err)
		}
		binds = append(binds, ArgBind{Name: a.Name(), Value: v})
	}
	return binds, nil
}

// parsePoll reads a `poll <verb> <resource> { args {...}; until; every; timeout;
// as }` block. Every, Timeout, Until, and As are mandatory: the bound is grammar.
func parsePoll(n *kdl.Node) (Poll, error) {
	args := n.Arguments()
	if len(args) != 2 {
		return Poll{}, fmt.Errorf("poll needs a verb and a resource, e.g. `poll list tasks { ... }`")
	}
	p := Poll{Verb: args[0].String(), Resource: args[1].String()}
	for _, c := range n.Children().Nodes {
		if err := applyPollChild(&p, c); err != nil {
			return Poll{}, err
		}
	}
	switch {
	case p.Until == "":
		return Poll{}, fmt.Errorf("poll: `until` is required (the JMESPath that ends the loop)")
	case p.Every == "":
		return Poll{}, fmt.Errorf("poll: `every` is required (no unbounded poll exists in the grammar)")
	case p.Timeout == "":
		return Poll{}, fmt.Errorf("poll: `timeout` is required (no unbounded poll exists in the grammar)")
	case p.As == "":
		return Poll{}, fmt.Errorf("poll: `as` is required (the binding name for the final response)")
	}
	return p, nil
}

// parseCollect reads a `collect <verb> <resource> { args {...}; page-param;
// limit-param; limit; default-limit; as; cache }` block.
func parseCollect(n *kdl.Node) (Collect, error) {
	args := n.Arguments()
	if len(args) != 2 {
		return Collect{}, fmt.Errorf("collect needs a verb and a resource, e.g. `collect list issues { ... }`")
	}
	col := Collect{Verb: args[0].String(), Resource: args[1].String()}
	for _, c := range n.Children().Nodes {
		if err := applyCollectChild(&col, c); err != nil {
			return Collect{}, err
		}
	}
	switch {
	case col.PageParam == "":
		return Collect{}, fmt.Errorf("collect: `page-param` is required")
	case col.LimitParam == "":
		return Collect{}, fmt.Errorf("collect: `limit-param` is required")
	case col.DefaultLimit == "":
		return Collect{}, fmt.Errorf("collect: `default-limit` is required")
	case col.As == "":
		return Collect{}, fmt.Errorf("collect: `as` is required (the binding name for the accumulated array)")
	}
	return col, nil
}

func applyCollectChild(col *Collect, c *kdl.Node) error {
	if c.Name() == "args" {
		for _, a := range c.Children().Nodes {
			v, err := singleArg(a)
			if err != nil {
				return fmt.Errorf("collect args %q: %w", a.Name(), err)
			}
			col.Args = append(col.Args, ArgBind{Name: a.Name(), Value: v})
		}
		return nil
	}
	scalars := map[string]*string{
		"page-param":    &col.PageParam,
		"limit-param":   &col.LimitParam,
		"limit":         &col.Limit,
		"default-limit": &col.DefaultLimit,
		"as":            &col.As,
		"cache":         &col.Cache,
	}
	target, ok := scalars[c.Name()]
	if !ok {
		if reservedActionKeywords[c.Name()] {
			return fmt.Errorf("collect: %q is reserved for a future version (fail-closed)", c.Name())
		}
		return fmt.Errorf("collect: unknown body node %q (want args | page-param | limit-param | limit | default-limit | as | cache; fail-closed)", c.Name())
	}
	v, err := singleArg(c)
	if err != nil {
		return fmt.Errorf("collect %s: %w", c.Name(), err)
	}
	*target = v
	return nil
}

// applyPollChild dispatches one child node of a poll body onto p; the scalar
// fields (until/every/timeout/as) share one single-arg path keyed by a table.
func applyPollChild(p *Poll, c *kdl.Node) error {
	if c.Name() == "args" {
		for _, a := range c.Children().Nodes {
			v, err := singleArg(a)
			if err != nil {
				return fmt.Errorf("poll args %q: %w", a.Name(), err)
			}
			p.Args = append(p.Args, ArgBind{Name: a.Name(), Value: v})
		}
		return nil
	}
	scalars := map[string]*string{"until": &p.Until, "every": &p.Every, "timeout": &p.Timeout, "as": &p.As}
	target, ok := scalars[c.Name()]
	if !ok {
		if reservedActionKeywords[c.Name()] {
			return fmt.Errorf("poll: %q is reserved for a future version, not implemented in v1 (fail-closed)", c.Name())
		}
		return fmt.Errorf("poll: unknown body node %q (want args | until | every | timeout | as; fail-closed)", c.Name())
	}
	v, err := singleArg(c)
	if err != nil {
		return fmt.Errorf("poll %s: %w (durations quote: `every \"10s\"`)", c.Name(), err)
	}
	*target = v
	return nil
}

// singleArg returns the lone argument of a node, verbatim (significant spaces
// kept), erroring unless there is exactly one.
func singleArg(n *kdl.Node) (string, error) {
	args := n.Arguments()
	if len(args) != 1 {
		return "", fmt.Errorf("%s expects exactly one value, got %d", n.Name(), len(args))
	}
	return args[0].String(), nil
}

// parseProvider reads `provider <name> { exec <argv...> }`: the consumer's
// store-backed resolver, run as a subprocess with the address appended.
func parseProvider(n *kdl.Node) (ProviderDecl, error) {
	name, err := singleArg(n)
	if err != nil {
		return ProviderDecl{}, fmt.Errorf("guardfile: provider: %w", err)
	}
	pd := ProviderDecl{Name: name}
	for _, c := range n.Children().Nodes {
		if c.Name() != "exec" {
			return ProviderDecl{}, fmt.Errorf("guardfile: provider %s: unknown field %q (want exec; fail-closed)", name, c.Name())
		}
		for _, a := range c.Arguments() {
			pd.Exec = append(pd.Exec, argText(a))
		}
	}
	if len(pd.Exec) == 0 {
		return ProviderDecl{}, fmt.Errorf("guardfile: provider %s requires `exec <argv...>`", name)
	}
	return pd, nil
}

// ParseProviderNode exposes the provider grammar to the exec dialect, so both
// dialects declare store-backed resolvers the same way.
func ParseProviderNode(n *kdl.Node) (ProviderDecl, error) { return parseProvider(n) }

// argText renders a KDL argument as the literal token a shell would receive.
// Value.String() debug-formats non-string kinds, so a bare `-4` needs this.
func argText(v kdl.Value) string {
	if v.Kind() == kdl.String {
		return v.String()
	}
	if lit, ok := v.Literal(); ok {
		return lit
	}
	return fmt.Sprintf("%v", v.RawValue())
}

// countWrap counts top-level `wrap` nodes. One guardfile is one wrap.
func countWrap(doc *kdl.Document) int {
	n := 0
	for _, node := range doc.Nodes {
		if node.Name() == "wrap" {
			n++
		}
	}
	return n
}
