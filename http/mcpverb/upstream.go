package mcpverb

import (
	"fmt"
	"net/url"

	kdl "github.com/calico32/kdl-go"
	"github.com/coilyco/umbra/http/guardfile"
)

// UpstreamNode is the top-level node of an upstream guardfile, a sibling of
// `wrap` rather than a form of it. See docs/mcpverb-upstream.md.
const UpstreamNode = "mcp-upstream"

// UpstreamTransport is the one transport a proxied upstream speaks, so
// `transport` is a statement rather than a choice.
const UpstreamTransport = "streamable-http"

// Shape is which top-level node a guardfile opens with, so a consumer picks a
// parser rather than handing a file to one that reads it as something else.
type Shape string

// The two guardfile shapes: a wrapped command path, or a fronted MCP upstream.
const (
	ShapeCommand  Shape = "command"
	ShapeUpstream Shape = "upstream"
)

// Upstream is a parsed `mcp-upstream` guardfile: policy about a network
// upstream, and never its tool schemas. See docs/mcpverb-upstream.md.
type Upstream struct {
	// Name is the node argument, the registry name by convention.
	Name string

	// Description is the optional sibling `description "..."` prose, the same
	// node the other dialects take.
	Description string

	// URL is the streamable-HTTP endpoint.
	URL string

	// Transport is UpstreamTransport, stated or defaulted.
	Transport string

	// Coverage is nil when the guardfile states no annotation-coverage marker.
	Coverage *AnnotationCoverage

	// Auth is the credential this upstream presents, in the guardfile-wide auth
	// grammar. Zero when the file names none.
	Auth guardfile.Auth

	// Tools is the allowlist in stated order. Empty is a valid statement: a
	// guardfile that exposes nothing still says where the upstream is.
	Tools []string
}

// AnnotationCoverage records whether the upstream declares `readOnlyHint` on
// every tool, some, or none: the fact deciding what may safely be allowed.
type AnnotationCoverage struct {
	// Kind is declared, partial, or undeclared.
	Kind string
	// Annotated counts tools carrying a readOnlyHint either way.
	Annotated int
	// Silent counts tools declaring nothing.
	Silent int
}

// Providers returns the distinct value-source provider names the upstream's
// credential names, so a driver wires exactly the resolvers in play.
func (u *Upstream) Providers() []string {
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
	add(u.Auth.Value)
	for _, p := range u.Auth.Params {
		add(p.Value)
	}
	return out
}

// Classify reports which shape guardfile source carries, without parsing either
// body. Both nodes, or neither, is an error: there is no safe default.
func Classify(src []byte) (Shape, error) {
	doc, err := kdl.ParseString(string(src))
	if err != nil {
		return "", fmt.Errorf("mcpverb: parse KDL: %w", err)
	}
	hasWrap := doc.GetNode("wrap") != nil
	hasUpstream := doc.GetNode(UpstreamNode) != nil
	switch {
	case hasWrap && hasUpstream:
		return "", fmt.Errorf("mcpverb: a guardfile carries `wrap` or `%s`, not both (fail-closed)", UpstreamNode)
	case hasUpstream:
		return ShapeUpstream, nil
	case hasWrap:
		return ShapeCommand, nil
	default:
		return "", fmt.Errorf("mcpverb: missing top-level `wrap` or `%s` node", UpstreamNode)
	}
}

// ParseUpstream reads an `mcp-upstream` guardfile. Nodes beside it and
// `description` are the consumer's. See docs/mcpverb-upstream.md.
func ParseUpstream(src []byte) (*Upstream, error) {
	doc, err := kdl.ParseString(string(src))
	if err != nil {
		return nil, fmt.Errorf("mcpverb: parse KDL: %w", err)
	}
	n := doc.GetNode(UpstreamNode)
	if n == nil {
		return nil, fmt.Errorf("mcpverb: missing top-level `%s` node", UpstreamNode)
	}
	if doc.GetNode("wrap") != nil {
		return nil, fmt.Errorf("mcpverb: a guardfile carries `wrap` or `%s`, not both (fail-closed)", UpstreamNode)
	}
	up := &Upstream{Transport: UpstreamTransport}
	if err := upstreamHeader(n, up); err != nil {
		return nil, err
	}
	if err := upstreamDescription(doc, up); err != nil {
		return nil, err
	}
	if err := upstreamBody(n, up); err != nil {
		return nil, err
	}
	if up.URL == "" {
		return nil, fmt.Errorf("mcpverb: `%s` needs a `url` naming the streamable-HTTP endpoint", UpstreamNode)
	}
	return up, nil
}

