package execverb

import (
	"context"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/urfave/cli/v3"
)

// sequenceGuardfile exercises ordered exec steps, a transport override, and
// `$step.field` data-flow without embedding deployment policy.
const ecoGuardfile = `wrap ward-kdl ops eco server {
	exec ssh {
		argv-prefix "kai@kai-server"
	}
	can run snapshot { argv bash "/scripts/snapshot.sh" }
	can run apply { bin scp; argv "-r" }
	can run restart { argv bash "/scripts/restart.sh" }
	can run health { argv bash "/scripts/health.sh" }
	action promote {
		describe "snapshot, apply, restart, health"
		input mod { positional; required; help "mod name" }
		call run snapshot {
			as snap
		}
		call run apply { args "$mod" }
		call run restart
		call run health
	}
}`

// capturedCall records one CaptureRunner invocation.
type capturedCall struct {
	bin  string
	argv []string
}

// scriptedCapture fakes step commands: outputs and exits keyed by an argv
// substring, recording every call.
type scriptedCapture struct {
	calls []capturedCall
}

func (s *scriptedCapture) run(_ context.Context, bin string, argv, _ []string) ([]byte, []byte, int, error) {
	s.calls = append(s.calls, capturedCall{bin: bin, argv: append([]string{}, argv...)})
	joined := strings.Join(argv, " ")
	switch {
	case strings.Contains(joined, "snapshot.sh"):
		return []byte(">>> copying\nsnap-20260703-1\n"), nil, 0, nil
	case strings.Contains(joined, "health.sh"):
		out := "service_active=1 journal_clean=1 server_ready=1"
		return []byte(out), nil, 0, nil
	default:
		return []byte("ok"), nil, 0, nil
	}
}

// binsOf projects the recorded calls onto "bin:lastScript" labels for ordering
// assertions.
func binsOf(calls []capturedCall) []string {
	var out []string
	for _, c := range calls {
		label := c.bin
		for _, a := range c.argv {
			if strings.Contains(a, ".sh") {
				label += ":" + a[strings.LastIndex(a, "/")+1:]
			}
		}
		out = append(out, label)
	}
	return out
}

// runAction mounts the guardfile with the given capture and runs the action.
func runAction(t *testing.T, src string, capture CaptureRunner, argv ...string) error {
	t.Helper()
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	group, err := Build(Config{Guardfile: gf, RunCapture: capture})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	root := &cli.Command{Name: "ward", Commands: []*cli.Command{group}}
	return root.Run(context.Background(), append([]string{"ward", "server"}, argv...))
}

// TestExecActionGreenPath proves the full sequence fires in order over the pinned
// transport, and the scp step drops the ssh argv-prefix.
func TestExecActionGreenPath(t *testing.T) {
	rec := &scriptedCapture{}
	if err := runAction(t, ecoGuardfile, rec.run, "promote", "EcoTelemetry"); err != nil {
		t.Fatalf("green promote: %v", err)
	}
	got := binsOf(rec.calls)
	want := []string{"ssh:snapshot.sh", "scp", "ssh:restart.sh", "ssh:health.sh"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	// the ssh transport prefix pins every ssh step; the scp override drops it
	if rec.calls[0].argv[0] != "kai@kai-server" {
		t.Errorf("snapshot argv = %v, want the ssh argv-prefix first", rec.calls[0].argv)
	}
	if got := strings.Join(rec.calls[1].argv, " "); got != "-r EcoTelemetry" {
		t.Errorf("apply argv = %q, want the scp override without the ssh prefix", got)
	}
}

// TestExecActionStepFailureStopsSequence proves a failing step does not run a
// later call or infer a recovery action.
func TestExecActionStepFailureStopsSequence(t *testing.T) {
	src := strings.Replace(ecoGuardfile, `can run restart { argv bash "/scripts/restart.sh" }`,
		`can run restart { argv bash "/scripts/fail.sh" }`, 1)
	rec := &scriptedCapture{}
	failing := &failWrapCapture{inner: rec}
	err := runAction(t, src, failing.run, "promote", "EcoTelemetry")
	if err == nil {
		t.Fatal("expected a failed action, got nil")
	}
	coded := exitcode.From(err)
	if coded == nil || coded.Kind() != "action_failed" {
		t.Fatalf("error = %v, want action_failed", err)
	}
	if got := binsOf(rec.calls); strings.Join(got, ",") != "ssh:snapshot.sh,scp,ssh:fail.sh" {
		t.Errorf("calls = %v, want sequence to stop at fail.sh", got)
	}
}

// failWrapCapture fails any step whose argv names fail.sh, delegating the rest.
type failWrapCapture struct{ inner *scriptedCapture }

func (f *failWrapCapture) run(ctx context.Context, bin string, argv, env []string) ([]byte, []byte, int, error) {
	if strings.Contains(strings.Join(argv, " "), "fail.sh") {
		f.inner.calls = append(f.inner.calls, capturedCall{bin: bin, argv: argv})
		return nil, []byte("boom"), 7, nil
	}
	return f.inner.run(ctx, bin, argv, env)
}

// TestExecActionDryRunFiresNothing proves --dry-run renders the plan without
// spawning a single step command.
func TestExecActionDryRunFiresNothing(t *testing.T) {
	rec := &scriptedCapture{}
	if err := runAction(t, ecoGuardfile, rec.run, "promote", "EcoTelemetry", "--dry-run"); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("dry-run fired %d step(s), want 0", len(rec.calls))
	}
}

