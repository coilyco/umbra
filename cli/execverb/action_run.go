// Exec action execution: the stepflow Runner over captured exec invocations,
// the structured step response, and the mounted action leaves. See docs/execverb.md.

package execverb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/respfmt"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/policy"
	"github.com/coilyco/umbra/pkg/stepflow"
	"github.com/coilyco/umbra/pkg/valuesource"
	"github.com/urfave/cli/v3"
)

// flagDryRun / flagOutput are the action-envelope flags, mirroring specverb.
const (
	flagDryRun = "dry-run"
	flagOutput = "output"
)

// execStepRunner implements stepflow.Runner over captured exec invocations: each
// step fires a granted leaf's pinned command and parses its structured response.
type execStepRunner struct {
	gf        *Guardfile
	wrap      func(verb.Spec) cli.ActionFunc
	capture   CaptureRunner
	host      HostResolver
	providers map[string]valuesource.Provider
}

// Fire runs an authorized step. A non-zero exit fails the sequence.
func (r *execStepRunner) Fire(ctx context.Context, c *cli.Command, leaf stepflow.Leaf, args []guardfile.ArgBind, resolve stepflow.Resolve, sliceOf stepflow.SliceOf) (any, []byte, error) {
	if err := refuseSliceArgs(args, sliceOf); err != nil {
		return nil, nil, err
	}
	return r.fire(ctx, c, leaf, args, resolve)
}

// refuseSliceArgs fails closed when a list-valued input reaches an exec step.
// argv is flat, so joining or spreading would guess what the pinned command meant.
func refuseSliceArgs(args []guardfile.ArgBind, sliceOf stepflow.SliceOf) error {
	if sliceOf == nil {
		return nil
	}
	for _, arg := range args {
		if _, ok := sliceOf(arg.Value); ok {
			return fmt.Errorf("execverb: arg %q binds a list-valued input to an exec step, which takes argv tokens only (fail-closed)", arg.Name)
		}
	}
	return nil
}

// fire resolves the step argv, applies the leaf's guards, and runs the pinned
// command through the audited verb pipeline, capturing its output.
func (r *execStepRunner) fire(ctx context.Context, c *cli.Command, leaf stepflow.Leaf, args []guardfile.ArgBind, resolve stepflow.Resolve) (any, []byte, error) {
	l, tokens, err := r.prepare(ctx, leaf, args, resolve)
	if err != nil {
		return nil, nil, err
	}
	env, err := resolveEnv(ctx, r.gf, r.providers)
	if err != nil {
		return nil, nil, exitcode.New(exitcode.Internal, "internal", err, "check the env value provider address and credentials")
	}
	resolvedTokens, written, err := resolveFlagValues(ctx, tokens, l.grant, r.providers)
	if err != nil {
		return nil, nil, exitcode.New(exitcode.Internal, "internal", err, "check the resolve-flag value source and its permissions")
	}
	defer unlinkAll(written)
	argv := append(append(append([]string{}, r.gf.argvPrefixFor(l.grant)...), l.grant.executionArgv()...), resolvedTokens...)
	var decoded map[string]any
	var raw []byte
	inner := verb.Spec{
		Name:       l.verbName(),
		SkipPolicy: true, // the action envelope already gated the operator's argv
		Action: func(ictx context.Context, _ *cli.Command) error {
			stdout, stderr, code, rerr := r.capture(ictx, l.grant.ExecBin(r.gf.Bin), argv, env)
			if rerr != nil {
				return exitcode.New(exitcode.UpstreamFailed, "upstream_failed", rerr, "the step command could not be spawned")
			}
			decoded = execStepResponse(stdout, stderr, code)
			raw, _ = json.Marshal(decoded)
			if code != 0 {
				return fmt.Errorf("%s exited %d: %s", l.Label(), code, tailOf(stderr, stdout))
			}
			return nil
		},
	}
	if err := r.wrap(inner)(ctx, c); err != nil {
		return decoded, raw, err
	}
	return decoded, raw, nil
}

