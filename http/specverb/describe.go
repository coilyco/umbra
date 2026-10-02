// The describe model: one in-engine view of the mounted surface, the shared
// source for rich per-verb help and the `describe` verb. See docs/specverb-describe.md.

package specverb

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/urfave/cli/v3"
)

// Surface is the in-engine model of a mounted command surface: the structural
// truth shared by help, the describe verb, and (later) completions and the skill.
type Surface struct {
	Group       []string          `json:"group"`                 // command path, e.g. ["ward","ops","forgejo"]
	Description string            `json:"description,omitempty"` // the guardfile's top-level `description` prose, "" when none
	BaseURL     string            `json:"base_url"`              // resolved request base, scheme defaulted
	Auth        AuthInfo          `json:"auth"`                  // how the engine authenticates
	Verbs       []VerbInfo        `json:"verbs"`                 // every mounted leaf, in mount order
	Fetches     []FetchInfo       `json:"fetches,omitempty"`     // HTTP fetch overlays, in declaration order
	Actions     []ActionInfo      `json:"actions,omitempty"`     // complex actions, in declaration order
	Denied      []DenyInfo        `json:"denied,omitempty"`      // blocked classes, in declaration order
	Restrict    []RestrictionInfo `json:"restrict,omitempty"`    // wrap-level scope allowlists
}

// FetchInfo is one mounted HTTP overlay: the fixed method/path, output mode,
// env-backed headers, and the positional guards that gate invocation.
type FetchInfo struct {
	Name     string            `json:"name"`               // dotted audit name, e.g. ward.ops.forgejo.fetch.actions-logs
	Leaf     string            `json:"leaf"`               // CLI leaf, e.g. actions-logs
	Title    string            `json:"title"`              // author-supplied fetch label
	Describe string            `json:"describe,omitempty"` // optional human note
	Method   string            `json:"method"`             // HTTP method
	Path     string            `json:"path"`               // path template
	Output   string            `json:"output"`             // raw
	Params   []ParamInfo       `json:"params,omitempty"`   // positional path args, in order
	Env      []FetchEnvInfo    `json:"env,omitempty"`      // env value sources, never resolved values
	Headers  []FetchHeaderInfo `json:"headers,omitempty"`  // literal headers with env templates
	Whens    []FetchWhenInfo   `json:"whens,omitempty"`    // glob guards on the positional inputs
}

// FetchEnvInfo is one fetch env source, rendered as the value chain rather than
// the resolved secret.
type FetchEnvInfo struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// FetchHeaderInfo is one fetch header, rendered with its template intact.
type FetchHeaderInfo struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// FetchWhenInfo is one fetch guard clause.
type FetchWhenInfo struct {
	Selector string   `json:"selector"`
	Globs    []string `json:"globs"`
}

// RestrictionInfo is one wrap-level restrict clause for the describe surface: the
// path param it gates and the globs an argument must match.
type RestrictionInfo struct {
	Param string   `json:"param"`
	Globs []string `json:"globs"`
}

// DenyInfo is one blocked (verb,resource) class for the describe surface: its CLI
// placement and the teaching message an operator sees when they reach for it.
type DenyInfo struct {
	Name    string `json:"name"`    // dotted audit name, e.g. ward.ops.forgejo.orgs.create
	Group   string `json:"group"`   // CLI noun, e.g. orgs
	Leaf    string `json:"leaf"`    // CLI verb, e.g. create
	Message string `json:"message"` // the teaching error
}