// upstreamHeader reads the node's own argument list.
func upstreamHeader(n *kdl.Node, up *Upstream) error {
	name, err := singleArg(n, UpstreamNode)
	if err != nil {
		return fmt.Errorf("mcpverb: `%s` wants exactly one name, as `%s \"<registry-name>\"`: %w", UpstreamNode, UpstreamNode, err)
	}
	if name == "" {
		return fmt.Errorf("mcpverb: `%s` name must be non-empty", UpstreamNode)
	}
	if len(n.Properties()) > 0 {
		return fmt.Errorf("mcpverb: `%s` takes no properties (fail-closed)", UpstreamNode)
	}
	up.Name = name
	return nil
}

// upstreamDescription reads the optional sibling `description "..."`.
func upstreamDescription(doc *kdl.Document, up *Upstream) error {
	d := doc.GetNode("description")
	if d == nil {
		return nil
	}
	v, err := singleArg(d, "description")
	if err != nil {
		return fmt.Errorf("mcpverb: %w", err)
	}
	if v == "" {
		return fmt.Errorf("mcpverb: `description` must be a non-empty string (fail-closed)")
	}
	up.Description = v
	return nil
}

// upstreamBody reads the node's children, refusing a repeat of any single-value
// field so a second one cannot silently win.
func upstreamBody(n *kdl.Node, up *Upstream) error {
	seen := map[string]bool{}
	tools := map[string]bool{}
	for _, c := range n.Children().Nodes {
		name := c.Name()
		if name != "can" {
			if seen[name] {
				return fmt.Errorf("mcpverb: duplicate `%s` in `%s` (fail-closed)", name, UpstreamNode)
			}
			seen[name] = true
		}
		if err := applyUpstreamChild(c, up, tools); err != nil {
			return err
		}
	}
	return nil
}

// applyUpstreamChild dispatches one child of the upstream body.
func applyUpstreamChild(c *kdl.Node, up *Upstream, tools map[string]bool) error {
	switch c.Name() {
	case "url":
		return applyUpstreamURL(c, up)
	case "transport":
		return applyUpstreamTransport(c, up)
	case "annotation-coverage":
		cov, err := parseAnnotationCoverage(c)
		if err != nil {
			return err
		}
		up.Coverage = cov
		return nil
	case "auth":
		a, err := guardfile.ParseAuthNode(c)
		if err != nil {
			return fmt.Errorf("mcpverb: %w", err)
		}
		up.Auth = a
		return nil
	case "can":
		return applyUpstreamTool(c, up, tools)
	case "inherit":
		// Inheriting half an upstream declaration would serve wider than either
		// file reads, and flattening this shape is unreconciled. See umbra#1036.
		return fmt.Errorf("mcpverb: `inherit` is not supported in `%s` (fail-closed)", UpstreamNode)
	default:
		return fmt.Errorf("mcpverb: unknown node %q in `%s` (want url | transport | annotation-coverage | auth | can; fail-closed)", c.Name(), UpstreamNode)
	}
}

// applyUpstreamURL reads and validates the endpoint.
func applyUpstreamURL(c *kdl.Node, up *Upstream) error {
	raw, err := singleArg(c, "url")
	if err != nil {
		return fmt.Errorf("mcpverb: %w", err)
	}
	parsed, perr := url.Parse(raw)
	if perr != nil {
		return fmt.Errorf("mcpverb: `url %q` does not parse: %w", raw, perr)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("mcpverb: `url %q` must be an absolute http or https URL", raw)
	}
	up.URL = raw
	return nil
}

