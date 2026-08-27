// Complex actions: named composite verbs running a bounded poll-until loop over
// an already-granted leaf, with a fail-when exit. See docs/specverb-actions.md.

package specverb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/cli/verb"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/guardfile"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/opcore"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/respfmt"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/audit"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/stepflow"
	"github.com/urfave/cli/v3"
)

// cacheOutcome carries a collect's cache-hit disposition from runCollectAction
// to the envelope OnComplete hook. One per built leaf; a CLI runs one action.
type cacheOutcome struct{ hit bool }

// actionGroup is the CLI noun every complex action mounts under, e.g.
// `forgejo action ci-watch`, mirroring the audit name `<path>.action.<name>`.
const actionGroup = "action"

// ownerRepoArg is the `args` sugar (a single `owner/name` value split across the
// `owner`/`repo` path params); the binder consuming it moved to opcore.
const ownerRepoArg = opcore.OwnerRepoArg

// actionDescriptor is one resolved complex action: its envelope identity, the
// inputs, the granted poll leaf, and the bounds. All validated at Build time.
type actionDescriptor struct {
	Name     string            // CLI leaf, e.g. ci-watch
	VerbName string            // envelope audit name, e.g. ward.ops.forgejo.action.ci-watch
	Describe string            // optional human note
	Inputs   []guardfile.Input // declared positional args and flags
	Leaf     opDescriptor      // the resolved poll target (a granted leaf)
	Args     []guardfile.ArgBind
	Until    string        // JMESPath; truthy ends the loop
	Every    time.Duration // sample interval, > 0
	Timeout  time.Duration // wall-clock bound, > 0
	As       string        // binding name for the final response
	FailWhen string        // JMESPath over the final response + bindings; truthy => non-zero exit

	// Calls is the resolved multi-call sequence; non-empty marks a call action,
	// mutually exclusive with the poll fields above.
	Calls []stepflow.Step

	// Collect is the resolved auto-pagination step; non-nil marks a collect
	// action, mutually exclusive with poll/call.
	Collect *collectStep

	// MountVerb/MountResource: the mount form shadows that leaf path instead of
	// mounting under the `action` noun. Empty for a named action.
	MountVerb     string
	MountResource string
	Combine       bool // render all `as` bindings together (mount call-actions)
}

// isMount reports whether ad shadows a leaf path (the `action <verb> <resource>` form).
func (ad actionDescriptor) isMount() bool { return ad.MountResource != "" }

// leafOp recovers the HTTP opDescriptor from an engine leaf. specverb only ever
// resolves opDescriptor leaves, so the assertion cannot fail on a built tree.
func leafOp(l stepflow.Leaf) opDescriptor { return l.(opDescriptor) }

// isCall reports whether ad is a multi-call action (vs a poll).
func (ad actionDescriptor) isCall() bool { return len(ad.Calls) > 0 }

// collectStep is one resolved auto-pagination action over a granted list leaf.
type collectStep struct {
	Leaf         opDescriptor
	Args         []guardfile.ArgBind
	PageParam    string
	LimitParam   string
	Limit        string
	DefaultLimit int
	As           string
	// CacheTTL is the on-disk cache lifetime, > 0 when the action declared a
	// `cache "<ttl>"` modifier; zero disables caching for this collect.
	CacheTTL time.Duration
}

// isCollect reports whether ad is an auto-pagination action.
func (ad actionDescriptor) isCollect() bool { return ad.Collect != nil }

// cacheable reports whether ad is a collect action with a configured TTL cache.
func (ad actionDescriptor) cacheable() bool { return ad.isCollect() && ad.Collect.CacheTTL > 0 }

// resolveActions resolves every Guardfile action into a descriptor, failing
// closed at each gate; granted is the (verb, resource) set the Guardfile grants.
func resolveActions(spec *spec, gf *guardfile.Guardfile, granted map[grantKey]guardfile.Grant) ([]actionDescriptor, error) {
	var out []actionDescriptor
	for _, a := range gf.Actions {
		ad, err := resolveAction(spec, gf, granted, a)
		if err != nil {
			return nil, err
		}
		out = append(out, ad)
	}
	return out, nil
}

func resolveAction(spec *spec, gf *guardfile.Guardfile, granted map[grantKey]guardfile.Grant, a guardfile.Action) (actionDescriptor, error) {
	if err := validateArrayInputs(a); err != nil {
		return actionDescriptor{}, err
	}
	// Parser guarantees exactly one of Poll/Calls/Collect is set.
	if len(a.Calls) > 0 {
		return resolveCallAction(spec, gf, granted, a)
	}
	if a.Collect != nil {
		return resolveCollectAction(spec, gf, granted, a)
	}
	return resolvePollAction(spec, gf, granted, a)
}