// ActionInfo is one mounted complex action for the describe surface: its
// envelope name, the polled leaf, the bounds, and the conditions.
type ActionInfo struct {
	Name     string `json:"name"`                // envelope audit name, e.g. ward.ops.forgejo.action.ci-watch
	Leaf     string `json:"leaf"`                // CLI leaf, e.g. ci-watch
	Describe string `json:"describe,omitempty"`  // optional human note
	Method   string `json:"method"`              // the polled leaf's HTTP method
	Path     string `json:"path"`                // the polled leaf's path template
	Grant    string `json:"grant"`               // the grant that authorizes the polled leaf
	Every    string `json:"every"`               // sample interval
	Timeout  string `json:"timeout"`             // wall-clock bound
	Until    string `json:"until"`               // loop-ending JMESPath
	FailWhen string `json:"fail_when,omitempty"` // non-zero-exit JMESPath

	// Defaults is the pre-flight input bindings: each absent input resolved from
	// the polled leaf before the loop starts. Empty when the action declares none.
	Defaults []ActionDefaultInfo `json:"defaults,omitempty"`

	// MountVerb/MountResource: set when the action shadows a leaf path (mounts at
	// `<resource> <verb>`, not the `action` noun). Empty for a named action.
	MountVerb     string `json:"mount_verb,omitempty"`
	MountResource string `json:"mount_resource,omitempty"`

	// Calls is the resolved step sequence for a multi-call action; empty for a poll.
	Calls []ActionCallInfo `json:"calls,omitempty"`

	// Collect describes an auto-pagination action; nil for poll/call actions.
	Collect *ActionCollectInfo `json:"collect,omitempty"`
}

// ActionDefaultInfo is one input `default` pre-flight binding for the describe
// surface: the input it resolves and the JMESPath evaluated against the poll leaf.
type ActionDefaultInfo struct {
	Input    string `json:"input"`
	JMESPath string `json:"jmespath"`
}

// ActionCallInfo is one step of a multi-call action for the describe surface.
type ActionCallInfo struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Grant  string `json:"grant"`
	As     string `json:"as,omitempty"`
}

// ActionCollectInfo is the resolved auto-pagination leaf for the describe surface.
type ActionCollectInfo struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	Grant        string `json:"grant"`
	PageParam    string `json:"page_param"`
	LimitParam   string `json:"limit_param"`
	DefaultLimit int    `json:"default_limit"`
	As           string `json:"as"`
	// Cache is the on-disk TTL (e.g. "10m") when the action declares a
	// `cache "<ttl>"` modifier; empty when the collect does not cache.
	Cache string `json:"cache,omitempty"`
}

// AuthInfo is the auth scope a describe consumer sees: scheme, header, and the
// value source (provider + address). The secret value itself never appears here.
type AuthInfo struct {
	Scheme string `json:"scheme"`
	Header string `json:"header"`
	Source string `json:"source"` // value source (provider + address), not the secret
}

// VerbInfo is one mounted leaf: its CLI placement, the HTTP op it drives, its
// destructive flag, the grant that authorized it, and the optional human note.
type VerbInfo struct {
	Name        string         `json:"name"`                 // dotted audit name, e.g. ward.ops.forgejo.repo.create
	Group       string         `json:"group"`                // CLI noun, e.g. repo
	Leaf        string         `json:"leaf"`                 // CLI verb, e.g. create
	Method      string         `json:"method"`               // HTTP method
	Path        string         `json:"path"`                 // path template
	Destructive bool           `json:"destructive"`          // mutates irreversibly
	Grant       string         `json:"grant"`                // authorizing grant sentence
	Describe    string         `json:"describe,omitempty"`   // Guardfile describe "..." note
	Params      []ParamInfo    `json:"params,omitempty"`     // path/query/body params, in invocation order
	FixedBody   map[string]any `json:"fixed_body,omitempty"` // exact body a state-toggle leaf sends
}

// ParamInfo is one input to a verb, tagged by kind so help can always show the
// structure the engine knows even where the upstream spec carries no description.
type ParamInfo struct {
	Name         string `json:"name"`
	UpstreamName string `json:"upstream_name,omitempty"` // aliased outgoing query parameter
	Kind         string `json:"kind"`                    // "path" | "query" | "body" | "form"
	Type         string `json:"type"`                    // swagger type, arrays render as []elem
	Required     bool   `json:"required"`                // path params and required-schema fields
	Desc         string `json:"desc,omitempty"`          // upstream spec description, often blank
}