// prepare resolves and gates one step's argv tokens against the leaf's guards:
// preflight gates, wrap + grant when-guards, flag policy, and the metachar gate.
func (r *execStepRunner) prepare(ctx context.Context, leaf stepflow.Leaf, args []guardfile.ArgBind, resolve stepflow.Resolve) (execLeaf, []string, error) {
	l, ok := leaf.(execLeaf)
	if !ok {
		return execLeaf{}, nil, fmt.Errorf("execverb: step leaf is not an exec leaf (engine misuse)")
	}
	tokens, err := resolveStepArgs(args, resolve)
	if err != nil {
		return execLeaf{}, nil, err
	}
	if err := policy.ValidateArgSlice("step", tokens); err != nil {
		return execLeaf{}, nil, refused(err, "a resolved step arg carries a shell metacharacter; step args must stay clean")
	}
	for _, gate := range l.gates {
		if err := gate(tokens); err != nil {
			return execLeaf{}, nil, refused(err, "this step is refused by a Guardfile gate")
		}
	}
	if err := checkWhens(ctx, r.gf.Whens, l.grant, tokens, r.host); err != nil {
		return execLeaf{}, nil, refused(err, "this step is refused by a Guardfile guard")
	}
	if err := checkWhens(ctx, l.grant.Whens, l.grant, tokens, r.host); err != nil {
		return execLeaf{}, nil, refused(err, "this step is refused by a Guardfile guard")
	}
	if err := checkFlagPolicy(tokens, l.grant); err != nil {
		return execLeaf{}, nil, refused(err, "this flag is refused by the Guardfile policy")
	}
	return l, tokens, nil
}

// Plan renders one resolved step for a --dry-run: the pinned invocation with
// placeholders for data-flow refs. Fires nothing.
func (r *execStepRunner) Plan(_ context.Context, leaf stepflow.Leaf, args []guardfile.ArgBind, resolve stepflow.Resolve, sliceOf stepflow.SliceOf) (map[string]any, error) {
	if err := refuseSliceArgs(args, sliceOf); err != nil {
		return nil, err
	}
	l, ok := leaf.(execLeaf)
	if !ok {
		return nil, fmt.Errorf("execverb: step leaf is not an exec leaf (engine misuse)")
	}
	tokens, err := resolveStepArgs(args, resolve)
	if err != nil {
		return nil, err
	}
	argv := append(append(append([]string{}, r.gf.argvPrefixFor(l.grant)...), l.grant.executionArgv()...), tokens...)
	return map[string]any{
		"leaf": l.Label(),
		"exec": l.grant.ExecBin(r.gf.Bin),
		"argv": argv,
	}, nil
}

// resolveStepArgs maps each positional arg binding onto its resolved token.
func resolveStepArgs(args []guardfile.ArgBind, resolve stepflow.Resolve) ([]string, error) {
	var tokens []string
	for _, a := range args {
		v, err := resolve(a.Value)
		if err != nil {
			return nil, exitcode.New(exitcode.UserError, "user_error", err, "supply the input this step arg references")
		}
		tokens = append(tokens, v)
	}
	return tokens, nil
}

// execStepResponse builds the structured object a step's predicates and data-flow
// read: exit_code, ok, both streams, the final stdout line, and `key=val` kv tokens.
func execStepResponse(stdout, stderr []byte, code int) map[string]any {
	out := string(stdout)
	kv := map[string]any{}
	for _, f := range strings.Fields(out) {
		if k, v, ok := strings.Cut(f, "="); ok && k != "" {
			kv[k] = v
		}
	}
	return map[string]any{
		"exit_code": float64(code),
		"ok":        code == 0,
		"stdout":    out,
		"stderr":    string(stderr),
		"last_line": lastNonEmptyLine(out),
		"kv":        kv,
	}
}