// validateArrayInputs fails an action at Build time when a list input reaches a
// form that cannot carry one. A collect step assembles its own paged request
// from string bindings, so a list arriving there would flatten silently, and the
// whole point of a typed input is that it refuses instead. Poll and call steps
// both bind lists properly and are unaffected.
func validateArrayInputs(a guardfile.Action) error {
	if a.Collect == nil {
		return nil
	}
	arrays := map[string]bool{}
	for _, in := range a.Inputs {
		if in.Array {
			arrays[in.Name] = true
		}
	}
	if len(arrays) == 0 {
		return nil
	}
	for _, arg := range a.Collect.Args {
		if ref := strings.TrimPrefix(arg.Value, "$"); strings.HasPrefix(arg.Value, "$") && arrays[ref] {
			return fmt.Errorf("specverb: action %q: arg %q references the `array` input $%s from a `collect` step, which pages a request built from scalar bindings only (fail-closed)", a.Name, arg.Name, ref)
		}
	}
	return nil
}

// resolvePollAction resolves a poll action: it binds the granted leaf, parses the
// loop bounds, and validates the until/fail-when/arg/default gates, all fail-closed.
func resolvePollAction(spec *spec, gf *guardfile.Guardfile, granted map[grantKey]guardfile.Grant, a guardfile.Action) (actionDescriptor, error) {
	p := a.Poll
	// Granted-only: an action may only poll an op the same Guardfile grants.
	g, ok := granted[grantKey{Verb: p.Verb, Resource: p.Resource}]
	if !ok {
		return actionDescriptor{}, fmt.Errorf("specverb: action %q polls %q %q which no `can` grant authorizes (deny-by-default; add `can %s %s`)", a.Name, p.Verb, p.Resource, p.Verb, p.Resource)
	}
	leaf, err := resolveDescriptor(spec, gf.Group, g)
	if err != nil {
		return actionDescriptor{}, fmt.Errorf("specverb: action %q: %w", a.Name, err)
	}
	every, err := positiveDuration(p.Every)
	if err != nil {
		return actionDescriptor{}, fmt.Errorf("specverb: action %q: every: %w", a.Name, err)
	}
	timeout, err := positiveDuration(p.Timeout)
	if err != nil {
		return actionDescriptor{}, fmt.Errorf("specverb: action %q: timeout: %w", a.Name, err)
	}
	if err := respfmt.Validate(p.Until); err != nil {
		return actionDescriptor{}, fmt.Errorf("specverb: action %q: until: %w", a.Name, err)
	}
	if a.FailWhen != "" {
		if err := respfmt.Validate(a.FailWhen); err != nil {
			return actionDescriptor{}, fmt.Errorf("specverb: action %q: fail-when: %w", a.Name, err)
		}
	}
	inputNames := map[string]bool{}
	for _, in := range a.Inputs {
		inputNames[in.Name] = true
	}
	if err := validateArgs(a, leaf, inputNames); err != nil {
		return actionDescriptor{}, err
	}
	if err := validateDefaults(a); err != nil {
		return actionDescriptor{}, err
	}
	return actionDescriptor{
		Name:          a.Name,
		VerbName:      actionVerbName(gf.Group, a),
		Describe:      a.Describe,
		Inputs:        a.Inputs,
		Leaf:          leaf,
		Args:          p.Args,
		Until:         p.Until,
		Every:         every,
		Timeout:       timeout,
		As:            p.As,
		FailWhen:      a.FailWhen,
		MountVerb:     a.MountVerb,
		MountResource: a.MountResource,
	}, nil
}

// actionVerbName is the action's dotted audit identity: a mount action mirrors the
// shadowed leaf (`<group>.<resource>.<verb>`), a named one is `<group>.action.<name>`.
func actionVerbName(group []string, a guardfile.Action) string {
	if a.IsMount() {
		return strings.Join(group, ".") + "." + a.MountResource + "." + a.MountVerb
	}
	return strings.Join(group, ".") + "." + actionGroup + "." + a.Name
}