// Describe builds the surface model for cfg without mounting a command tree, so a
// caller (skill, completions, docs) can read it directly. Fails closed like Build.
func Describe(cfg Config) (*Surface, error) {
	if cfg.Guardfile == nil {
		return nil, fmt.Errorf("specverb: Config.Guardfile is nil")
	}
	gf := cfg.Guardfile
	if len(gf.Group) == 0 {
		return nil, fmt.Errorf("specverb: Guardfile has no command group")
	}
	fetchDescs, err := resolveFetchDescriptors(gf)
	if err != nil {
		return nil, err
	}
	var descs []opDescriptor
	var actionDescs []actionDescriptor
	hasSpecDriven := len(gf.Grants) > 0 || len(gf.Actions) > 0 || len(gf.Restrict) > 0
	if hasSpecDriven {
		spec, err := parseSwagger(cfg.Spec)
		if err != nil {
			return nil, err
		}
		gf, err = expandWildcards(spec, gf)
		if err != nil {
			return nil, err
		}
		descs, err = resolveDescriptors(spec, gf)
		if err != nil {
			return nil, err
		}
		actionDescs, err = resolveActions(spec, gf, grantedGrants(gf))
		if err != nil {
			return nil, err
		}
		// Match Build: a mount action shadows its generated leaf, so the surface
		// must drop that leaf too (the action stands in for it).
		mountActions, _ := splitMountActions(actionDescs)
		descs = suppressShadowed(descs, mountActions)
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = gf.BaseURL
	}
	display := baseURLDisplay(gf, opcore.DefaultScheme(strings.TrimRight(baseURL, "/")))
	return buildSurface(gf, display, descs, actionDescs, fetchDescs), nil
}

// buildSurface assembles the model from the already-resolved descriptors, so the
// description can never name a verb the runtime did not mount.
func buildSurface(gf *guardfile.Guardfile, baseURL string, descs []opDescriptor, actions []actionDescriptor, fetches []fetchDescriptor) *Surface {
	s := &Surface{
		Group:       gf.Group,
		Description: gf.Description,
		BaseURL:     baseURL,
		Auth:        AuthInfo{Scheme: gf.Auth.Scheme, Header: gf.Auth.Header, Source: authSourceDisplay(gf.Auth)},
	}
	s.Verbs = verbInfosOf(descs)
	s.Fetches = fetchInfosOf(fetches)
	s.Actions = actionInfosOf(actions)
	s.Denied = denyInfosOf(gf)
	s.Restrict = restrictInfosOf(gf)
	return s
}

func verbInfosOf(descs []opDescriptor) []VerbInfo {
	var out []VerbInfo
	for _, d := range descs {
		out = append(out, VerbInfo{
			Name:        d.VerbName,
			Group:       d.Group,
			Leaf:        d.Leaf,
			Method:      d.Method,
			Path:        d.Path,
			Destructive: d.Destructive,
			Grant:       d.Grant,
			Describe:    d.Describe,
			Params:      paramsOf(d),
			FixedBody:   d.FixedBody,
		})
	}
	return out
}

func fetchInfosOf(fetches []fetchDescriptor) []FetchInfo {
	var out []FetchInfo
	for _, f := range fetches {
		out = append(out, fetchInfoOf(f))
	}
	return out
}

func actionInfosOf(actions []actionDescriptor) []ActionInfo {
	var out []ActionInfo
	for _, a := range actions {
		info := ActionInfo{Name: a.VerbName, Leaf: a.Name, Describe: a.Describe, FailWhen: a.FailWhen, MountVerb: a.MountVerb, MountResource: a.MountResource}
		switch {
		case a.isCall():
			info = addCallInfo(info, a)
		case a.isCollect():
			info = addCollectInfo(info, a)
		default:
			info = addLeafActionInfo(info, a)
		}
		out = append(out, info)
	}
	return out
}

func addCallInfo(info ActionInfo, a actionDescriptor) ActionInfo {
	for _, step := range a.Calls {
		op := leafOp(step.Leaf)
		info.Calls = append(info.Calls, ActionCallInfo{Method: op.Method, Path: op.Path, Grant: op.Grant, As: step.As})
	}
	return info
}

