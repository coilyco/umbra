// Package mcpverb mounts MCP-shaped verbs from a KDL Guardfile: the mcp
// dialect of the spec-driven design, beside the spec dialect (specverb) and the
// exec dialect (execverb). One `can call` grant becomes one guarded leaf that
// fires tools/call against a declared upstream. See docs/mcpverb.md.
package mcpverb

import (
	"fmt"
	"regexp"
	"strings"

	kdl "github.com/calico32/kdl-go"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
)

// TransportNode is the `wrap` child that marks a guardfile as the mcp dialect,
// the way `exec` marks the exec one. The driver sniffs on this name.
const TransportNode = "mcp"

// WildcardTool is the `can call *` sentinel: grant every tool the lock holds.
const WildcardTool = "*"

// Guardfile is the parsed form of one mcp-dialect wrap block.
type Guardfile struct {
	// Description is the optional top-level `description "..."` prose, a sibling
	// of `wrap`, matching the other two dialects.
	Description string

	Group  []string // command path, e.g. ["aosguard", "ops", "forgejo"]
	Server Server   // the single declared upstream
	Grants []Grant

	// Restrict are wrap-level argument allowlists, enforced on every leaf against
	// the bound tool arguments rather than path segments.
	Restrict []guardfile.Restriction

	// ProviderDecls are consumer-declared value resolvers, shared verbatim with
	// the other two dialects. See docs/value-providers.md.
	ProviderDecls []guardfile.ProviderDecl
}

// Server is the declared upstream: exactly one transport, its credential
// material named as value chains that resolve at call time rather than parse.
type Server struct {
	Kind string // "stdio" | "http"

	// Stdio fields.
	Command string
	Argv    []string
	Env     []EnvVar

	// HTTP fields.
	URL      string
	URLValue guardfile.ValueChain
	Auth     guardfile.Auth
}

// EnvVar is one environment injection on a stdio upstream. A secret reaches the
// child process here rather than through argv, which any local process can read.
type EnvVar struct {
	Name  string
	Value guardfile.ValueChain
}

// Grant is one `can call <tool>` sentence and its guards.
type Grant struct {
	Modal string // can | cannot | never
	Tool  string // upstream tool name, or WildcardTool
	Name  string // local leaf name; derived from Tool when unset

	Describe string
	Message  string // teaching text on a deny
	FailWhen string // JMESPath postcondition over the call result

	// Destructive marks a leaf that mutates irreversibly, so the audit row and
	// help say so. MCP has no verb to infer it from, unlike HTTP's DELETE.
	Destructive bool

	Allow    []opcore.ProxyRule
	Deny     []opcore.ProxyRule
	PostCall []opcore.ProxyRule

	// Widget declares what this tool's MCP Apps view may call back. Absent
	// means nothing reaches the upstream. See docs/mcpapps.md.
	Widget *Widget
}

// Widget is the `widget { ... }` block inside a grant, taking the same
// `can call` sentence the outer block does.
type Widget struct {
	Grants []Grant

	// Reads, Opens, and Saves gate resources/read, ui/open-link, and
	// ui/download-file. Each is its own grant: they reach different places.
	Reads []URIRule
	Opens []URIRule
	Saves []URIRule

	// Connects feed connect-src: CSP source expressions rather than regexes,
	// and allow-only. See docs/mcpapps.md.
	Connects []string
}

// URIRule is one `can read` / `open` / `save` sentence: an unanchored regex over
// the whole URI, deny by absence like the tool grants.
type URIRule struct {
	Modal   string // can | cannot | never
	Verb    string // read | open | save
	Pattern string
}

// IsDeny reports whether the rule closes a URI rather than opening it.
func (r URIRule) IsDeny() bool { return r.Modal == "cannot" || r.Modal == "never" }

// IsDeny reports whether the grant closes a tool rather than opening it.
func (g Grant) IsDeny() bool { return g.Modal == "cannot" || g.Modal == "never" }

// LeafName is the CLI leaf this grant mounts: an MCP tool name is flat, so it
// lowers to one kebab-case leaf under the wrap group. See docs/mcpverb.md.
func (g Grant) LeafName() string {
	if g.Name != "" {
		return g.Name
	}
	return LeafFor(g.Tool)
}

// LeafFor derives the default leaf name for an upstream tool name.
func LeafFor(tool string) string {
	return strings.ReplaceAll(tool, "_", "-")
}

