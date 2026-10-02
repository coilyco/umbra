package execverb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/exitcode"
)

// umbra#7329: audit derives decision=reject from PolicyDenied alone, so every
// refusal of a granted verb must carry that code or it is logged as accepted.
func runAudited(t *testing.T, src string, argv ...string) ([]audit.Record, *capture, error) {
	t.Helper()
	w := &audit.Writer{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	t.Cleanup(func() { _ = w.Close() })
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cp := &capture{}
	root := &cli.Command{Name: "ward"}
	cfg := Config{
		Guardfile: gf,
		Run:       cp.run,
		Wrap:      func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) },
	}
	if err := Mount(root, cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	runErr := root.Run(context.Background(), append([]string{"ward"}, argv...))
	data, _ := os.ReadFile(w.Path)
	records, _ := audit.ReadAll(bytes.NewReader(data))
	return records, cp, runErr
}

func TestEveryGuardfileRefusalIsPolicyDeniedAndAuditsAsReject(t *testing.T) {
	gateRegistry["test-refuse"] = func(GateSpec) gateFunc {
		return func([]string) error { return errors.New("gate refuses every call") }
	}
	t.Cleanup(func() { delete(gateRegistry, "test-refuse") })

	cases := []struct {
		name string
		src  string
		argv []string
	}{
		{"deny-flag", gitGuardfile, []string{"git", "commit", "-m", "x", "--no-verify"}},
		{"allowlist flag policy", gitGuardfile, []string{"git", "push", "--force"}},
		{"deny-when guard", awsWhenGuardfile, []string{"ops", "aws", "s3", "ls", "s3://prod-secrets-bucket"}},
		{"sealed verb given an argument", sealedGuardfile, []string{"ops", "forgejo", "read", "runner-token", "othersecret"}},
		{
			"pin conflict",
			`wrap ward ops aws { exec aws; can run "s3 ls" { pin "--region" "us-east-1" } }`,
			[]string{"ops", "aws", "s3", "ls", "--region", "eu-west-1"},
		},
		{
			"gate",
			`wrap ward ops aws { exec aws; can run "*" { gate test-refuse {} } }`,
			[]string{"ops", "aws", "s3", "ls"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records, cp, err := runAudited(t, tc.src, tc.argv...)
			coded := exitcode.From(err)
			if coded == nil || coded.Code() != exitcode.PolicyDenied || coded.Kind() != "policy_denied" {
				t.Fatalf("refusal = %v, want a coded policy_denied (code %d)", err, exitcode.PolicyDenied)
			}
			if cp.bin != "" {
				t.Errorf("refused call still executed: %s %v", cp.bin, cp.argv)
			}
			if len(records) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(records))
			}
			if records[0].Decision != audit.DecisionReject || records[0].ExitCode != exitcode.PolicyDenied {
				t.Errorf("row = decision %q exit_code %d, want reject with %d",
					records[0].Decision, records[0].ExitCode, exitcode.PolicyDenied)
			}
		})
	}
}

// A granted call is the control: reject must not be applied to everything.
func TestAGrantedCallStillAuditsAsAccept(t *testing.T) {
	records, _, err := runAudited(t, gitGuardfile, "git", "status")
	if err != nil {
		t.Fatalf("granted call refused: %v", err)
	}
	if len(records) != 1 || records[0].Decision != audit.DecisionAccept || records[0].ExitCode != 0 {
		t.Errorf("records = %+v, want one accept row with exit_code 0", records)
	}
}

// umbra#8121: a withhold or never leaf refuses before any binary, and still
// writes its reject row under its own verb name.
func TestStatedRefusalsWriteARejectRow(t *testing.T) {
	cases := []struct {
		name, src, verb string
		argv            []string
	}{
		{"withhold", withholdGuardfile, "ward.repo.delete", []string{"repo", "delete"}},
		{"never run", gitGuardfile, "ward.git.reflog.expire", []string{"git", "reflog", "expire"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records, cp, err := runAudited(t, tc.src, tc.argv...)
			if coded := exitcode.From(err); coded == nil || coded.Code() != exitcode.PolicyDenied {
				t.Fatalf("refusal = %v, want policy_denied", err)
			}
			if cp.bin != "" {
				t.Errorf("refused call reached a binary: %s %v", cp.bin, cp.argv)
			}
			if len(records) != 1 || records[0].Decision != audit.DecisionReject ||
				records[0].ExitCode != exitcode.PolicyDenied || records[0].Verb != tc.verb {
				t.Errorf("records = %+v, want one reject row for %s", records, tc.verb)
			}
		})
	}
}