func addCollectInfo(info ActionInfo, a actionDescriptor) ActionInfo {
	info.Collect = &ActionCollectInfo{
		Method:       a.Collect.Leaf.Method,
		Path:         a.Collect.Leaf.Path,
		Grant:        a.Collect.Leaf.Grant,
		PageParam:    a.Collect.PageParam,
		LimitParam:   a.Collect.LimitParam,
		DefaultLimit: a.Collect.DefaultLimit,
		As:           a.Collect.As,
		Cache:        collectCacheLabel(a.Collect.CacheTTL),
	}
	return info
}

func addLeafActionInfo(info ActionInfo, a actionDescriptor) ActionInfo {
	info.Method, info.Path, info.Grant = a.Leaf.Method, a.Leaf.Path, a.Leaf.Grant
	info.Every, info.Timeout, info.Until = a.Every.String(), a.Timeout.String(), a.Until
	for _, in := range defaultedInputs(a) {
		info.Defaults = append(info.Defaults, ActionDefaultInfo{Input: in.Name, JMESPath: in.Default})
	}
	return info
}

func denyInfosOf(gf *guardfile.Guardfile) []DenyInfo {
	var out []DenyInfo
	for _, d := range denyDescriptors(gf) {
		out = append(out, DenyInfo{Name: d.VerbName, Group: d.Group, Leaf: d.Leaf, Message: d.Message})
	}
	return out
}

func restrictInfosOf(gf *guardfile.Guardfile) []RestrictionInfo {
	var out []RestrictionInfo
	for _, r := range gf.Restrict {
		out = append(out, RestrictionInfo{Param: r.Param, Globs: r.Globs})
	}
	return out
}

func fetchInfoOf(f fetchDescriptor) FetchInfo {
	info := FetchInfo{
		Name:     f.VerbName,
		Leaf:     f.Leaf,
		Title:    f.Name,
		Describe: f.Describe,
		Method:   f.Method,
		Path:     f.Path,
		Output:   f.Output,
	}
	for _, p := range f.PathParams {
		info.Params = append(info.Params, ParamInfo{Name: p, Kind: "path", Type: "string", Required: true})
	}
	for _, e := range f.Env {
		info.Env = append(info.Env, FetchEnvInfo{Name: e.Name, Source: e.Value.String()})
	}
	for _, h := range f.Headers {
		info.Headers = append(info.Headers, FetchHeaderInfo{Name: h.Name, Value: h.Value})
	}
	for _, w := range f.Whens {
		info.Whens = append(info.Whens, FetchWhenInfo{Selector: w.Selector, Globs: w.Globs})
	}
	return info
}

// paramsOf flattens path params (positional, required), query flags, and body
// flags into one tagged list, path params first to match invocation order.
func paramsOf(d opDescriptor) []ParamInfo {
	var params []ParamInfo
	for _, p := range d.PathParams {
		params = append(params, ParamInfo{Name: p, Kind: "path", Type: "string", Required: true})
	}
	for _, f := range d.QueryFlags {
		params = append(params, ParamInfo{Name: f.Name, UpstreamName: f.UpstreamName, Kind: "query", Type: f.TypeLabel(), Required: f.Required, Desc: f.Desc})
	}
	for _, f := range d.BodyFlags {
		params = append(params, ParamInfo{Name: f.Name, Kind: "body", Type: f.TypeLabel(), Required: f.Required, Desc: f.Desc})
	}
	for _, f := range d.FormFlags {
		params = append(params, ParamInfo{Name: f.Name, Kind: "form", Type: f.TypeLabel(), Required: f.Required, Desc: f.Desc})
	}
	return params
}