// Providers returns the distinct value-source provider names this guardfile
// names, so the driver wires exactly the resolvers in play.
func (gf *Guardfile) Providers() []string {
	seen := map[string]bool{}
	var out []string
	add := func(chain guardfile.ValueChain) {
		for _, vs := range chain {
			if vs.Provider != "" && !seen[vs.Provider] {
				seen[vs.Provider] = true
				out = append(out, vs.Provider)
			}
		}
	}
	for _, e := range gf.Server.Env {
		add(e.Value)
	}
	add(gf.Server.URLValue)
	add(gf.Server.Auth.Value)
	for _, p := range gf.Server.Auth.Params {
		add(p.Value)
	}
	return out
}

// Parse turns mcp-dialect Guardfile source into a Guardfile. It fails closed:
// an unknown node, a missing requirement, or a malformed sentence is an error.
func Parse(src []byte) (*Guardfile, error) {
	doc, err := kdl.ParseString(string(src))
	if err != nil {
		return nil, fmt.Errorf("mcpverb: parse KDL: %w", err)
	}
	wrap := doc.GetNode("wrap")
	if wrap == nil {
		return nil, fmt.Errorf("mcpverb: missing top-level `wrap` node")
	}
	// GetNode returns the first, so a second wrap would drop silently (umbra#8163).
	if n := countWrap(doc); n > 1 {
		return nil, fmt.Errorf("mcpverb: %d top-level `wrap` nodes; a guardfile holds one, so split the rest into their own files (fail-closed)", n)
	}
	gf := &Guardfile{}
	if d := doc.GetNode("description"); d != nil {
		v, derr := singleArg(d, "description")
		if derr != nil {
			return nil, fmt.Errorf("mcpverb: %w", derr)
		}
		gf.Description = v
	}
	for _, a := range wrap.Arguments() {
		gf.Group = append(gf.Group, a.String())
	}
	if len(gf.Group) == 0 {
		return nil, fmt.Errorf("mcpverb: `wrap` needs a command path, e.g. `wrap aosguard ops forgejo`")
	}
	for _, n := range wrap.Children().Nodes {
		if err := gf.applyNode(n); err != nil {
			return nil, err
		}
	}
	return gf, gf.validate()
}

// applyNode dispatches one child of the wrap block, failing closed outside the
// grammar in docs/mcpverb.md.
func (gf *Guardfile) applyNode(n *kdl.Node) error {
	switch n.Name() {
	case TransportNode:
		return gf.applyServer(n)
	case "can", "cannot", "never":
		return gf.applyGrant(n)
	case "restrict":
		r, err := guardfile.ParseRestrictNode(n)
		if err != nil {
			return fmt.Errorf("mcpverb: %w", err)
		}
		gf.Restrict = append(gf.Restrict, r)
		return nil
	case "provider":
		p, err := guardfile.ParseProviderNode(n)
		if err != nil {
			return fmt.Errorf("mcpverb: %w", err)
		}
		gf.ProviderDecls = append(gf.ProviderDecls, p)
		return nil
	default:
		return fmt.Errorf("mcpverb: unknown node %q in `wrap` (want mcp | can | cannot | never | restrict | provider; fail-closed)", n.Name())
	}
}

// applyServer reads the single `mcp <transport> { ... }` declaration.
func (gf *Guardfile) applyServer(n *kdl.Node) error {
	if gf.Server.Kind != "" {
		return fmt.Errorf("mcpverb: duplicate `mcp` block; a wrap declares one upstream (fail-closed)")
	}
	kind, err := singleArg(n, TransportNode)
	if err != nil {
		return fmt.Errorf("mcpverb: %w", err)
	}
	switch kind {
	case "stdio", "http":
	default:
		return fmt.Errorf("mcpverb: unknown mcp transport %q (want stdio | http; fail-closed)", kind)
	}
	gf.Server.Kind = kind
	for _, c := range n.Children().Nodes {
		if err := gf.applyServerChild(kind, c); err != nil {
			return fmt.Errorf("mcpverb: mcp %s: %w", kind, err)
		}
	}
	return nil
}