// validateArgs checks every poll arg resolves: a `$ref` names a declared input,
// and the arg targets a real leaf path param, flag, or the owner-repo sugar.
func validateArgs(a guardfile.Action, leaf opDescriptor, inputNames map[string]bool) error {
	paramNames := map[string]bool{}
	for _, p := range leaf.PathParams {
		paramNames[p] = true
	}
	flagNames := map[string]bool{}
	for _, f := range append(append(append([]fieldFlag{}, leaf.QueryFlags...), leaf.BodyFlags...), leaf.FormFlags...) {
		flagNames[f.Name] = true
	}
	for _, arg := range a.Poll.Args {
		if strings.HasPrefix(arg.Value, "$") {
			ref := strings.TrimPrefix(arg.Value, "$")
			if !inputNames[ref] {
				return fmt.Errorf("specverb: action %q: arg %q references $%s, which no `input` declares", a.Name, arg.Name, ref)
			}
		}
		switch {
		case arg.Name == ownerRepoArg:
			if !paramNames["owner"] || !paramNames["repo"] {
				return fmt.Errorf("specverb: action %q: arg %q needs the leaf to take owner+repo path params, but %s %s does not", a.Name, ownerRepoArg, leaf.Method, leaf.Path)
			}
		case paramNames[arg.Name] || flagNames[arg.Name]:
			// binds a real path param or flag
		default:
			return fmt.Errorf("specverb: action %q: arg %q targets nothing on %s %s (not a path param or flag; fail-closed)", a.Name, arg.Name, leaf.Method, leaf.Path)
		}
	}
	return nil
}

// validateDefaults checks each input `default <jmespath>`: it must parse, and a
// defaulted input may not also bind a poll arg. See specverb-action-defaults.md.
func validateDefaults(a guardfile.Action) error {
	for _, in := range a.Inputs {
		if in.Default == "" {
			continue
		}
		if err := respfmt.Validate(in.Default); err != nil {
			return fmt.Errorf("specverb: action %q: input %q default: %w", a.Name, in.Name, err)
		}
		for _, arg := range a.Poll.Args {
			if arg.Value == "$"+in.Name {
				return fmt.Errorf("specverb: action %q: input %q has a `default` resolved from the poll response, so it cannot also bind poll arg %q (the request is built before the default resolves; fail-closed)", a.Name, in.Name, arg.Name)
			}
		}
	}
	return nil
}

// positiveDuration parses a Go duration string and requires it to be > 0, so a
// bound of "0s" can never disable the loop's clock.
func positiveDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (e.g. \"10s\", \"30m\"): %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q must be greater than zero", s)
	}
	return d, nil
}

// buildActionGroup mounts the resolved actions under the `action` noun, or
// returns nil when the Guardfile declares none.
func (rt *runtime) buildActionGroup(descs []actionDescriptor) *cli.Command {
	if len(descs) == 0 {
		return nil
	}
	grp := &cli.Command{Name: actionGroup, Usage: "complex actions: bounded composite verbs"}
	for _, ad := range descs {
		grp.Commands = append(grp.Commands, rt.buildActionLeaf(ad))
	}
	return grp
}

// buildActionLeaf turns one action descriptor into a guarded leaf: positional
// inputs become args, flag inputs flags, wrapped for the envelope audit row.
func (rt *runtime) buildActionLeaf(ad actionDescriptor) *cli.Command {
	flags := []cli.Flag{
		&cli.BoolFlag{Name: flagDryRun, Usage: "print the action plan (the call sequence and compiled until) without firing it"},
		&cli.StringFlag{Name: flagQuery, Usage: "JMESPath projection applied to the final response"},
		&cli.StringFlag{Name: flagOutput, Usage: "output format: yaml | yaml-stream | json | text | table"},
	}
	if ad.cacheable() {
		flags = append(flags,
			&cli.BoolFlag{Name: flagNoCache, Usage: "bypass the TTL cache for this run (no read, no write)"},
			&cli.BoolFlag{Name: flagRefresh, Usage: "invalidate the cached entry and refetch"},
		)
	}
	var positional []guardfile.Input
	for _, in := range ad.Inputs {
		if in.Positional {
			positional = append(positional, in)
			continue
		}
		if in.Array {
			flags = append(flags, &cli.StringSliceFlag{Name: in.Name, Usage: in.Help})
			continue
		}
		flags = append(flags, &cli.StringFlag{Name: in.Name, Usage: in.Help})
	}
	outcome := &cacheOutcome{}
	spec := verb.Spec{
		Name:     ad.VerbName,
		ArgsFunc: actionArgsFunc(ad),
		Action:   rt.runAction(ad, outcome),
	}
	if ad.cacheable() {
		spec.OnComplete = func(rec *audit.Record) {
			if outcome.hit {
				rec.Cache = "hit"
			}
		}
	}
	return &cli.Command{
		Name:        ad.Name,
		Usage:       actionUsage(ad),
		Description: actionDescription(ad),
		ArgsUsage:   argsUsage(inputNamesOf(positional)),
		Flags:       flags,
		Action:      rt.wrap(spec),
	}
}