// buildDescribeLeaf mounts `describe` as a stdout-only reference verb; the build
// emits the committed doc beside the Guardfile. See docs/specverb-describe.md.
func (rt *runtime) buildDescribeLeaf(gf *guardfile.Guardfile, surface *Surface) *cli.Command {
	name := strings.Join(gf.Group, ".") + ".describe"
	return &cli.Command{
		Name:  "describe",
		Usage: "print the mounted surface as a readable reference",
		Action: rt.wrap(verb.Spec{
			Name: name,
			Action: func(_ context.Context, _ *cli.Command) error {
				fmt.Print(surface.Markdown())
				return nil
			},
		}),
	}
}

// Markdown renders the pulled surface that the describe verb prints.
func (s *Surface) Markdown() string {
	return renderProse(s)
}

// renderProse renders the Surface as valid Markdown: header, auth sentence, a
// stanza per verb, each fact a blank-line-separated block. See docs/specverb-describe.md.
func renderProse(s *Surface) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", strings.Join(s.Group, " "))
	if s.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", s.Description)
	}
	fmt.Fprintf(&b, "Spec-driven CLI. Every verb issues an HTTP request against the API base %s.\n\n", s.BaseURL)
	fmt.Fprintf(&b, "%s\n", authSentence(s.Auth))

	prefix := strings.Join(s.Group, " ")
	for _, v := range s.Verbs {
		heading := fmt.Sprintf("## %s %s %s", prefix, v.Group, v.Leaf)
		if v.Describe != "" { // the Guardfile note adds intent the path can't; bare heading otherwise
			heading += " - " + v.Describe
		}
		fmt.Fprintf(&b, "\n%s\n\n", heading)
		fmt.Fprintf(&b, "`%s %s`\n\n", v.Method, v.Path)
		dest := "Not destructive."
		if v.Destructive {
			dest = "Destructive - mutates irreversibly."
		}
		fmt.Fprintf(&b, "Authorized by grant: %s. %s\n", v.Grant, dest)
		if line := fixedBodySentence(v.FixedBody); line != "" {
			fmt.Fprintf(&b, "\n%s\n", line)
		}
		writeParamSections(&b, v.Params)
	}
	writeFetches(&b, prefix, s.Fetches)
	writeActions(&b, prefix, s.Actions)
	writeRestrictions(&b, s.Restrict)
	writeDenied(&b, prefix, s.Denied)
	return b.String()
}

func writeFetches(b *strings.Builder, prefix string, fetches []FetchInfo) {
	for _, f := range fetches {
		writeFetch(b, prefix, f)
	}
}

func writeFetch(b *strings.Builder, prefix string, f FetchInfo) {
	writeFetchHeading(b, prefix, f)
	writeFetchSignature(b, f)
	writeFetchSummary(b)
	writeFetchLabel(b, f)
	writeFetchParams(b, f.Params)
	writeFetchEnv(b, f.Env)
	writeFetchHeaders(b, f.Headers)
	writeFetchGuards(b, f.Whens)
}

func writeFetchHeading(b *strings.Builder, prefix string, f FetchInfo) {
	heading := fmt.Sprintf("## %s fetch %s", prefix, f.Leaf)
	if f.Describe != "" {
		heading += " - " + f.Describe
	}
	fmt.Fprintf(b, "\n%s\n\n", heading)
}

func writeFetchSignature(b *strings.Builder, f FetchInfo) {
	fmt.Fprintf(b, "`%s %s`\n\n", f.Method, f.Path)
}

func writeFetchSummary(b *strings.Builder) {
	b.WriteString("Fetch overlay. Output is raw stdout.\n")
}

func writeFetchLabel(b *strings.Builder, f FetchInfo) {
	if f.Title != "" && f.Title != f.Leaf {
		fmt.Fprintf(b, "\nLabel: %s.\n", f.Title)
	}
}

func writeFetchParams(b *strings.Builder, params []ParamInfo) {
	if len(params) == 0 {
		return
	}
	b.WriteString("\nPositional arguments:\n\n")
	for _, p := range params {
		fmt.Fprintf(b, "- `<%s>` (%s)\n", p.Name, p.Type)
	}
}

func writeFetchEnv(b *strings.Builder, env []FetchEnvInfo) {
	if len(env) == 0 {
		return
	}
	b.WriteString("\nEnv values:\n\n")
	for _, e := range env {
		fmt.Fprintf(b, "- `%s` <- `%s`\n", e.Name, e.Source)
	}
}

