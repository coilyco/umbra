package mcpverb

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/mcpapps"
	"github.com/coilyco/umbra/pkg/mcpclient"
)

// WidgetGate is one granted tool's `widget` block resolved against the lock: the
// mcpapps.Policy a host consults before forwarding. See docs/mcpapps.md.
type WidgetGate struct {
	// tool is the instantiating tool whose view this gates, named in refusals.
	tool string

	// surface is what the view may see and call, deny by absence.
	surface []mcpclient.Tool

	// guards are the per-tool argument rules, keyed by upstream tool name.
	guards map[string]*opcore.MCPCall

	// reads, opens, and saves are the compiled URI sentences, one set per verb.
	reads []compiledRule
	opens []compiledRule
	saves []compiledRule

	// connects are the CSP source expressions the view may reach. Not a
	// compiledRule: CSP is an allowlist and takes no regex.
	connects []string

	// declared records whether the grant authored a `widget` block at all, so
	// the refusal names the missing block rather than a missing grant.
	declared bool
}

// The gate is what the host consults, so the seam is asserted at compile time
// rather than discovered when a view's first call arrives.
var _ mcpapps.Policy = (*WidgetGate)(nil)

// compiledRule is one URI rule with its regex already built, so a match costs
// no compile and a malformed pattern was rejected at parse.
type compiledRule struct {
	deny bool
	re   *regexp.Regexp
	raw  string
}

// WidgetPolicy resolves the `widget` block of one granted tool into the policy
// the host consults, failing closed at build time. See docs/mcpapps.md.
func WidgetPolicy(cfg Config, tool string) (*WidgetGate, error) {
	if cfg.Guardfile == nil {
		return nil, fmt.Errorf("mcpverb: nil Guardfile")
	}
	byName := map[string]mcpclient.Tool{}
	names := make([]string, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		byName[t.Name] = t
		names = append(names, t.Name)
	}
	grants, err := cfg.Guardfile.Granted(names)
	if err != nil {
		return nil, err
	}
	host, ok := grantFor(grants, tool)
	if !ok {
		return nil, fmt.Errorf("mcpverb: tool %q is not granted, so it has no view to gate (fail-closed)", tool)
	}
	if host.Widget == nil {
		return &WidgetGate{tool: tool, guards: map[string]*opcore.MCPCall{}}, nil
	}
	return buildGate(tool, host.Widget, byName)
}

// grantFor finds the resolved grant naming one tool.
func grantFor(grants []Grant, tool string) (Grant, bool) {
	for _, g := range grants {
		if g.Tool == tool {
			return g, true
		}
	}
	return Grant{}, false
}

// buildGate resolves a widget block against the lock, refusing a contradiction
// rather than picking a side.
func buildGate(tool string, w *Widget, byName map[string]mcpclient.Tool) (*WidgetGate, error) {
	denied := map[string]bool{}
	for _, g := range w.Grants {
		if g.IsDeny() {
			denied[g.Tool] = true
		}
	}
	gate := &WidgetGate{tool: tool, guards: map[string]*opcore.MCPCall{}, declared: true}
	seen := map[string]bool{}
	for _, g := range w.Grants {
		if g.IsDeny() {
			continue
		}
		if denied[g.Tool] {
			return nil, fmt.Errorf("mcpverb: widget of %s: tool %q is both granted and denied; drop one (fail-closed)", tool, g.Tool)
		}
		if seen[g.Tool] {
			return nil, fmt.Errorf("mcpverb: widget of %s: duplicate `can call %s` (fail-closed)", tool, g.Tool)
		}
		seen[g.Tool] = true
		called, held := byName[g.Tool]
		if !held {
			return nil, fmt.Errorf("mcpverb: widget of %s: view call %q is not in the lock; run `umbra lock` or fix the name (fail-closed)", tool, g.Tool)
		}
		if err := checkWidgetSelectors(tool, g, called); err != nil {
			return nil, err
		}
		gate.surface = append(gate.surface, called)
		gate.guards[g.Tool] = &opcore.MCPCall{Tool: g.Tool, Allow: g.Allow, Deny: g.Deny}
	}
	sort.Slice(gate.surface, func(i, j int) bool { return gate.surface[i].Name < gate.surface[j].Name })
	for _, set := range []struct {
		rules []URIRule
		dst   *[]compiledRule
	}{{w.Reads, &gate.reads}, {w.Opens, &gate.opens}, {w.Saves, &gate.saves}} {
		compiled, err := compileRules(tool, set.rules)
		if err != nil {
			return nil, err
		}
		*set.dst = compiled
	}
	gate.connects = append(gate.connects, w.Connects...)
	return gate, nil
}

