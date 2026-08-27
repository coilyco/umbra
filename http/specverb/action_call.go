// Multi-call action execution: an ordered sequence of granted leaf calls with
// `as` bindings and `$step.field` data-flow between them. See docs/specverb-actions.md.

package specverb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/cli/verb"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/guardfile"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/respfmt"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/stepflow"
	"github.com/urfave/cli/v3"
)

// resolveCallAction resolves a multi-call action, failing closed at each gate
// (granted-only leaf, valid args, valid refs). See docs/specverb-actions.md.
func resolveCallAction(spec *spec, gf *guardfile.Guardfile, granted map[grantKey]guardfile.Grant, a guardfile.Action) (actionDescriptor, error) {
	inputNames := map[string]bool{}
	for _, in := range a.Inputs {
		inputNames[in.Name] = true
		if in.Default != "" {
			return actionDescriptor{}, fmt.Errorf("specverb: action %q: input %q: `default` is a poll-action pre-flight binding and is not supported on call actions (fail-closed)", a.Name, in.Name)
		}
	}
	bound := map[string]bool{}
	var steps []stepflow.Step
	for i, call := range a.Calls {
		step, err := resolveCallStep(spec, gf, granted, a.Name, i, call, inputNames, bound)
		if err != nil {
			return actionDescriptor{}, err
		}
		steps = append(steps, step)
		if call.As != "" {
			bound[call.As] = true
		}
	}
	if a.FailWhen != "" {
		if err := respfmt.Validate(a.FailWhen); err != nil {
			return actionDescriptor{}, fmt.Errorf("specverb: action %q: fail-when: %w", a.Name, err)
		}
	}
	return actionDescriptor{
		Name:          a.Name,
		VerbName:      actionVerbName(gf.Group, a),
		Describe:      a.Describe,
		Inputs:        a.Inputs,
		Calls:         steps,
		FailWhen:      a.FailWhen,
		MountVerb:     a.MountVerb,
		MountResource: a.MountResource,
		Combine:       a.IsMount(), // a shadow renders the whole chain, not just the last call
	}, nil
}

// resolveCallStep resolves one call step and validates its args. bound is the
// prior steps' `as` set.
func resolveCallStep(spec *spec, gf *guardfile.Guardfile, granted map[grantKey]guardfile.Grant, action string, i int, call guardfile.Call, inputNames, bound map[string]bool) (stepflow.Step, error) {
	leaf, err := resolveGrantedLeaf(spec, gf, granted, action, fmt.Sprintf("call %d", i+1), call.Verb, call.Resource)
	if err != nil {
		return stepflow.Step{}, err
	}
	if err := validateCallArgs(action, fmt.Sprintf("call %d", i+1), call.Args, leaf, inputNames, bound); err != nil {
		return stepflow.Step{}, err
	}
	return stepflow.Step{Leaf: leaf, Args: call.Args, As: call.As}, nil
}

// resolveGrantedLeaf recovers the granted leaf a step names, failing closed on a
// (verb, resource) the Guardfile does not `can`-grant. label sits in errors.
func resolveGrantedLeaf(spec *spec, gf *guardfile.Guardfile, granted map[grantKey]guardfile.Grant, action, label, verb, resource string) (opDescriptor, error) {
	g, ok := granted[grantKey{Verb: verb, Resource: resource}]
	if !ok {
		return opDescriptor{}, fmt.Errorf("specverb: action %q %s: %q %q which no `can` grant authorizes (deny-by-default; add `can %s %s`)", action, label, verb, resource, verb, resource)
	}
	leaf, err := resolveDescriptor(spec, gf.Group, g)
	if err != nil {
		return opDescriptor{}, fmt.Errorf("specverb: action %q %s: %w", action, label, err)
	}
	return leaf, nil
}

// validateCallArgs checks a step's args: a `$ref` names an input or a bound `as`,
// and the arg targets a real path param, flag, or sugar. where labels the step.
func validateCallArgs(action, where string, args []guardfile.ArgBind, leaf opDescriptor, inputNames, bound map[string]bool) error {
	paramNames := map[string]bool{}
	for _, p := range leaf.PathParams {
		paramNames[p] = true
	}
	flagNames := map[string]bool{}
	for _, f := range append(append(append([]fieldFlag{}, leaf.QueryFlags...), leaf.BodyFlags...), leaf.FormFlags...) {
		flagNames[f.Name] = true
	}
	for _, arg := range args {
		if err := validateCallArgRef(action, where, arg, inputNames, bound); err != nil {
			return err
		}
		switch {
		case arg.Name == ownerRepoArg:
			if !paramNames["owner"] || !paramNames["repo"] {
				return fmt.Errorf("specverb: action %q %s: arg %q needs the leaf to take owner+repo path params, but %s %s does not", action, where, ownerRepoArg, leaf.Method, leaf.Path)
			}
		case paramNames[arg.Name] || flagNames[arg.Name]:
			// binds a real path param or flag
		default:
			return fmt.Errorf("specverb: action %q %s: arg %q targets nothing on %s %s (not a path param or flag; fail-closed)", action, where, arg.Name, leaf.Method, leaf.Path)
		}
	}
	return nil
}

