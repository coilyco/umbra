// Package stepflow executes transport-agnostic ordered step sequences.
// A dialect resolves its authorized leaves into Steps and supplies a Runner;
// stepflow preserves their order and threads explicit `as` bindings between
// them. It deliberately does not attach deployment policy to a sequence.
package stepflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/guardfile"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/respfmt"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
	"github.com/urfave/cli/v3"
)

// Leaf is one resolved, authorized step target. The Runner that resolved it
// knows its concrete type (for example, an HTTP operation or exec grant).
type Leaf interface {
	Label() string
}

// Step is one resolved call in sequence order. As makes its decoded response
// available to later arguments as `$as.field`.
type Step struct {
	Leaf Leaf
	Args []guardfile.ArgBind
	As   string
}

// Resolve maps one argument value (a literal, $input, or $step.field) to its
// current bound string.
type Resolve func(string) (string, error)

// SliceOf reports whether an argument value references a list-valued input, and
// with what elements. It is consulted before Resolve, so a list never reaches
// the scalar path and flattens there unnoticed.
type SliceOf func(string) ([]string, bool)

// Runner fires or plans one already-resolved step.
type Runner interface {
	Fire(ctx context.Context, c *cli.Command, leaf Leaf, args []guardfile.ArgBind,
		resolve Resolve, sliceOf SliceOf) (decoded any, raw []byte, err error)
	Plan(ctx context.Context, leaf Leaf, args []guardfile.ArgBind,
		resolve Resolve, sliceOf SliceOf) (map[string]any, error)
}

// SliceRefs builds a SliceOf over the list-valued inputs bound for this run. A
// step-output reference (`$step.field`) is never a list: only a declared
// `array` input is.
func SliceRefs(sliceVars map[string][]string) SliceOf {
	return func(value string) ([]string, bool) {
		if len(sliceVars) == 0 || !strings.HasPrefix(value, "$") {
			return nil, false
		}
		v, ok := sliceVars[strings.TrimPrefix(value, "$")]
		return v, ok
	}
}

// Run executes steps in order and returns the successfully completed prefix.
// A failure stops the sequence; it does not infer a recovery action.
func Run(ctx context.Context, c *cli.Command, steps []Step, strVars map[string]string, sliceVars map[string][]string, r Runner) (bindings map[string]any, lastRaw []byte, err error) {
	bindings = map[string]any{}
	sliceOf := SliceRefs(sliceVars)
	for i, step := range steps {
		resolve := func(v string) (string, error) { return ResolveArg(v, strVars, bindings) }
		decoded, raw, ferr := r.Fire(ctx, c, step.Leaf, step.Args, resolve, sliceOf)
		if ferr != nil {
			return bindings, lastRaw, exitcode.New(exitcode.UpstreamFailed, "action_failed",
				fmt.Errorf("call %d (%s): %w", i+1, step.Leaf.Label(), ferr),
				"a step in the action sequence failed; no later steps ran")
		}
		lastRaw = raw
		if step.As != "" {
			bindings[step.As] = decoded
		}
	}
	return bindings, lastRaw, nil
}

// PlanCalls renders each step for a dry run. Data-flow references are resolved
// by the supplied resolver, which normally preserves future bindings as hints.
func PlanCalls(ctx context.Context, steps []Step, resolve Resolve, sliceOf SliceOf, r Runner) ([]any, error) {
	plan := make([]any, 0, len(steps))
	for _, step := range steps {
		stepPlan, err := r.Plan(ctx, step.Leaf, step.Args, resolve, sliceOf)
		if err != nil {
			return nil, err
		}
		if step.As != "" {
			stepPlan["as"] = step.As
		}
		plan = append(plan, stepPlan)
	}
	return plan, nil
}

// ResolveArg resolves a step argument: a literal, a `$input`, or a
// `$step.field` projection from a completed step response.
func ResolveArg(value string, inputs map[string]string, bindings map[string]any) (string, error) {
	if !strings.HasPrefix(value, "$") {
		return value, nil
	}
	ref := strings.TrimPrefix(value, "$")
	if dot := strings.IndexByte(ref, '.'); dot >= 0 {
		head, path := ref[:dot], ref[dot+1:]
		data, ok := bindings[head]
		if !ok {
			return "", fmt.Errorf("$%s: step %q is not bound (it runs later or sets no `as`)", ref, head)
		}
		out, err := respfmt.Eval(data, path, nil)
		if err != nil {
			return "", fmt.Errorf("$%s: %w", ref, err)
		}
		if out == nil {
			return "", fmt.Errorf("$%s resolved to null in the prior response", ref)
		}
		return ScalarToString(out), nil
	}
	if v, ok := inputs[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("$%s: %w", ref, ErrUnsetOptional)
}

// ErrUnsetOptional reports a `$name` naming an input the caller omitted. Build
// validation rejects an undeclared ref, so a caller may drop the arg. #326.
var ErrUnsetOptional = errors.New("optional input was not supplied")

// ResolveArgDry resolves an argument for a dry-run plan. Unavailable step
// output is left as a `${ref}` placeholder.
func ResolveArgDry(value string, inputs map[string]string) string {
	if !strings.HasPrefix(value, "$") {
		return value
	}
	ref := strings.TrimPrefix(value, "$")
	if !strings.Contains(ref, ".") {
		if v, ok := inputs[ref]; ok {
			return v
		}
	}
	return "${" + ref + "}"
}

// UnsetInputRef reports whether value is a bare `$name` naming an input the
// caller did not supply, so a dry plan can drop it exactly as a live call does.
func UnsetInputRef(value string, inputs map[string]string) bool {
	if !strings.HasPrefix(value, "$") {
		return false
	}
	ref := strings.TrimPrefix(value, "$")
	if strings.Contains(ref, ".") {
		return false
	}
	_, ok := inputs[ref]
	return !ok
}

// CondScope merges coerced inputs with completed step bindings. A binding wins
// over an equally named input.
func CondScope(jmesVars, bindings map[string]any) map[string]any {
	out := make(map[string]any, len(jmesVars)+len(bindings))
	for k, v := range jmesVars {
		out[k] = v
	}
	for k, v := range bindings {
		out[k] = v
	}
	return out
}

// CoerceScalar lowers numeric inputs to the JSON number shape used by JMESPath.
func CoerceScalar(s string) any {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return float64(i)
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// ScalarToString renders a JMESPath scalar for a later argument.
func ScalarToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}