// applyServerChild dispatches one field of the transport block. A field of the
// other transport is rejected rather than ignored, never silently dropped.
func (gf *Guardfile) applyServerChild(kind string, c *kdl.Node) error {
	stdioField := map[string]bool{"command": true, "argv": true, "env": true}
	httpField := map[string]bool{"url": true, "auth": true}
	switch {
	case stdioField[c.Name()] && kind != "stdio":
		return fmt.Errorf("`%s` belongs to the stdio transport", c.Name())
	case c.Name() == "auth" && kind != "http":
		return fmt.Errorf("`auth` belongs to the http transport; a stdio upstream takes credentials through `env`")
	case httpField[c.Name()] && kind != "http":
		return fmt.Errorf("`%s` belongs to the http transport", c.Name())
	case stdioField[c.Name()]:
		return gf.applyStdioField(c)
	case httpField[c.Name()]:
		return gf.applyHTTPField(c)
	default:
		return fmt.Errorf("unknown field %q (fail-closed)", c.Name())
	}
}

// applyStdioField reads one field of the stdio transport.
func (gf *Guardfile) applyStdioField(c *kdl.Node) error {
	switch c.Name() {
	case "command":
		v, err := singleArg(c, "command")
		if err != nil {
			return err
		}
		gf.Server.Command = v
	case "argv":
		for _, a := range c.Arguments() {
			gf.Server.Argv = append(gf.Server.Argv, a.String())
		}
	default: // env
		return gf.applyEnv(c)
	}
	return nil
}

// applyHTTPField reads one field of the http transport.
func (gf *Guardfile) applyHTTPField(c *kdl.Node) error {
	if c.Name() == "auth" {
		a, err := guardfile.ParseAuthNode(c)
		if err != nil {
			return err
		}
		gf.Server.Auth = a
		return nil
	}
	raw, chain, err := guardfile.ParseBaseURL(c)
	if err != nil {
		return err
	}
	gf.Server.URL, gf.Server.URLValue = raw, chain
	return nil
}

// applyEnv reads one `env NAME { value <provider> "<address>" }` injection.
func (gf *Guardfile) applyEnv(c *kdl.Node) error {
	name, err := singleArg(c, "env")
	if err != nil {
		return err
	}
	chain, err := guardfile.ParseValueBlock(c, "env "+name)
	if err != nil {
		return err
	}
	gf.Server.Env = append(gf.Server.Env, EnvVar{Name: name, Value: chain})
	return nil
}

// applyGrant reads one `<modal> call <tool> { ... }` sentence.
func (gf *Guardfile) applyGrant(n *kdl.Node) error {
	args := n.Arguments()
	if len(args) != 2 || args[0].String() != "call" {
		return fmt.Errorf("mcpverb: `%s` needs `call <tool>`, e.g. `can call list_issue` (fail-closed)", n.Name())
	}
	tool := args[1].String()
	if tool == "" {
		return fmt.Errorf("mcpverb: `%s call` needs a non-empty tool name", n.Name())
	}
	g := Grant{Modal: n.Name(), Tool: tool}
	for _, c := range n.Children().Nodes {
		if err := applyGrantChild(&g, c); err != nil {
			return fmt.Errorf("mcpverb: %s call %s: %w", g.Modal, tool, err)
		}
	}
	if g.IsDeny() && (len(g.Allow) > 0 || len(g.Deny) > 0 || len(g.PostCall) > 0 || g.FailWhen != "" || g.Widget != nil) {
		return fmt.Errorf("mcpverb: %s call %s: a deny mounts no leaf, so it carries no guards (fail-closed)", g.Modal, tool)
	}
	gf.Grants = append(gf.Grants, g)
	return nil
}

// applyGrantChild dispatches one child of a grant body.
func applyGrantChild(g *Grant, c *kdl.Node) error {
	if dst := stringTarget(g, c.Name()); dst != nil {
		v, err := singleArg(c, c.Name())
		if err != nil {
			return err
		}
		*dst = v
		return nil
	}
	switch c.Name() {
	case "destructive":
		if len(c.Arguments()) > 0 {
			return fmt.Errorf("`destructive` is a bare marker and takes no argument (fail-closed)")
		}
		g.Destructive = true
		return nil
	case "allow", "deny", "post-call":
		return appendRule(g, c)
	case "widget":
		return applyWidget(g, c)
	default:
		return fmt.Errorf("unknown node %q (want name | describe | message | fail-when | destructive | allow | deny | post-call | widget; fail-closed)", c.Name())
	}
}