// The `action run` primitive checks each step against the same policy in its
// own copy of the sequence, so it is covered separately from the leaf path.
func TestActionStepRefusalsArePolicyDenied(t *testing.T) {
	gateRegistry["test-refuse"] = func(GateSpec) gateFunc {
		return func([]string) error { return errors.New("gate refuses every step") }
	}
	t.Cleanup(func() { delete(gateRegistry, "test-refuse") })

	const apply = `can run apply { bin scp; argv "-r" }`
	withApply := func(replacement string) string {
		return strings.Replace(ecoGuardfile, apply, replacement, 1)
	}
	cases := []struct {
		name string
		src  string
		mod  string
	}{
		{"metacharacter in a resolved arg", ecoGuardfile, "Eco;rm -rf /"},
		{"step guard", withApply(`can run apply { bin scp; argv "-r"; deny-when any-arg matches "*secret*" }`), "secret-mod"},
		{"step gate", withApply(`can run apply { bin scp; argv "-r"; gate test-refuse {} }`), "Eco"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &scriptedCapture{}
			err := runAction(t, tc.src, rec.run, "promote", tc.mod)
			coded := exitcode.From(err)
			if coded == nil || coded.Code() != exitcode.PolicyDenied || coded.Kind() != "policy_denied" {
				t.Fatalf("step refusal = %v, want a coded policy_denied (code %d)", err, exitcode.PolicyDenied)
			}
			for _, c := range rec.calls {
				if c.bin == "scp" {
					t.Errorf("the refused apply step still spawned: %v", rec.calls)
				}
			}
		})
	}
}

// umbra#8121: a closed replacement refuses an ungranted verb or root flag
// before any leaf, and the fallback's wrap still writes the reject row.
func TestReplacementUnmatchedRefusalsWriteARejectRow(t *testing.T) {
	gf, err := Parse([]byte(`wrap gh { exec gh; replace; can run status }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	w := &audit.Writer{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	t.Cleanup(func() { _ = w.Close() })
	fb, err := NewFallback(Config{Guardfile: gf, Wrap: func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) }})
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}
	ctx := context.Background()
	for _, e := range []error{
		fb.refuse(ctx, nil, "repo", RefuseUngranted(gf, "repo")),
		RootFlagFallback(ctx, gf, fb, errors.New("flag provided but not defined: -x")),
	} {
		if c := exitcode.From(e); c == nil || c.Code() != exitcode.PolicyDenied {
			t.Fatalf("refusal = %v, want policy_denied", e)
		}
	}
	data, _ := os.ReadFile(w.Path)
	records, _ := audit.ReadAll(bytes.NewReader(data))
	if len(records) != 2 {
		t.Fatalf("audit rows = %d, want one per refusal", len(records))
	}
	for i, want := range []string{"gh.repo", "gh.(root flag)"} {
		if r := records[i]; r.Decision != audit.DecisionReject || r.Verb != want {
			t.Errorf("row %d = %+v, want a reject row for %s", i, r, want)
		}
	}
}

// umbra#8162: a call forwarded under default-allow writes its own row, accept
// when it runs, reject when a wrap-level guard refuses it.
func TestForwardedCallsWriteAnAuditRow(t *testing.T) {
	gf, err := Parse([]byte(`wrap gh {
		exec gh
		replace
		default-allow { reason "test fixture" }
		never pass delete
		withhold pr merge { reason "fixture" }
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	w := &audit.Writer{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	t.Cleanup(func() { _ = w.Close() })
	var cp capture
	fb, err := NewFallback(Config{Guardfile: gf, Run: cp.run,
		Wrap: func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) }})
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}
	ctx := context.Background()
	if err := fb.forwardAudited(ctx, nil, []string{"repo", "view"}); err != nil {
		t.Fatalf("forwarded call refused: %v", err)
	}
	if cp.bin == "" {
		t.Fatal("forwarded call never reached the runner")
	}
	if err := fb.forwardAudited(ctx, nil, []string{"repo", "delete"}); exitcode.From(err) == nil {
		t.Fatalf("guarded forward = %v, want a coded refusal", err)
	}
	data, _ := os.ReadFile(w.Path)
	records, _ := audit.ReadAll(bytes.NewReader(data))
	if len(records) != 2 {
		t.Fatalf("audit rows = %d, want one per forwarded call", len(records))
	}
	if r := records[0]; r.Decision != audit.DecisionAccept || r.Verb != "gh.repo" {
		t.Errorf("row 0 = %+v, want accept for gh.repo", r)
	}
	if r := records[1]; r.Decision != audit.DecisionReject || r.Verb != "gh.repo" {
		t.Errorf("row 1 = %+v, want reject for gh.repo", r)
	}
}