// actionArgsFunc feeds the shell-metachar gate, location-aware: only inputs whose
// value reaches a URL path/query are gated (the injection surface).
func actionArgsFunc(ad actionDescriptor) func(*cli.Command) (map[string]string, []string) {
	urlBound := urlBoundInputs(ad)
	return func(c *cli.Command) (map[string]string, []string) {
		named := map[string]string{}
		positional := c.Args().Slice()
		pi := 0
		for _, in := range ad.Inputs {
			var val string
			switch {
			case in.Positional:
				if pi >= len(positional) {
					continue
				}
				val = positional[pi]
				pi++
			case c.IsSet(in.Name):
				val = c.String(in.Name)
			default:
				continue
			}
			if urlBound[in.Name] {
				named[in.Name] = val
			}
		}
		return named, nil
	}
}

// urlBoundInputs returns input names whose value binds to a URL location (path or
// query, incl. owner-repo sugar) in any leaf; body-only inputs are absent.
func urlBoundInputs(ad actionDescriptor) map[string]bool {
	bound := map[string]bool{}
	mark := func(leaf opDescriptor, args []guardfile.ArgBind) {
		path := map[string]bool{}
		for _, p := range leaf.PathParams {
			path[p] = true
		}
		query := map[string]bool{}
		for _, f := range leaf.QueryFlags {
			query[f.Name] = true
		}
		for _, arg := range args {
			if !strings.HasPrefix(arg.Value, "$") {
				continue // literal binding: author-supplied, not operator argv
			}
			if arg.Name == ownerRepoArg || path[arg.Name] || query[arg.Name] {
				bound[strings.TrimPrefix(arg.Value, "$")] = true
			}
		}
	}
	if ad.isCall() {
		for _, s := range ad.Calls {
			mark(leafOp(s.Leaf), s.Args)
		}
		return bound
	}
	if ad.isCollect() {
		mark(ad.Collect.Leaf, ad.Collect.Args)
		if strings.HasPrefix(ad.Collect.Limit, "$") {
			bound[strings.TrimPrefix(ad.Collect.Limit, "$")] = true
		}
		return bound
	}
	mark(ad.Leaf, ad.Args)
	return bound
}

// runAction binds inputs, then runs the action: a multi-call sequence, or the
// poll path (build the request, then print the plan or run the bounded loop).
func (rt *runtime) runAction(ad actionDescriptor, outcome *cacheOutcome) cli.ActionFunc {
	return func(ctx context.Context, c *cli.Command) error {
		strVars, jmesVars, sliceVars, err := bindInputs(ad, c)
		if err != nil {
			return err
		}
		if ad.isCall() {
			return rt.runCallAction(ctx, c, ad, strVars, sliceVars, jmesVars)
		}
		if ad.isCollect() {
			return rt.runCollectAction(ctx, c, ad, strVars, jmesVars, outcome)
		}
		dry := c.Bool(flagDryRun)
		method, url, body, contentType, err := rt.buildLeafRequest(ctx, dry, ad, strVars, sliceVars)
		if err != nil {
			return err
		}
		pending := pendingDefaults(ad, strVars)
		if dry {
			return rt.renderActionPlan(ad, method, url, body, contentType, pending, c.String(flagOutput))
		}
		if len(pending) > 0 {
			if derr := rt.resolveDefaults(ctx, c, ad, method, url, body, contentType, pending, strVars, jmesVars); derr != nil {
				return derr
			}
		}
		return rt.runPoll(ctx, c, ad, method, url, body, contentType, jmesVars)
	}
}

// pendingDefaults returns the defaulted inputs that were not supplied on the CLI,
// so a pre-flight only fires when there is at least one input left to resolve.
func pendingDefaults(ad actionDescriptor, strVars map[string]string) []guardfile.Input {
	var out []guardfile.Input
	for _, in := range ad.Inputs {
		if in.Default == "" {
			continue
		}
		if _, set := strVars[in.Name]; set {
			continue // supplied on the CLI; the default is not consulted
		}
		out = append(out, in)
	}
	return out
}