// applyWidget reads the `widget { ... }` block: what the tool's MCP Apps view
// may call back. One block per grant, so two cannot silently union.
func applyWidget(g *Grant, n *kdl.Node) error {
	if g.Widget != nil {
		return fmt.Errorf("duplicate `widget` block; a grant declares one view policy (fail-closed)")
	}
	if len(n.Arguments()) > 0 {
		return fmt.Errorf("`widget` takes no arguments, only a body (fail-closed)")
	}
	w := &Widget{}
	for _, c := range n.Children().Nodes {
		if err := applyWidgetChild(w, c); err != nil {
			return fmt.Errorf("widget: %w", err)
		}
	}
	g.Widget = w
	return nil
}

// applyWidgetChild dispatches one sentence of a widget body.
func applyWidgetChild(w *Widget, c *kdl.Node) error {
	switch c.Name() {
	case "can", "cannot", "never":
	default:
		return fmt.Errorf("unknown node %q (want can | cannot | never; fail-closed)", c.Name())
	}
	args := c.Arguments()
	if len(args) < 1 {
		return fmt.Errorf("`%s` needs `call <tool>`, `read \"<regex>\"`, or `connect \"<source>\"` (fail-closed)", c.Name())
	}
	switch verb := args[0].String(); verb {
	case "call":
		return applyWidgetCall(w, c)
	case "read", "open", "save":
		return applyWidgetURI(w, c, verb)
	case "connect":
		return applyWidgetConnect(w, c)
	default:
		return fmt.Errorf("`%s %s` is not a widget sentence (want call | read | open | save | connect; fail-closed)", c.Name(), verb)
	}
}

// applyWidgetCall reads one `<modal> call <tool> { ... }` sentence inside a
// widget block. A wildcard is refused here even though the outer block takes one.
func applyWidgetCall(w *Widget, c *kdl.Node) error {
	args := c.Arguments()
	if len(args) != 2 {
		return fmt.Errorf("`%s call` needs exactly one tool name (fail-closed)", c.Name())
	}
	tool := args[1].String()
	switch tool {
	case "":
		return fmt.Errorf("`%s call` needs a non-empty tool name", c.Name())
	case WildcardTool:
		return fmt.Errorf("`%s call *` is refused inside `widget`; name each tool a view may call (fail-closed)", c.Name())
	}
	g := Grant{Modal: c.Name(), Tool: tool}
	for _, b := range c.Children().Nodes {
		if err := applyWidgetGrantChild(&g, b); err != nil {
			return fmt.Errorf("%s call %s: %w", g.Modal, tool, err)
		}
	}
	if g.IsDeny() && (len(g.Allow) > 0 || len(g.Deny) > 0) {
		return fmt.Errorf("%s call %s: a deny grants no call, so it carries no guards (fail-closed)", g.Modal, tool)
	}
	w.Grants = append(w.Grants, g)
	return nil
}

// applyWidgetGrantChild dispatches one child of a widget grant body. A view call
// mounts no CLI leaf, so the leaf-shaped settings are refused.
func applyWidgetGrantChild(g *Grant, c *kdl.Node) error {
	switch c.Name() {
	case "allow", "deny":
		return appendRule(g, c)
	default:
		return fmt.Errorf("unknown node %q; a view call mounts no leaf, so it takes only allow | deny (fail-closed)", c.Name())
	}
}

// applyWidgetURI reads one `<modal> <verb> "<regex>"` sentence, for the three
// verbs that name a URI rather than a tool.
func applyWidgetURI(w *Widget, c *kdl.Node, verb string) error {
	args := c.Arguments()
	if len(args) != 2 {
		return fmt.Errorf("`%s %s` needs exactly one regex, e.g. `%s %s \"^ui://\"` (fail-closed)", c.Name(), verb, c.Name(), verb)
	}
	pattern := args[1].String()
	if pattern == "" {
		return fmt.Errorf("`%s %s` has an empty regex (fail-closed)", c.Name(), verb)
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("`%s %s` regex %q does not compile: %w", c.Name(), verb, pattern, err)
	}
	if len(c.Children().Nodes) > 0 {
		return fmt.Errorf("`%s %s` takes no body (fail-closed)", c.Name(), verb)
	}
	rule := URIRule{Modal: c.Name(), Verb: verb, Pattern: pattern}
	switch verb {
	case "read":
		w.Reads = append(w.Reads, rule)
	case "open":
		w.Opens = append(w.Opens, rule)
	default:
		w.Saves = append(w.Saves, rule)
	}
	return nil
}

