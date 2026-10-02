// Exec-dialect complex actions: ordered call sequences over granted exec leaves
// through the shared generic stepflow engine. See docs/execverb.md.

package execverb

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/respfmt"
	"github.com/coilyco/umbra/pkg/stepflow"
)

// CaptureRunner fires a step command and captures its output (unlike Runner,
// which streams). err is a spawn failure; a non-zero exit returns exitCode.
type CaptureRunner func(ctx context.Context, bin string, argv, env []string) (stdout, stderr []byte, exitCode int, err error)

// realCapture is the production CaptureRunner: it execs the command and
// captures both streams, mapping an exec.ExitError onto its exit code.
func realCapture(ctx context.Context, bin string, argv, env []string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, bin, argv...)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), errBuf.Bytes(), 0, nil
	}
	var xerr *exec.ExitError
	if ok := errorsAs(err, &xerr); ok {
		return out.Bytes(), errBuf.Bytes(), xerr.ExitCode(), nil
	}
	return out.Bytes(), errBuf.Bytes(), -1, err
}

// errorsAs is a tiny indirection so realCapture reads flat; errors.As inline.
func errorsAs(err error, target *(*exec.ExitError)) bool {
	x, ok := err.(*exec.ExitError) //nolint:errorlint // CommandContext returns it unwrapped
	if ok {
		*target = x
	}
	return ok
}

// execLeaf is one resolved action step target: a granted exec leaf plus its
// preflight gates. It satisfies stepflow.Leaf.
type execLeaf struct {
	gf    *Guardfile
	grant Grant
	gates []gateFunc
}

// Label names the leaf in engine errors, e.g. "snapshot".
func (l execLeaf) Label() string { return l.grant.subcommandLabel() }

// verbName is the leaf's dotted audit identity, e.g. ward.ops.eco.server.snapshot.
func (l execLeaf) verbName() string {
	return strings.Join(l.gf.Group, ".") + "." + strings.Join(l.grant.Subcommand, ".")
}

// execAction is one resolved exec-dialect complex action.
type execAction struct {
	Name     string
	VerbName string
	Describe string
	Inputs   []guardfile.Input
	Calls    []stepflow.Step
	FailWhen string
	// RejectEmpty refuses an answer carrying nothing; see rejectempty.go.
	RejectEmpty bool
}

// resolveExecActions resolves every declared action against the named grants,
// failing closed: exec v1 runs named call actions only (no poll/collect/mount).
func resolveExecActions(gf *Guardfile) ([]execAction, error) {
	var out []execAction
	for _, a := range gf.Actions {
		ea, err := resolveExecAction(gf, a)
		if err != nil {
			return nil, err
		}
		out = append(out, ea)
	}
	return out, nil
}

// resolveExecAction resolves one action's inputs and ordered steps, fail-closed.
func resolveExecAction(gf *Guardfile, a guardfile.Action) (execAction, error) {
	inputNames, err := validateExecActionHeader(a)
	if err != nil {
		return execAction{}, err
	}
	ea := execAction{
		Name:     a.Name,
		VerbName: strings.Join(gf.Group, ".") + ".action." + a.Name,
		Describe: a.Describe, Inputs: a.Inputs, FailWhen: a.FailWhen,
		RejectEmpty: a.RejectEmpty,
	}
	bound := map[string]bool{}
	for i, call := range a.Calls {
		step, err := resolveExecStep(gf, a.Name, i, call, inputNames, bound)
		if err != nil {
			return execAction{}, err
		}
		ea.Calls = append(ea.Calls, step)
		if call.As != "" {
			bound[call.As] = true
		}
	}
	return ea, nil
}