// resolveDefaults fires the poll leaf once as an audited pre-flight, then binds
// each absent defaulted input. Fails closed. See specverb-action-defaults.md.
func (rt *runtime) resolveDefaults(ctx context.Context, c *cli.Command, ad actionDescriptor, method, url string, body []byte, contentType string, pending []guardfile.Input, strVars map[string]string, jmesVars map[string]any) error {
	decoded, _, ferr := rt.fireLeafAudited(ctx, ad, method, url, body, contentType, c)
	if ferr != nil {
		return ferr
	}
	for _, in := range pending {
		v, err := respfmt.Eval(decoded, in.Default, jmesVars)
		if err != nil {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("action %q: input %q default %q: %w", ad.Name, in.Name, in.Default, err),
				"check the default JMESPath against the pre-flight response shape")
		}
		switch v.(type) {
		case string, float64, bool:
			// a scalar binds as the input value
		case nil:
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("action %q: input %q default %q resolved to null (empty listing?)", ad.Name, in.Name, in.Default),
				"pass --"+in.Name+" explicitly, or check the pre-flight listing is non-empty")
		default:
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("action %q: input %q default %q resolved to a non-scalar value", ad.Name, in.Name, in.Default),
				"a default must select a single value, e.g. max(...) or [0].field")
		}
		strVars[in.Name] = stepflow.ScalarToString(v)
		jmesVars[in.Name] = v
	}
	return nil
}

// bindScope is the three parallel scopes one action run binds its inputs into:
// raw strings for the request, coerced values for conditions, and lists for the
// arguments a scalar cannot carry.
type bindScope struct {
	str    map[string]string
	jmes   map[string]any
	slices map[string][]string
}

// bindInputs reads inputs into strVars (raw, for the request) and jmesVars
// (coerced, for conditions). Unset optional flags bind in neither scope, and a
// missing required one ends the run before any request is built.
func bindInputs(ad actionDescriptor, c *cli.Command) (strVars map[string]string, jmesVars map[string]any, sliceVars map[string][]string, err error) {
	sc := bindScope{str: map[string]string{}, jmes: map[string]any{}, slices: map[string][]string{}}
	positional := c.Args().Slice()
	pi := 0
	for _, in := range ad.Inputs {
		var berr error
		switch {
		case in.Array:
			berr = sc.bindArray(in, c)
		case in.Positional:
			berr = sc.bindPositional(in, positional, &pi)
		default:
			berr = sc.bindFlag(in, c)
		}
		if berr != nil {
			return nil, nil, nil, berr
		}
	}
	if pi < len(positional) {
		return nil, nil, nil, exitcode.New(exitcode.UserError, "user_error",
			fmt.Errorf("got %d positional args, this action takes %d", len(positional), pi), "remove the extra arguments")
	}
	strVars, jmesVars, sliceVars = sc.str, sc.jmes, sc.slices
	return strVars, jmesVars, sliceVars, nil
}

// bindArray binds one repeated flag into the list scope.
func (sc bindScope) bindArray(in guardfile.Input, c *cli.Command) error {
	if !c.IsSet(in.Name) {
		return missingRequiredFlag(in, "supply at least one value")
	}
	vals := c.StringSlice(in.Name)
	if err := checkMatches(in, vals); err != nil {
		return err
	}
	sc.slices[in.Name] = vals
	sc.jmes[in.Name] = coerceScalars(vals)
	return nil
}

// checkMatches enforces in's `matches` constraints while inputs bind, before any
// request exists. Constraints are independent so a refusal names one. #322.
func checkMatches(in guardfile.Input, vals []string) error {
	for _, m := range in.Matches {
		if slices.ContainsFunc(vals, func(v string) bool {
			return opcore.MatchesAnyGlob(v, m.Globs)
		}) {
			continue
		}
		reason := m.Message
		if reason == "" {
			reason = fmt.Sprintf("no value matching %v", m.Globs)
		}
		return exitcode.New(exitcode.UserError, "user_error",
			fmt.Errorf("--%s: %s", in.Name, reason),
			fmt.Sprintf("supply a value matching one of %v", m.Globs))
	}
	return nil
}

// bindPositional consumes the next positional argument, advancing pi.
func (sc bindScope) bindPositional(in guardfile.Input, positional []string, pi *int) error {
	if *pi >= len(positional) {
		if in.Required {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("missing required argument <%s>", in.Name), "supply the positional arguments this action names")
		}
		return nil
	}
	val := positional[*pi]
	*pi++
	if err := checkMatches(in, []string{val}); err != nil {
		return err
	}
	sc.str[in.Name] = val
	sc.jmes[in.Name] = coerceScalar(val)
	return nil
}

// bindFlag binds one scalar flag.
func (sc bindScope) bindFlag(in guardfile.Input, c *cli.Command) error {
	if !c.IsSet(in.Name) {
		return missingRequiredFlag(in, "supply it on the command line")
	}
	val := c.String(in.Name)
	if err := checkMatches(in, []string{val}); err != nil {
		return err
	}
	sc.str[in.Name] = val
	sc.jmes[in.Name] = coerceScalar(val)
	return nil
}