// lastNonEmptyLine returns the final non-blank line of s, trimmed: the id
// handback convention for snapshot-style scripts.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// tailOf renders the most useful failure evidence: stderr when present, else
// stdout, capped so an error line stays readable.
func tailOf(stderr, stdout []byte) string {
	s := strings.TrimSpace(string(stderr))
	if s == "" {
		s = strings.TrimSpace(string(stdout))
	}
	if len(s) > 400 {
		s = "..." + s[len(s)-400:]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

// mountActions resolves and mounts the guardfile's actions as leaves under the
// group, each an audited envelope over the shared stepflow engine.
func mountActions(root *cli.Command, gf *Guardfile, wrap func(verb.Spec) cli.ActionFunc, capture CaptureRunner, host HostResolver, providers map[string]valuesource.Provider) error {
	acts, err := resolveExecActions(gf)
	if err != nil {
		return err
	}
	if len(acts) == 0 {
		return nil
	}
	runner := &execStepRunner{gf: gf, wrap: wrap, capture: capture, host: host, providers: providers}
	for _, ea := range acts {
		if findChild(root, ea.Name) != nil {
			return fmt.Errorf("execverb: action %q collides with a granted subcommand (fail-closed)", ea.Name)
		}
		root.Commands = append(root.Commands, buildExecActionLeaf(ea, runner, wrap))
	}
	return nil
}

// buildExecActionLeaf turns one resolved action into a guarded leaf: positional
// inputs become args, flag inputs flags, wrapped for the envelope audit row.
func buildExecActionLeaf(ea execAction, runner *execStepRunner, wrap func(verb.Spec) cli.ActionFunc) *cli.Command {
	flags := []cli.Flag{
		&cli.BoolFlag{Name: flagDryRun, Usage: "print the ordered action plan without firing it"},
		&cli.StringFlag{Name: flagOutput, Usage: "output format: yaml | yaml-stream | json | text | table"},
	}
	var positional []string
	for _, in := range ea.Inputs {
		if in.Positional {
			positional = append(positional, in.Name)
			continue
		}
		flags = append(flags, &cli.StringFlag{Name: in.Name, Usage: in.Help})
	}
	spec := verb.Spec{
		Name:     ea.VerbName,
		ArgsFunc: execActionArgsFunc(ea),
		Action:   runExecAction(ea, runner),
	}
	return &cli.Command{
		Name:        ea.Name,
		Usage:       execActionUsage(ea),
		Description: execActionDescription(ea),
		ArgsUsage:   strings.TrimSpace("<" + strings.Join(positional, "> <") + ">"),
		Flags:       flags,
		Action:      wrap(spec),
	}
}

// execActionArgsFunc feeds the envelope's metachar gate: every supplied input
// is argv-bound in the exec dialect, so all of them are gated.
func execActionArgsFunc(ea execAction) func(*cli.Command) (map[string]string, []string) {
	return func(c *cli.Command) (map[string]string, []string) {
		named := map[string]string{}
		positional := c.Args().Slice()
		pi := 0
		for _, in := range ea.Inputs {
			switch {
			case in.Positional:
				if pi < len(positional) {
					named[in.Name] = positional[pi]
					pi++
				}
			case c.IsSet(in.Name):
				named[in.Name] = c.String(in.Name)
			}
		}
		return named, nil
	}
}

// runExecAction binds inputs, then runs the action: --dry-run renders the plan,
// otherwise the stepflow engine drives the ordered sequence.
func runExecAction(ea execAction, runner *execStepRunner) cli.ActionFunc {
	return func(ctx context.Context, c *cli.Command) error {
		strVars, jmesVars, err := bindExecInputs(ea, c)
		if err != nil {
			return err
		}
		if c.Bool(flagDryRun) {
			return renderExecActionPlan(ctx, c, ea, strVars, runner)
		}
		bindings, lastRaw, err := stepflow.Run(ctx, c, ea.Calls, strVars, nil, runner)
		if err != nil {
			return err
		}
		if err := renderRaw(lastRaw, c.String(flagOutput)); err != nil {
			return err
		}
		if err := applyRejectEmpty(ea, lastRaw); err != nil {
			return err
		}
		return applyExecFailWhen(ea, lastRaw, stepflow.CondScope(jmesVars, bindings))
	}
}

// bindExecInputs reads inputs into strVars (raw, for argv) and jmesVars
// (coerced, for conditions). Unset optional flags bind in neither scope.
func bindExecInputs(ea execAction, c *cli.Command) (map[string]string, map[string]any, error) {
	strVars, jmesVars := map[string]string{}, map[string]any{}
	positional := c.Args().Slice()
	pi := 0
	for _, in := range ea.Inputs {
		if in.Positional {
			if pi >= len(positional) {
				if in.Required {
					return nil, nil, exitcode.New(exitcode.UserError, "user_error",
						fmt.Errorf("missing required argument <%s>", in.Name), "supply the positional arguments this action names")
				}
				continue
			}
			strVars[in.Name] = positional[pi]
			jmesVars[in.Name] = stepflow.CoerceScalar(positional[pi])
			pi++
			continue
		}
		if c.IsSet(in.Name) {
			strVars[in.Name] = c.String(in.Name)
			jmesVars[in.Name] = stepflow.CoerceScalar(c.String(in.Name))
		}
	}
	if pi < len(positional) {
		return nil, nil, exitcode.New(exitcode.UserError, "user_error",
			fmt.Errorf("got %d positional args, this action takes %d", len(positional), pi), "remove the extra arguments")
	}
	return strVars, jmesVars, nil
}

// renderExecActionPlan prints the planned ordered step sequence without firing it.
func renderExecActionPlan(ctx context.Context, c *cli.Command, ea execAction, strVars map[string]string, runner *execStepRunner) error {
	resolve := func(v string) (string, error) { return stepflow.ResolveArgDry(v, strVars), nil }
	calls, err := stepflow.PlanCalls(ctx, ea.Calls, resolve, nil, runner)
	if err != nil {
		return err
	}
	out := map[string]any{"action": ea.Name, "calls": calls}
	raw, err := json.Marshal(out)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	return renderRaw(raw, c.String(flagOutput))
}

// renderRaw prints a JSON payload through respfmt, honoring --output.
func renderRaw(raw []byte, output string) error {
	if len(raw) == 0 {
		return nil
	}
	rendered, err := respfmt.Render(raw, "", output)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "the step response was not valid JSON")
	}
	fmt.Print(string(rendered))
	return nil
}