// validateExecActionHeader gates the action's shape for the exec dialect (call
// actions only, no mount form, no pre-flight defaults) and collects its inputs.
func validateExecActionHeader(a guardfile.Action) (map[string]bool, error) {
	if a.Poll != nil || a.Collect != nil {
		return nil, fmt.Errorf("execverb: action %q: the exec dialect runs `call` actions only (no poll/collect; fail-closed)", a.Name)
	}
	if a.IsMount() {
		return nil, fmt.Errorf("execverb: action %q: the mount form is not supported in the exec dialect (name the action; fail-closed)", a.MountVerb)
	}
	inputNames := map[string]bool{}
	for _, in := range a.Inputs {
		if in.Default != "" {
			return nil, fmt.Errorf("execverb: action %q: input %q: `default` is a spec-dialect pre-flight binding (fail-closed)", a.Name, in.Name)
		}
		inputNames[in.Name] = true
	}
	if a.FailWhen != "" {
		if err := respfmt.Validate(a.FailWhen); err != nil {
			return nil, fmt.Errorf("execverb: action %q: fail-when: %w", a.Name, err)
		}
	}
	return inputNames, nil
}

// resolveExecStep resolves one call step onto a granted exec leaf with
// positional args validated.
func resolveExecStep(gf *Guardfile, action string, i int, call guardfile.Call, inputNames, bound map[string]bool) (stepflow.Step, error) {
	label := fmt.Sprintf("call %d", i+1)
	leaf, err := resolveExecLeaf(gf, action, label, call.Verb, call.Resource, call.Args)
	if err != nil {
		return stepflow.Step{}, err
	}
	if err := validateExecArgs(action, label, call.Args, inputNames, bound); err != nil {
		return stepflow.Step{}, err
	}
	return stepflow.Step{Leaf: leaf, Args: call.Args, As: call.As}, nil
}

// resolveExecLeaf recovers the granted exec leaf a step names: `<verb>` must be
// `run` and `<resource>` a named `can run` grant. Deny-by-default.
func resolveExecLeaf(gf *Guardfile, action, label, verbTok, resource string, args []guardfile.ArgBind) (execLeaf, error) {
	if verbTok != "run" {
		return execLeaf{}, fmt.Errorf("execverb: action %q %s: steps read `call run <grant>`, got %q (fail-closed)", action, label, verbTok)
	}
	want := strings.Fields(resource)
	for _, g := range gf.Grants {
		if g.Wildcard || !equalTokens(g.Subcommand, want) {
			continue
		}
		if g.Sealed && len(args) > 0 {
			return execLeaf{}, fmt.Errorf("execverb: action %q %s: grant %q is sealed and takes no step args (fail-closed)", action, label, resource)
		}
		gates, err := buildGates(g)
		if err != nil {
			return execLeaf{}, err
		}
		return execLeaf{gf: gf, grant: g, gates: gates}, nil
	}
	return execLeaf{}, fmt.Errorf("execverb: action %q %s: no `can run %s` grant authorizes it (deny-by-default)", action, label, resource)
}

// validateExecArgs checks a step's args are positional (`args "<v>" ...`) and
// each `$ref` names a declared input or a bound `as`. Fail-closed.
func validateExecArgs(action, where string, args []guardfile.ArgBind, inputNames, bound map[string]bool) error {
	for _, arg := range args {
		if arg.Name != "" {
			return fmt.Errorf("execverb: action %q %s: exec step args are positional (`args \"<value>\" ...`), got named arg %q (fail-closed)", action, where, arg.Name)
		}
		if !strings.HasPrefix(arg.Value, "$") {
			continue
		}
		ref := strings.TrimPrefix(arg.Value, "$")
		if dot := strings.IndexByte(ref, '.'); dot >= 0 {
			if !bound[ref[:dot]] {
				return fmt.Errorf("execverb: action %q %s: $%s references a step no `as` in scope binds (fail-closed)", action, where, ref)
			}
			continue
		}
		if !inputNames[ref] {
			return fmt.Errorf("execverb: action %q %s: $%s references an undeclared input (fail-closed)", action, where, ref)
		}
	}
	return nil
}

// equalTokens reports whether two token paths are identical.
func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