func writeFetchHeaders(b *strings.Builder, headers []FetchHeaderInfo) {
	if len(headers) == 0 {
		return
	}
	b.WriteString("\nHeaders:\n\n")
	for _, h := range headers {
		fmt.Fprintf(b, "- `%s`: `%s`\n", h.Name, h.Value)
	}
}

func writeFetchGuards(b *strings.Builder, whens []FetchWhenInfo) {
	if len(whens) == 0 {
		return
	}
	b.WriteString("\nGuards:\n\n")
	for _, w := range whens {
		fmt.Fprintf(b, "- `%s` matches %s\n", w.Selector, strings.Join(w.Globs, " or "))
	}
}

// writeRestrictions renders the wrap-level scope allowlists: each gated param and
// the globs an argument must match, so the reference names the enforced scope.
func writeRestrictions(b *strings.Builder, restrict []RestrictionInfo) {
	if len(restrict) == 0 {
		return
	}
	b.WriteString("\n## Scope restrictions\n\n")
	b.WriteString("Every verb whose path carries one of these parameters must supply a value matching a glob below, or it fails closed.\n")
	for _, r := range restrict {
		fmt.Fprintf(b, "\n- `%s` must match: %s\n", r.Param, strings.Join(r.Globs, ", "))
	}
}

// writeDenied renders the blocked-class stanzas: one heading per deny with its
// teaching message, so the reference documents what the guardrail forbids and why.
func writeDenied(b *strings.Builder, prefix string, denied []DenyInfo) {
	if len(denied) == 0 {
		return
	}
	b.WriteString("\n## Denied operations\n")
	for _, d := range denied {
		fmt.Fprintf(b, "\n### %s %s %s (denied)\n\n", prefix, d.Group, d.Leaf)
		fmt.Fprintf(b, "%s\n", d.Message)
	}
}

// writeActions renders the complex-action stanzas after the leaf verbs, then a
// closing note naming the condition language - the one surface a reader meets it.
func writeActions(b *strings.Builder, prefix string, actions []ActionInfo) {
	if len(actions) == 0 {
		return
	}
	for _, a := range actions {
		heading := actionHeading(prefix, a)
		if a.Describe != "" {
			heading += " - " + a.Describe
		}
		fmt.Fprintf(b, "\n%s\n\n", heading)
		if a.MountResource != "" {
			fmt.Fprintf(b, "Shadows the generated `%s %s` leaf: invoking it runs this composite in the leaf's place.\n\n", a.MountResource, a.MountVerb)
		}
		switch {
		case len(a.Calls) > 0:
			writeCallAction(b, a)
		case a.Collect != nil:
			writeCollectAction(b, a)
		default:
			fmt.Fprintf(b, "Complex action. Polls `%s %s` every %s, up to %s, until:\n\n", a.Method, a.Path, a.Every, a.Timeout)
			fmt.Fprintf(b, "    %s\n\n", a.Until)
			fmt.Fprintf(b, "Authorized by grant: %s.\n", a.Grant)
			if len(a.Defaults) > 0 {
				b.WriteString("\nPre-flight defaults, resolved against the polled leaf when the input is absent:\n\n")
				for _, d := range a.Defaults {
					fmt.Fprintf(b, "- `%s` <- `%s`\n", d.Input, d.JMESPath)
				}
			}
		}
		if a.FailWhen != "" {
			fmt.Fprintf(b, "\nExits non-zero when:\n\n    %s\n", a.FailWhen)
		}
	}
	b.WriteString(conditionLanguageNote)
}

// actionHeading is an action stanza's title: a mount action reads at the leaf
// path it shadows (`<prefix> <resource> <verb>`), a named action under `action`.
func actionHeading(prefix string, a ActionInfo) string {
	if a.MountResource != "" {
		return fmt.Sprintf("## %s %s %s", prefix, a.MountResource, a.MountVerb)
	}
	return fmt.Sprintf("## %s %s %s", prefix, actionGroup, a.Leaf)
}