// missingRequiredFlag is the pre-write refusal for an absent required flag, and
// nil for an absent optional one. A control that returned an error after the
// write would leave the hazard in place, so this fires before the request exists.
func missingRequiredFlag(in guardfile.Input, hint string) error {
	if !in.Required {
		return nil
	}
	return exitcode.New(exitcode.UserError, "user_error",
		fmt.Errorf("missing required flag --%s", in.Name), hint)
}

// coerceScalars lowers a list input for the condition scope, element by element,
// matching the single-value coercion so `contains($labels, ...)` sees numbers as
// numbers.
func coerceScalars(vals []string) []any {
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		out = append(out, coerceScalar(v))
	}
	return out
}

// coerceScalar lowers an input string to a number where it parses as one (so
// `run_number==$run` compares number to number), otherwise the string itself.
func coerceScalar(s string) any {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return float64(i) // JSON numbers decode to float64; match that
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// buildLeafRequest assembles the polled leaf's HTTP request from the arg
// bindings, using the same path/query machinery a directly-invoked leaf uses.
func (rt *runtime) buildLeafRequest(ctx context.Context, dry bool, ad actionDescriptor, strVars map[string]string, sliceVars map[string][]string) (method, url string, body []byte, contentType string, err error) {
	return rt.buildCallRequest(ctx, dry, ad.Leaf, ad.Args, func(v string) (string, error) {
		return resolveArgValue(v, strVars)
	}, stepflow.SliceRefs(sliceVars))
}

// buildCallRequest assembles one leaf's HTTP request from arg bindings, resolving
// each arg through resolve. ctx and dry feed base-url resolution (dry stays offline).
func (rt *runtime) buildCallRequest(ctx context.Context, dry bool, leaf opDescriptor, args []guardfile.ArgBind, resolve stepflow.Resolve, sliceOf stepflow.SliceOf) (method, url string, body []byte, contentType string, err error) {
	b := opcore.NewArgBinder(leaf)
	for _, arg := range args {
		if sliceOf != nil {
			if vals, ok := sliceOf(arg.Value); ok {
				if berr := b.BindSlice(arg.Name, vals); berr != nil {
					return "", "", nil, "", berr
				}
				continue
			}
		}
		val, rerr := resolve(arg.Value)
		if errors.Is(rerr, stepflow.ErrUnsetOptional) {
			continue
		}
		if rerr != nil {
			return "", "", nil, "", exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("action arg %q: %w", arg.Name, rerr), "supply the input this arg references")
		}
		if berr := b.Bind(arg.Name, val); berr != nil {
			return "", "", nil, "", berr
		}
	}
	if berr := b.RequireAllPaths(); berr != nil {
		return "", "", nil, "", berr
	}
	if dry {
		return rt.buildCallRequestDry(ctx, leaf, b)
	}
	return rt.buildCallRequestLive(ctx, leaf, b)
}

// buildCallRequestDry keeps the dry-run planner's placeholder-friendly request
// assembly local to specverb.
func (rt *runtime) buildCallRequestDry(ctx context.Context, leaf opDescriptor, b *opcore.ArgBinder) (method, url string, body []byte, contentType string, err error) {
	if rerr := rt.CheckRestrictions(leaf.PathParams, b.PathVals); rerr != nil {
		return "", "", nil, "", rerr
	}
	qs := ""
	if len(b.Query) > 0 {
		qs = "?" + b.Query.Encode()
	}
	base, berr := rt.BaseForRequest(ctx, true)
	if berr != nil {
		return "", "", nil, "", berr
	}
	url = base + opcore.FillPath(leaf.Path, b.PathVals) + qs
	contentType = contentTypeJSON
	// Through opcore so a preview cannot order the body modes differently from
	// the request it is previewing.
	body, err = opcore.AssembleBody(leaf, b.BodyObj)
	if err != nil {
		return "", "", nil, "", exitcode.New(exitcode.Internal, "internal", err, "")
	}
	return leaf.Method, url, body, contentType, nil
}

// buildCallRequestLive lowers a resolved leaf request through the transport-
// neutral opcore.Request resolver, then hands the assembled request back.
func (rt *runtime) buildCallRequestLive(ctx context.Context, leaf opDescriptor, b *opcore.ArgBinder) (method, url string, body []byte, contentType string, err error) {
	argsByLocation := opcore.Args{
		Path:  map[string]string{},
		Query: map[string]string{},
		Body:  b.BodyObj,
	}
	for i, p := range leaf.PathParams {
		argsByLocation.Path[p] = b.PathVals[i]
	}
	for k, vals := range b.Query {
		if len(vals) > 0 {
			argsByLocation.Query[k] = vals[0]
		}
	}
	req, rerr := (opcore.Operation{Desc: leaf, RT: rt.Runtime}).Resolve(ctx, argsByLocation, false)
	if rerr != nil {
		return "", "", nil, "", rerr
	}
	return req.Method, req.URL, req.Body, req.ContentType, nil
}