// validateCallArgRef checks a `$ref` arg value: a bare `$name` is a declared
// input, a `$step.field` names a bound `as` binding. Literals pass.
func validateCallArgRef(action, where string, arg guardfile.ArgBind, inputNames, bound map[string]bool) error {
	if !strings.HasPrefix(arg.Value, "$") {
		return nil
	}
	ref := strings.TrimPrefix(arg.Value, "$")
	if dot := strings.IndexByte(ref, '.'); dot >= 0 {
		head := ref[:dot]
		if !bound[head] {
			return fmt.Errorf("specverb: action %q %s: arg %q references $%s.* but no step in scope binds `as %s`", action, where, arg.Name, head, head)
		}
		return nil
	}
	if !inputNames[ref] {
		return fmt.Errorf("specverb: action %q %s: arg %q references $%s, which no `input` declares", action, where, arg.Name, ref)
	}
	return nil
}

// runCallAction runs the ordered call sequence through the generic stepflow engine.
func (rt *runtime) runCallAction(ctx context.Context, c *cli.Command, ad actionDescriptor, strVars map[string]string, sliceVars map[string][]string, jmesVars map[string]any) error {
	if c.Bool(flagDryRun) {
		return rt.renderCallActionPlan(ctx, c, ad, strVars, sliceVars)
	}
	bindings, lastRaw, err := stepflow.Run(ctx, c, ad.Calls, strVars, sliceVars, rt.stepRun)
	if err != nil {
		return err
	}
	if err := rt.renderCallResult(ad, bindings, lastRaw, c.String(flagQuery), c.String(flagOutput)); err != nil {
		return err
	}
	return rt.applyFailWhen(ad, lastRaw, stepflow.CondScope(jmesVars, bindings))
}

// renderCallResult prints a call action's output: a mount action (Combine) emits
// every `as` binding as one object; a named action prints just its final response.
func (rt *runtime) renderCallResult(ad actionDescriptor, bindings map[string]any, lastRaw []byte, query, output string) error {
	if !ad.Combine {
		return renderFinal(lastRaw, query, output)
	}
	combined := map[string]any{}
	for _, step := range ad.Calls {
		if step.As != "" {
			combined[step.As] = bindings[step.As]
		}
	}
	raw, err := json.Marshal(combined)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	return renderFinal(raw, query, output)
}

// fireCallAudited fires one call through the verb pipeline so it writes its own
// leaf audit row, returning the decoded response for the next step's data-flow.
func (rt *runtime) fireCallAudited(ctx context.Context, leaf opDescriptor, method, url string, body []byte, contentType string, c *cli.Command) (decoded any, raw []byte, err error) {
	inner := verb.Spec{
		Name:       leaf.VerbName,
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

// renderCallActionPlan prints the planned call sequence without firing it.
func (rt *runtime) renderCallActionPlan(ctx context.Context, c *cli.Command, ad actionDescriptor, strVars map[string]string, sliceVars map[string][]string) error {
	resolve := func(v string) (string, error) {
		if stepflow.UnsetInputRef(v, strVars) {
			return "", stepflow.ErrUnsetOptional
		}
		return stepflow.ResolveArgDry(v, strVars), nil
	}
	plan, err := stepflow.PlanCalls(ctx, ad.Calls, resolve, stepflow.SliceRefs(sliceVars), rt.stepRun)
	if err != nil {
		return err
	}
	return rt.renderPlanJSON(map[string]any{"action": ad.Name, "calls": plan}, c.String(flagOutput))
}

// leafPlan renders one resolved leaf request for a --dry-run plan: the request
// with its auth redacted; the caller adds `as` framing.
func (rt *runtime) leafPlan(leaf opDescriptor, method, url string, body []byte, contentType string) map[string]any {
	s := map[string]any{
		"leaf":    leaf.Leaf,
		"method":  method,
		"url":     rt.previewURL(url),
		"headers": rt.previewHeaders(body != nil, contentType),
	}
	if body != nil {
		var parsed any
		if err := json.Unmarshal(body, &parsed); err == nil {
			s["body"] = parsed
		}
	}
	return s
}

// renderPlanJSON marshals a plan object and prints it through respfmt, honoring
// --output so a dry-run reads the same as a live response.
func (rt *runtime) renderPlanJSON(plan map[string]any, output string) error {
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