// applyExecFailWhen evaluates fail-when against the final step response (with
// inputs and bindings as $variables); a truthy result is a non-zero exit.
func applyExecFailWhen(ea execAction, lastRaw []byte, vars map[string]any) error {
	if ea.FailWhen == "" {
		return nil
	}
	var data any
	if len(lastRaw) > 0 {
		if err := json.Unmarshal(lastRaw, &data); err != nil {
			return exitcode.New(exitcode.Internal, "internal", err, "the step response was not valid JSON")
		}
	}
	fail, err := respfmt.EvalBool(data, ea.FailWhen, vars)
	if err != nil {
		return exitcode.New(exitcode.UserError, "user_error", err, "check the `fail-when` JMESPath against the step response shape")
	}
	if fail {
		return exitcode.New(exitcode.Generic, "action_failed",
			fmt.Errorf("action %q: fail-when predicate matched", ea.Name),
			"the watched operation reported failure; inspect the output above")
	}
	return nil
}

// execActionUsage renders the one-line help: the describe note, or a default.
func execActionUsage(ea execAction) string {
	if ea.Describe != "" {
		return ea.Describe
	}
	return fmt.Sprintf("complex action: a %d-step guarded sequence", len(ea.Calls))
}

// execActionDescription is the rich per-action help body for the step sequence.
func execActionDescription(ea execAction) string {
	var b strings.Builder
	if ea.Describe != "" {
		fmt.Fprintf(&b, "%s\n\n", ea.Describe)
	}
	b.WriteString("Runs this sequence of granted steps in order; a failed step stops the sequence:\n")
	for i, s := range ea.Calls {
		fmt.Fprintf(&b, "  %d. %s", i+1, s.Leaf.Label())
		if s.As != "" {
			fmt.Fprintf(&b, " (as %s)", s.As)
		}
		b.WriteString("\n")
	}
	if ea.FailWhen != "" {
		fmt.Fprintf(&b, "\nExits non-zero when: %s\n", ea.FailWhen)
	}
	b.WriteString("\nUse --dry-run to print the plan without firing it.")
	return b.String()
}