// applyWidgetConnect reads one `can connect "<source>"` sentence. CSP is an
// allowlist with no deny form, so a deny modal is refused rather than ignored.
func applyWidgetConnect(w *Widget, c *kdl.Node) error {
	if c.Name() != "can" {
		return fmt.Errorf(
			"`%s connect` cannot be honoured: CSP is an allowlist with no deny form, so there is no source expression meaning \"not this origin\". Drop the rule and declare only what the view may reach (fail-closed)",
			c.Name(),
		)
	}
	args := c.Arguments()
	if len(args) != 2 {
		return fmt.Errorf("`can connect` needs exactly one source expression (fail-closed)")
	}
	source := strings.TrimSpace(args[1].String())
	if err := checkCSPSource(source); err != nil {
		return err
	}
	if len(c.Children().Nodes) > 0 {
		return fmt.Errorf("`can connect` takes no body (fail-closed)")
	}
	w.Connects = append(w.Connects, source)
	return nil
}

// checkCSPSource refuses what a CSP source expression cannot be. A semicolon
// would end the directive and start another one, which is the sharp case.
func checkCSPSource(source string) error {
	switch {
	case source == "":
		return fmt.Errorf("`can connect` needs a non-empty source expression (fail-closed)")
	case strings.ContainsAny(source, ";,"):
		return fmt.Errorf("`can connect %q` contains a directive separator, which would inject a second directive (fail-closed)", source)
	case strings.ContainsAny(source, " \t\n"):
		return fmt.Errorf("`can connect %q` has whitespace: declare one source per sentence (fail-closed)", source)
	case strings.ContainsAny(source, `^$\\`):
		return fmt.Errorf("`can connect %q` looks like a regex: this verb takes a CSP source expression such as https://api.example.com (fail-closed)", source)
	}
	return nil
}

// stringTarget maps a grant-body node name onto the field it sets, nil when the
// node is not one of the plain string settings.
func stringTarget(g *Grant, name string) *string {
	switch name {
	case "name":
		return &g.Name
	case "describe":
		return &g.Describe
	case "message":
		return &g.Message
	case "fail-when":
		return &g.FailWhen
	}
	return nil
}

// appendRule parses one guard and files it under its mode.
func appendRule(g *Grant, c *kdl.Node) error {
	rule, err := parseRule(c)
	if err != nil {
		return err
	}
	switch c.Name() {
	case "allow":
		g.Allow = append(g.Allow, rule)
	case "deny":
		g.Deny = append(g.Deny, rule)
	default:
		g.PostCall = append(g.PostCall, rule)
	}
	return nil
}

// validate rejects a guardfile that parsed but cannot mount.
func (gf *Guardfile) validate() error {
	if err := gf.validateServer(); err != nil {
		return err
	}
	return gf.validateGrants()
}

// validateServer checks the transport declaration is complete.
func (gf *Guardfile) validateServer() error {
	switch {
	case gf.Server.Kind == "":
		return fmt.Errorf("mcpverb: wrap has no `mcp` block; declare `mcp stdio { ... }` or `mcp http { ... }`")
	case gf.Server.Kind == "stdio" && gf.Server.Command == "":
		return fmt.Errorf("mcpverb: `mcp stdio` needs a `command`")
	case gf.Server.Kind == "http" && gf.Server.URL == "" && gf.Server.URLValue.IsZero():
		return fmt.Errorf("mcpverb: `mcp http` needs a `url`")
	}
	return nil
}

// validateGrants rejects a duplicate sentence and a tool that is both granted
// and denied. The contradiction is surfaced, never resolved by picking one.
func (gf *Guardfile) validateGrants() error {
	seen := map[string]bool{}
	denied := map[string]bool{}
	for _, g := range gf.Grants {
		key := g.Modal + " call " + g.Tool
		if seen[key] {
			return fmt.Errorf("mcpverb: duplicate grant `%s` (fail-closed)", key)
		}
		seen[key] = true
		if g.IsDeny() {
			denied[g.Tool] = true
		}
	}
	for _, g := range gf.Grants {
		if !g.IsDeny() && g.Tool != WildcardTool && denied[g.Tool] {
			return fmt.Errorf("mcpverb: tool %q is both granted and denied; drop one (fail-closed)", g.Tool)
		}
	}
	return nil
}