// writeCallAction renders a multi-call action stanza and its explicit bindings.
func writeCallAction(b *strings.Builder, a ActionInfo) {
	fmt.Fprintf(b, "Complex action. Runs %d granted calls in order, threading $step.field data between them; a failed call stops the sequence:\n\n", len(a.Calls))
	for i, s := range a.Calls {
		line := fmt.Sprintf("%d. `%s %s`", i+1, s.Method, s.Path)
		if s.As != "" {
			line += fmt.Sprintf(" - binds the response as `%s`", s.As)
		}
		fmt.Fprintf(b, "%s\n", line)
	}
}

// writeCollectAction renders an auto-pagination action stanza.
func writeCollectAction(b *strings.Builder, a ActionInfo) {
	c := a.Collect
	fmt.Fprintf(b, "Complex action. Collects every page from `%s %s`, incrementing `%s` and appending array responses until a page returns fewer than `%d` item(s).\n\n", c.Method, c.Path, c.PageParam, c.DefaultLimit)
	if c.Cache != "" {
		fmt.Fprintf(b, "Cached on disk for `%s` per resolved request; `--no-cache` bypasses and `--refresh` refetches.\n\n", c.Cache)
	}
	fmt.Fprintf(b, "Authorized by grant: %s.\n", c.Grant)
}

// collectCacheLabel renders a collect's cache TTL for the describe surface, or
// "" when the action declares no cache.
func collectCacheLabel(ttl time.Duration) string {
	if ttl <= 0 {
		return ""
	}
	return ttl.String()
}

// conditionLanguageNote names the until/fail-when dialect: JMESPath Community
// Edition, not the original spec. See docs/specverb-actions.md for the why.
const conditionLanguageNote = "\n## Condition language\n\n" +
	"The `until` and `fail-when` expressions above are [JMESPath, Community Edition](https://jmespath.site), " +
	"evaluated against the polled response as the root. A `$name` is a bound input or `as` capture, supplied " +
	"through the Community Edition's variable scope - baseline JMESPath (https://jmespath.org) has no `$variable` syntax, " +
	"so these expressions are not portable to an original-spec evaluator.\n"

// writeParamSections prints a verb's params as two blank-line-fronted Markdown
// lists, positional and options; a verb with neither says so, empties omitted.
func writeParamSections(b *strings.Builder, params []ParamInfo) {
	var positional, options []ParamInfo
	for _, p := range params {
		if p.Kind == "path" {
			positional = append(positional, p)
		} else {
			options = append(options, p)
		}
	}
	if len(positional) == 0 && len(options) == 0 {
		b.WriteString("\nTakes no arguments.\n")
		return
	}
	if len(positional) > 0 {
		fmt.Fprintf(b, "\nPositional arguments (%d):\n\n", len(positional))
		for _, line := range positionalLines(positional) {
			fmt.Fprintf(b, "- %s\n", line)
		}
	}
	if len(options) > 0 {
		fmt.Fprintf(b, "\nOptions (%d):\n\n", len(options))
		for _, line := range optionLines(options) {
			fmt.Fprintf(b, "- %s\n", line)
		}
	}
}

// fixedBodySentence states the exact body a state-toggle leaf sends, in sorted
// key order so the sentence is deterministic; "" for ordinary leaves.
func fixedBodySentence(fixed map[string]any) string {
	if len(fixed) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fixed))
	for k := range fixed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		v, _ := json.Marshal(fixed[k])
		parts[i] = fmt.Sprintf("%q: %s", k, v)
	}
	return fmt.Sprintf("Always sends the fixed body {%s}; takes no body flags.", strings.Join(parts, ", "))
}

// authSourceDisplay renders the scheme's value chain(s) a describe surface shows
// (`provider address`, fallbacks joined by " | "), never the resolved secret.
func authSourceDisplay(a guardfile.Auth) string {
	if a.Scheme != "query-param" {
		return a.Value.String()
	}
	parts := make([]string, len(a.Params))
	for i, p := range a.Params {
		parts[i] = p.Name + "=" + p.Value.String()
	}
	return strings.Join(parts, ", ")
}