// resolveArgValue resolves a `$ref` against the bound input strings, or returns
// a literal verbatim. A `$ref` to an unbound input is an error.
func resolveArgValue(value string, strVars map[string]string) (string, error) {
	if !strings.HasPrefix(value, "$") {
		return value, nil
	}
	ref := strings.TrimPrefix(value, "$")
	v, ok := strVars[ref]
	if !ok {
		return "", fmt.Errorf("$%s: %w", ref, stepflow.ErrUnsetOptional)
	}
	return v, nil
}

// runPoll runs the bounded loop: each tick fires the leaf, binds the response
// under `as`, tests `until`; timeout is a non-zero exit. See specverb-actions.md.
func (rt *runtime) runPoll(ctx context.Context, c *cli.Command, ad actionDescriptor, method, url string, body []byte, contentType string, jmesVars map[string]any) error {
	loopCtx, cancel := context.WithTimeout(ctx, ad.Timeout)
	defer cancel()
	ticker := time.NewTicker(ad.Every)
	defer ticker.Stop()

	var finalRaw []byte
	for {
		decoded, raw, ferr := rt.fireLeafAudited(loopCtx, ad, method, url, body, contentType, c)
		if ferr != nil {
			return ferr
		}
		finalRaw = raw
		jmesVars[ad.As] = decoded

		done, eerr := respfmt.EvalBool(decoded, ad.Until, jmesVars)
		if eerr != nil {
			return exitcode.New(exitcode.UserError, "user_error", eerr, "check the `until` JMESPath against the response shape")
		}
		if done {
			break
		}
		select {
		case <-loopCtx.Done():
			return exitcode.New(exitcode.UpstreamFailed, "action_timeout",
				fmt.Errorf("action %q: `until` did not settle within %s", ad.Name, ad.Timeout),
				"raise `timeout`, or check the run is progressing")
		case <-ticker.C:
		}
	}

	if err := renderFinal(finalRaw, c.String(flagQuery), c.String(flagOutput)); err != nil {
		return err
	}
	return rt.applyFailWhen(ad, finalRaw, jmesVars)
}

// fireLeafAudited fires one tick through the verb pipeline so the call writes
// its own leaf audit row. SkipPolicy: the action envelope already gated argv.
func (rt *runtime) fireLeafAudited(ctx context.Context, ad actionDescriptor, method, url string, body []byte, contentType string, c *cli.Command) (decoded any, raw []byte, err error) {
	inner := verb.Spec{
		Name:       ad.Leaf.VerbName,
		SkipPolicy: true, // the action envelope already gated the operator's argv
		Action: func(ictx context.Context, _ *cli.Command) error {
			var e error
			decoded, raw, _, e = rt.FireCapture(ictx, method, url, body, contentType)
			return e
		},
	}
	err = rt.wrap(inner)(ctx, c)
	return decoded, raw, err
}

// renderFinal prints a composite action's final response through respfmt,
// honoring --query and --output so it reads the same as a generated leaf.
func renderFinal(raw []byte, query, output string) error {
	rendered, err := respfmt.Render(raw, query, output)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "the response was not valid JSON")
	}
	if len(rendered) == 0 {
		return nil
	}
	fmt.Print(string(rendered))
	return nil
}

// applyFailWhen evaluates fail-when against the final response (with bindings as
// $variables); a truthy result is a non-zero exit. See specverb-actions.md.
func (rt *runtime) applyFailWhen(ad actionDescriptor, finalRaw []byte, jmesVars map[string]any) error {
	if ad.FailWhen == "" {
		return nil
	}
	var data any
	if len(finalRaw) > 0 {
		if err := json.Unmarshal(finalRaw, &data); err != nil {
			return exitcode.New(exitcode.Internal, "internal", err, "the response was not valid JSON")
		}
	}
	fail, err := respfmt.EvalBool(data, ad.FailWhen, jmesVars)
	if err != nil {
		return exitcode.New(exitcode.UserError, "user_error", err, "check the `fail-when` JMESPath against the response shape")
	}
	if fail {
		return exitcode.New(exitcode.Generic, "action_failed",
			fmt.Errorf("action %q: fail-when predicate matched", ad.Name),
			"the watched operation reported failure; inspect the output above")
	}
	return nil
}