// Granted resolves the guardfile's policy against the tools the lock holds and
// returns the grants that mount, wildcard expanded and denials removed.
func (gf *Guardfile) Granted(tools []string) ([]Grant, error) {
	denied := map[string]bool{}
	for _, g := range gf.Grants {
		if g.IsDeny() {
			denied[g.Tool] = true
		}
	}
	// Explicit grants resolve before the wildcard expands, so a tool named
	// outright keeps its own guards no matter which sentence was authored first.
	out, explicit, err := gf.explicitGrants(tools)
	if err != nil {
		return nil, err
	}
	out = append(out, gf.wildcardGrants(tools, denied, explicit)...)
	return out, dupeLeaf(out)
}

// explicitGrants resolves every grant naming one tool, failing closed on a name
// the lock does not hold.
func (gf *Guardfile) explicitGrants(tools []string) ([]Grant, map[string]bool, error) {
	known := map[string]bool{}
	for _, t := range tools {
		known[t] = true
	}
	explicit := map[string]bool{}
	var out []Grant
	for _, g := range gf.Grants {
		if g.IsDeny() || g.Tool == WildcardTool {
			continue
		}
		if !known[g.Tool] {
			return nil, nil, fmt.Errorf("mcpverb: granted tool %q is not in the lock; run `umbra lock` or fix the name (fail-closed)", g.Tool)
		}
		explicit[g.Tool] = true
		out = append(out, g)
	}
	return out, explicit, nil
}

// wildcardGrants expands `can call *` over the lock, minus what is denied or
// already named. Over the lock, so a newly added tool does not join silently.
func (gf *Guardfile) wildcardGrants(tools []string, denied, explicit map[string]bool) []Grant {
	var out []Grant
	for _, g := range gf.Grants {
		if g.IsDeny() || g.Tool != WildcardTool {
			continue
		}
		for _, t := range tools {
			if denied[t] || explicit[t] {
				continue
			}
			expanded := g
			expanded.Tool = t
			expanded.Name = ""
			out = append(out, expanded)
		}
	}
	return out
}

// dupeLeaf fails closed when two grants would mount the same leaf, which two
// tools differing only in `_` versus `-` would otherwise do silently.
func dupeLeaf(grants []Grant) error {
	seen := map[string]string{}
	for _, g := range grants {
		leaf := g.LeafName()
		if prior, dup := seen[leaf]; dup {
			return fmt.Errorf("mcpverb: tools %q and %q both mount leaf %q; give one a `name` (fail-closed)", prior, g.Tool, leaf)
		}
		seen[leaf] = g.Tool
	}
	return nil
}

// parseRule reads one `<allow|deny|post-call> <field> matches "<regex>"...`
// node. Build checks the selector against the locked schema, not a vocabulary.
func parseRule(n *kdl.Node) (opcore.ProxyRule, error) {
	args := n.Arguments()
	if len(args) < 3 || args[1].String() != "matches" {
		return opcore.ProxyRule{}, fmt.Errorf("`%s` needs `<field> matches \"<regex>\"`, e.g. `%s owner matches \"^coilyco\"`", n.Name(), n.Name())
	}
	field := args[0].String()
	if field == "" {
		return opcore.ProxyRule{}, fmt.Errorf("`%s` needs a non-empty selector", n.Name())
	}
	rule := opcore.ProxyRule{Field: field}
	for _, a := range args[2:] {
		pat := a.String()
		if pat == "" {
			return opcore.ProxyRule{}, fmt.Errorf("`%s %s` has an empty regex (fail-closed)", n.Name(), field)
		}
		if _, err := regexp.Compile(pat); err != nil {
			// Caught here rather than at match time, where a malformed pattern
			// matches nothing and reads as a guard that passed.
			return opcore.ProxyRule{}, fmt.Errorf("`%s %s` regex %q does not compile: %w", n.Name(), field, pat, err)
		}
		rule.Patterns = append(rule.Patterns, pat)
	}
	if len(n.Children().Nodes) > 0 || len(n.Properties()) > 0 {
		return opcore.ProxyRule{}, fmt.Errorf("`%s` takes only positional args (fail-closed)", n.Name())
	}
	return rule, nil
}

// singleArg reads exactly one string argument off a node.
func singleArg(n *kdl.Node, label string) (string, error) {
	args := n.Arguments()
	if len(args) != 1 {
		return "", fmt.Errorf("`%s` takes exactly one value (fail-closed)", label)
	}
	return args[0].String(), nil
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