// applyUpstreamTransport refuses a transport no consumer can honour, rather
// than carrying the word through to one that then speaks a different protocol.
func applyUpstreamTransport(c *kdl.Node, up *Upstream) error {
	got, err := singleArg(c, "transport")
	if err != nil {
		return fmt.Errorf("mcpverb: %w", err)
	}
	if got != UpstreamTransport {
		return fmt.Errorf("mcpverb: `transport %s` is not served; a fronted upstream speaks %s only (fail-closed)", got, UpstreamTransport)
	}
	up.Transport = got
	return nil
}

// applyUpstreamTool reads one `can "<tool>"` sentence: a bare string, because
// there is no leaf to name and no guard to hang.
func applyUpstreamTool(c *kdl.Node, up *Upstream, tools map[string]bool) error {
	args := c.Arguments()
	if len(args) != 1 {
		return fmt.Errorf("mcpverb: `can` in `%s` takes one bare tool name, as `can \"search_docs\"` (fail-closed)", UpstreamNode)
	}
	tool := args[0].String()
	if tool == "" {
		return fmt.Errorf("mcpverb: `can` needs a non-empty tool name")
	}
	if len(c.Properties()) > 0 || len(c.Children().Nodes) > 0 {
		return fmt.Errorf("mcpverb: `can %q` takes no properties or children: the contract stays upstream (fail-closed)", tool)
	}
	if tools[tool] {
		return fmt.Errorf("mcpverb: duplicate `can %q` (fail-closed)", tool)
	}
	tools[tool] = true
	up.Tools = append(up.Tools, tool)
	return nil
}

// parseAnnotationCoverage reads the marker and checks it against itself, so a
// hand edit cannot state `declared` beside a non-zero silent count.
func parseAnnotationCoverage(n *kdl.Node) (*AnnotationCoverage, error) {
	kind, err := singleArg(n, "annotation-coverage")
	if err != nil {
		return nil, fmt.Errorf("mcpverb: %w", err)
	}
	if len(n.Children().Nodes) > 0 {
		return nil, fmt.Errorf("mcpverb: `annotation-coverage` takes no children (fail-closed)")
	}
	out := &AnnotationCoverage{Kind: kind}
	have := map[string]bool{}
	for key, value := range n.Properties() {
		count, cerr := countProp(value, key)
		if cerr != nil {
			return nil, cerr
		}
		switch key {
		case "annotated":
			out.Annotated = count
		case "silent":
			out.Silent = count
		default:
			return nil, fmt.Errorf("mcpverb: unknown `annotation-coverage` property %q (want annotated, silent)", key)
		}
		have[key] = true
	}
	if !have["annotated"] || !have["silent"] {
		return nil, fmt.Errorf("mcpverb: `annotation-coverage` wants both annotated= and silent= counts")
	}
	return out, out.validate()
}

// validate refuses a marker that contradicts its own counts.
func (c AnnotationCoverage) validate() error {
	switch c.Kind {
	case "declared":
		if c.Silent != 0 || c.Annotated == 0 {
			return fmt.Errorf("mcpverb: `annotation-coverage declared` wants silent=0 and annotated>0, got annotated=%d silent=%d", c.Annotated, c.Silent)
		}
	case "undeclared":
		if c.Annotated != 0 {
			return fmt.Errorf("mcpverb: `annotation-coverage undeclared` wants annotated=0, got annotated=%d", c.Annotated)
		}
	case "partial":
		if c.Annotated == 0 || c.Silent == 0 {
			return fmt.Errorf("mcpverb: `annotation-coverage partial` wants both counts above zero, got annotated=%d silent=%d", c.Annotated, c.Silent)
		}
	default:
		return fmt.Errorf("mcpverb: unknown `annotation-coverage` kind %q (want declared, partial, undeclared)", c.Kind)
	}
	return nil
}

// countProp reads a whole-number property, refusing a string that merely looks
// like one.
func countProp(value kdl.Value, key string) (int, error) {
	if value.Kind() != kdl.Int {
		return 0, fmt.Errorf("mcpverb: `annotation-coverage` property %q wants a whole number, got %s", key, value.String())
	}
	count := value.Int()
	if count < 0 {
		return 0, fmt.Errorf("mcpverb: `annotation-coverage` property %q must not be negative", key)
	}
	return count, nil
}