// renderActionPlan prints the bound call sequence and compiled until/fail-when,
// firing nothing; an absent defaulted input adds the pre-flight call to the plan.
func (rt *runtime) renderActionPlan(ad actionDescriptor, method, url string, body []byte, contentType string, pending []guardfile.Input, output string) error {
	poll := map[string]any{
		"method":  method,
		"url":     rt.previewURL(url),
		"headers": rt.previewHeaders(body != nil, contentType),
		"every":   ad.Every.String(),
		"timeout": ad.Timeout.String(),
		"until":   ad.Until,
		"as":      ad.As,
	}
	if body != nil {
		var parsed any
		if err := json.Unmarshal(body, &parsed); err == nil {
			poll["body"] = parsed
		}
	}
	plan := map[string]any{
		"action": ad.Name,
		"poll":   poll,
	}
	if len(pending) > 0 {
		defaults := make([]map[string]any, 0, len(pending))
		for _, in := range pending {
			defaults = append(defaults, map[string]any{"input": in.Name, "jmespath": in.Default})
		}
		plan["preflight"] = map[string]any{
			"leaf":     ad.Leaf.Leaf,
			"method":   method,
			"url":      rt.previewURL(url),
			"defaults": defaults,
		}
	}
	if ad.FailWhen != "" {
		plan["fail_when"] = ad.FailWhen
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	rendered, err := respfmt.Render(raw, "", output)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	fmt.Print(string(rendered))
	return nil
}

// actionUsage is the one-line help: the action describe note, or a default.
func actionUsage(ad actionDescriptor) string {
	if ad.Describe != "" {
		return ad.Describe
	}
	if ad.isCall() {
		return fmt.Sprintf("complex action: a %d-call sequence", len(ad.Calls))
	}
	if ad.isCollect() {
		return fmt.Sprintf("complex action collecting pages from %s %s", ad.Collect.Leaf.Method, ad.Collect.Leaf.Path)
	}
	return fmt.Sprintf("complex action polling %s %s", ad.Leaf.Method, ad.Leaf.Path)
}

// actionDescription is the rich per-action help body.
func actionDescription(ad actionDescriptor) string {
	var b strings.Builder
	if ad.Describe != "" {
		fmt.Fprintf(&b, "%s\n\n", ad.Describe)
	}
	switch {
	case ad.isCall():
		b.WriteString("Runs this sequence of granted calls, threading $step.field data between them:\n")
		for i, s := range ad.Calls {
			op := leafOp(s.Leaf)
			fmt.Fprintf(&b, "  %d. %s %s", i+1, op.Method, op.Path)
			if s.As != "" {
				fmt.Fprintf(&b, " (as %s)", s.As)
			}
			b.WriteString("\n")
		}
	case ad.isCollect():
		fmt.Fprintf(&b, "Collects every page from %s %s, incrementing %q and appending array responses until a page returns fewer than %d item(s).\n", ad.Collect.Leaf.Method, ad.Collect.Leaf.Path, ad.Collect.PageParam, ad.Collect.DefaultLimit)
		fmt.Fprintf(&b, "\nAuthorized by grant: %s.\n", ad.Collect.Leaf.Grant)
	default:
		fmt.Fprintf(&b, "Polls %s %s every %s, up to %s, until:\n  %s\n", ad.Leaf.Method, ad.Leaf.Path, ad.Every, ad.Timeout, ad.Until)
		fmt.Fprintf(&b, "\nAuthorized by grant: %s.\n", ad.Leaf.Grant)
		if defaults := defaultedInputs(ad); len(defaults) > 0 {
			b.WriteString("\nPre-flight defaults (resolved against the polled leaf when the input is absent):\n")
			for _, in := range defaults {
				fmt.Fprintf(&b, "  --%s <- %s\n", in.Name, in.Default)
			}
		}
	}
	if ad.FailWhen != "" {
		fmt.Fprintf(&b, "\nExits non-zero when: %s\n", ad.FailWhen)
	}
	b.WriteString("\nUse --dry-run to print the plan without firing it.")
	return b.String()
}

// defaultedInputs returns the action's inputs that declare a `default` pre-flight
// JMESPath, in declaration order; nil for an action with none.
func defaultedInputs(ad actionDescriptor) []guardfile.Input {
	var out []guardfile.Input
	for _, in := range ad.Inputs {
		if in.Default != "" {
			out = append(out, in)
		}
	}
	return out
}

// inputNamesOf returns the names of the given inputs, in order.
func inputNamesOf(inputs []guardfile.Input) []string {
	names := make([]string, len(inputs))
	for i, in := range inputs {
		names[i] = in.Name
	}
	return names
}