// CSPSources is what the page's policy is built from. Only connect-src has a
// verb today, so every other family stays at the spec's floor.
func (w *WidgetGate) CSPSources() mcpapps.CSPSources {
	return mcpapps.CSPSources{Connect: w.connects}
}

// compileRules builds the regexes for one verb's sentences. Parse compiled them
// already, so the error is kept rather than dropped only as a backstop.
func compileRules(tool string, rules []URIRule) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("mcpverb: widget of %s: %s regex %q does not compile: %w", tool, r.Verb, r.Pattern, err)
		}
		out = append(out, compiledRule{deny: r.IsDeny(), re: re, raw: r.Pattern})
	}
	return out, nil
}

// checkWidgetSelectors refuses a view guard naming an argument the called tool
// does not have, against that tool's schema rather than the instantiating one's.
func checkWidgetSelectors(host string, g Grant, called mcpclient.Tool) error {
	fields, err := FieldsFor(called)
	if err != nil {
		return fmt.Errorf("mcpverb: widget of %s: %w", host, err)
	}
	if err := checkSelectors(g, fields); err != nil {
		return fmt.Errorf("mcpverb: widget of %s: %w", host, err)
	}
	return nil
}

// Tools is the surface this view may see and call.
func (w *WidgetGate) Tools() []mcpclient.Tool {
	out := make([]mcpclient.Tool, len(w.surface))
	copy(out, w.surface)
	return out
}

// CheckToolCall applies the view call's own allow and deny guards, through the
// same rule engine a CLI leaf uses so the two cannot drift.
func (w *WidgetGate) CheckToolCall(tool string, args map[string]any) error {
	call, ok := w.guards[tool]
	if !ok {
		if !w.declared {
			return fmt.Errorf("the grant for %s declares no `widget` block, so its view calls nothing", w.tool)
		}
		return fmt.Errorf("tool %q is not granted to the view of %s; add `can call %s` to its `widget` block", tool, w.tool, tool)
	}
	return opcore.CheckProxyRules(call.Allow, call.Deny, args)
}

// CheckResourceRead applies the read rules: a `never read` match refuses, and so
// does a URI no `can read` matches. Deny by absence, like every guard here.
func (w *WidgetGate) CheckResourceRead(uri string) error {
	return w.checkURI("read", "readable", uri, w.reads)
}

// CheckOpenLink applies the `can open` rules. Opening a URL an untrusted view
// chose is its own grant rather than a consequence of being able to read.
func (w *WidgetGate) CheckOpenLink(url string) error {
	return w.checkURI("open", "openable", url, w.opens)
}

// CheckSaveFile applies the `can save` rules, over the URI of a link and of an
// inline resource alike.
func (w *WidgetGate) CheckSaveFile(uri string) error {
	return w.checkURI("save", "savable", uri, w.saves)
}

// checkURI is the one direction every URI verb reads: a deny match refuses, and
// so does anything no allow matches.
func (w *WidgetGate) checkURI(verb, adjective, uri string, rules []compiledRule) error {
	for _, r := range rules {
		if r.deny && r.re.MatchString(uri) {
			return fmt.Errorf("%q is denied to the view of %s (never %s %q)", uri, w.tool, verb, r.raw)
		}
	}
	for _, r := range rules {
		if !r.deny && r.re.MatchString(uri) {
			return nil
		}
	}
	if !w.declared {
		return fmt.Errorf("the grant for %s declares no `widget` block, so its view %ss nothing", w.tool, verb)
	}
	return fmt.Errorf("%q is not %s by the view of %s; add `can %s` to its `widget` block%s",
		uri, adjective, w.tool, verb, allowedPatterns(rules))
}

// allowedPatterns names the patterns that would have permitted it, so the
// refusal says what to widen rather than only that it refused.
func allowedPatterns(rules []compiledRule) string {
	var allowed []string
	for _, r := range rules {
		if !r.deny {
			allowed = append(allowed, fmt.Sprintf("%q", r.raw))
		}
	}
	if len(allowed) == 0 {
		return " (it declares none)"
	}
	return " (it permits " + strings.Join(allowed, ", ") + ")"
}
