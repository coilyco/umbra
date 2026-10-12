package execverb

import (
	"context"
	"strings"
	"testing"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/urfave/cli/v3"
)

// runGated mounts src under the real verb pipeline. The default Config wrap
// skips the metacharacter gate, so only this path can see it fire or lift.
func runGated(t *testing.T, src string, cp *capture, argv ...string) error {
	t.Helper()
	gf := parseOrFail(t, src)
	root := &cli.Command{Name: "ward"}
	cfg := Config{Guardfile: gf, Run: cp.run, Wrap: func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, nil) }}
	if err := Mount(root, cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return root.Run(context.Background(), append([]string{"ward"}, argv...))
}

// The shape allow-metacharacters exists for: a binary that gets argv through
// execve, where a commit message legitimately carries `(`, `;` and `<`.
const allowMetaGuardfile = `wrap ward git {
	exec git
	allow-metacharacters {
		reason "git receives argv through execve and never a shell"
	}
	can run commit {
		deny-flag "--no-verify" aliases="-n"
	}
	can run log
}`

func TestAllowMetacharactersParsesItsReason(t *testing.T) {
	gf := parseOrFail(t, allowMetaGuardfile)

	if !gf.AllowMeta.Declared {
		t.Fatal("`allow-metacharacters` must mark the guardfile")
	}
	if !strings.Contains(gf.AllowMeta.Reason, "execve") {
		t.Errorf("reason = %q, want the authored sentence", gf.AllowMeta.Reason)
	}
}

func TestAllowMetacharactersNeedsAReason(t *testing.T) {
	// Lifting a gate is a decision, and an undefended one reads as an accident.
	for name, node := range map[string]string{
		"bare":          "allow-metacharacters",
		"empty block":   "allow-metacharacters {}",
		"wrong child":   `allow-metacharacters { why "x" }`,
		"param names":   `allow-metacharacters "message"`,
		"duplicate":     `allow-metacharacters { reason "a"; reason "b" }`,
		"two arguments": `allow-metacharacters { reason "a" "b" }`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte("wrap ward git {\n\texec git\n\t" + node + "\n\tcan run log\n}"))
			if err == nil || !strings.Contains(err.Error(), "allow-metacharacters") {
				t.Fatalf("err = %v, want a refusal naming allow-metacharacters", err)
			}
		})
	}
}

func TestMetacharactersAreRefusedUnlessTheWrapAllowsThem(t *testing.T) {
	const closed = `wrap ward git {
	exec git
	can run commit
}`
	var cp capture
	err := runGated(t, closed, &cp, "git", "commit", "-m", "feat(x): y <a@b>; z")
	if err == nil || !strings.Contains(err.Error(), "metacharacter") {
		t.Fatalf("err = %v, want the gate to refuse without the declaration", err)
	}
	if cp.bin != "" {
		t.Errorf("a gated call still executed: %s %v", cp.bin, cp.argv)
	}
}

func TestAllowMetacharactersLetsTheBytesReachTheBinary(t *testing.T) {
	var cp capture
	msg := "feat(x): `y` $(z) <a@b>; c | d & e"
	if err := runGated(t, allowMetaGuardfile, &cp, "git", "commit", "-m", msg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "git" || len(cp.argv) != 3 || cp.argv[2] != msg {
		t.Errorf("ran %s %q, want the message to arrive verbatim", cp.bin, cp.argv)
	}
}

// The declaration lifts one gate. Every other refusal still binds the same call.
func TestAllowMetacharactersLeavesTheFlagPolicyBinding(t *testing.T) {
	for _, flag := range []string{"--no-verify", "-n", "-an"} {
		t.Run(flag, func(t *testing.T) {
			var cp capture
			err := runGated(t, allowMetaGuardfile, &cp, "git", "commit", flag, "-m", "feat(x): y <a@b>")
			if err == nil {
				t.Fatal("a denied flag must still refuse beside a metacharacter message")
			}
			if cp.bin != "" {
				t.Errorf("denied invocation still executed: %s %v", cp.bin, cp.argv)
			}
		})
	}
}

func TestAllowMetacharactersReachesAnInspectListLeaf(t *testing.T) {
	var cp capture
	src := `wrap ward inspect {
	allow grep
	allow-metacharacters {
		reason "grep patterns carry regex bytes and grep never hands them to a shell"
	}
}`
	if err := runGated(t, src, &cp, "inspect", "grep", "a|b(c)", "f"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "grep" {
		t.Errorf("bin = %q, want grep", cp.bin)
	}
}

func TestDescribeNamesTheLiftedGate(t *testing.T) {
	s := Describe(parseOrFail(t, allowMetaGuardfile))

	if !strings.Contains(s.AllowMetacharacters, "execve") {
		t.Errorf("describe lost the reason: %q", s.AllowMetacharacters)
	}
}