// authSentence states how the engine authenticates in plain language, naming the
// value source(s) but never the secret(s) they hold.
func authSentence(a AuthInfo) string {
	switch a.Scheme {
	case "":
		return "No authentication is configured."
	case "query-param":
		return fmt.Sprintf("Authenticates with query parameters (scheme %s), reading each secret from %s. The secret values are never shown.", a.Scheme, a.Source)
	default:
		return fmt.Sprintf("Authenticates with the %q header (scheme %s), reading the token from %s. The token value is never shown.", a.Header, a.Scheme, a.Source)
	}
}

// positionalLines renders each path param as a Markdown bullet body: code-spanned
// <name> then its type in parens. Always required by construction, left implicit.
func positionalLines(params []ParamInfo) []string {
	lines := make([]string, len(params))
	for i, p := range params {
		lines[i] = fmt.Sprintf("`<%s>` (%s)", p.Name, p.Type)
	}
	return lines
}

// optionLines renders each body flag as a Markdown bullet body: code-spanned --name,
// type and requiredness in parens, then any upstream description after a colon.
func optionLines(params []ParamInfo) []string {
	lines := make([]string, len(params))
	for i, p := range params {
		req := "optional"
		if p.Required {
			req = "required"
		}
		line := fmt.Sprintf("`--%s` (%s, %s)", p.Name, p.Type, req)
		var details []string
		if p.UpstreamName != "" {
			details = append(details, fmt.Sprintf("sends as query parameter `%s`", p.UpstreamName))
		}
		if p.Desc != "" {
			details = append(details, p.Desc)
		}
		if len(details) > 0 {
			line += ": " + strings.Join(details, ". ")
		}
		lines[i] = line
	}
	return lines
}

// leafDescription is the rich per-verb help body, always populated even where the
// spec description is blank because the structure is what the engine always knows.
func leafDescription(desc opDescriptor) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", desc.Method, desc.Path)
	if desc.Destructive {
		b.WriteString(" (destructive)")
	}
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Authorized by: %s\n", desc.Grant)
	if desc.Describe != "" {
		fmt.Fprintf(&b, "%s\n", desc.Describe)
	}

	params := paramsOf(desc)
	if len(params) > 0 {
		b.WriteString("\nParameters:\n")
		for _, line := range paramHelpLines(params) {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	if len(desc.BodyFlags) > 0 {
		b.WriteString("\n--body-file <path> supplies the full JSON body instead of the body flags.\n")
	}
	if line := fixedBodySentence(desc.FixedBody); line != "" {
		fmt.Fprintf(&b, "\n%s\n", line)
	}
	b.WriteString("\nUse --dry-run to print the resolved request without firing it.")
	return b.String()
}

// paramHelpLines renders each param as an aligned `name (kind, type) req desc`
// row. Path params show as <name>, body params as --name, mirroring invocation.
func paramHelpLines(params []ParamInfo) []string {
	labels := make([]string, len(params))
	width := 0
	for i, p := range params {
		display := "--" + p.Name
		if p.Kind == "path" {
			display = "<" + p.Name + ">"
		}
		labels[i] = fmt.Sprintf("%s (%s, %s)", display, p.Kind, p.Type)
		if len(labels[i]) > width {
			width = len(labels[i])
		}
	}
	lines := make([]string, len(params))
	for i, p := range params {
		req := "optional"
		if p.Required {
			req = "required"
		}
		line := fmt.Sprintf("%-*s  %s", width, labels[i], req)
		var details []string
		if p.UpstreamName != "" {
			details = append(details, fmt.Sprintf("sends as query parameter %q", p.UpstreamName))
		}
		if p.Desc != "" {
			details = append(details, p.Desc)
		}
		if len(details) > 0 {
			line += "  " + strings.Join(details, ". ")
		}
		lines[i] = line
	}
	return lines
}