// TestExecActionMetacharInputRefused proves the step-layer policy gate refuses a
// metacharacter-carrying arg before spawn.
func TestExecActionMetacharInputRefused(t *testing.T) {
	rec := &scriptedCapture{}
	err := runAction(t, ecoGuardfile, rec.run, "promote", "Eco;rm -rf /")
	if err == nil {
		t.Fatal("expected the metachar gate to refuse, got nil")
	}
	for _, c := range rec.calls {
		if c.bin == "scp" {
			t.Fatalf("the gated apply step still spawned: %v", rec.calls)
		}
	}
	if got := binsOf(rec.calls); strings.Join(got, ",") != "ssh:snapshot.sh" {
		t.Errorf("calls = %v, want sequence to stop before apply", got)
	}
}

// TestExecActionUnknownGrantFailsClosed proves a step naming an ungranted verb
// fails at Build, deny-by-default.
func TestExecActionUnknownGrantFailsClosed(t *testing.T) {
	src := strings.Replace(ecoGuardfile, "call run restart", "call run reboot", 1)
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Build(Config{Guardfile: gf}); err == nil || !strings.Contains(err.Error(), "deny-by-default") {
		t.Fatalf("Build err = %v, want deny-by-default", err)
	}
}

// TestExecActionSealedStepRejectsArgs proves a sealed grant cannot take step
// args (the seal pins the whole invocation).
func TestExecActionSealedStepRejectsArgs(t *testing.T) {
	src := strings.Replace(ecoGuardfile, `can run apply { bin scp; argv "-r" }`,
		`can run apply { bin scp; argv "-r"; sealed }`, 1)
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Build(Config{Guardfile: gf}); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("Build err = %v, want a sealed-step refusal", err)
	}
}

// TestExecActionNamedArgsRejected proves the exec dialect refuses the spec
// dialect's named `args { name value }` form on steps (positional only).
func TestExecActionNamedArgsRejected(t *testing.T) {
	src := strings.Replace(ecoGuardfile, `call run apply { args "$mod" }`,
		`call run apply { args { id "$mod" } }`, 1)
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Build(Config{Guardfile: gf}); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("Build err = %v, want a positional-args refusal", err)
	}
}

// TestActionUnderPassthroughRefused proves the funnel sugar cannot carry
// actions (they compose named grants).
func TestActionUnderPassthroughRefused(t *testing.T) {
	src := `wrap ward-kdl ssh {
		passthrough ssh
		action promote { call run snapshot }
	}`
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "passthrough") {
		t.Fatalf("Parse err = %v, want a passthrough refusal", err)
	}
}

// TestBinOverrideParsesAndMounts proves the per-grant `bin` override survives
// parse and shows in the leaf usage without the wrap transport prefix.
func TestBinOverrideParsesAndMounts(t *testing.T) {
	gf, err := Parse([]byte(ecoGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var apply *Grant
	for i := range gf.Grants {
		if gf.Grants[i].subcommandLabel() == "apply" {
			apply = &gf.Grants[i]
		}
	}
	if apply == nil || apply.Bin != "scp" {
		t.Fatalf("apply grant bin = %+v, want scp", apply)
	}
	if u := leafUsage(gf, *apply); !strings.HasPrefix(u, "exec: scp -r") {
		t.Errorf("usage = %q, want the scp invocation without the ssh prefix", u)
	}
}
