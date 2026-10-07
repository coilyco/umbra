package execverb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/valuesource"
	"github.com/urfave/cli/v3"
)

const gitGuardfile = `wrap ward git {
	exec git

	can run status
	can run log
	can run commit {
		deny-flag "--no-verify" aliases="-n"
		describe "record staged changes"
	}
	can run push {
		allow-flag "--force-with-lease"
	}
	never run "reflog expire"
}`

const adminGuardfile = `wrap ward ops forgejo admin {
	exec ssh {
		argv-prefix "kai@kai-server" "k3s" "kubectl" "-n" "forgejo" "exec" "deploy/forgejo" "--" "forgejo"
	}
	can run "admin user list"
	can run "doctor check" {
		allow-flag "--run"
	}
}`

// capture is a Runner recording the resolved invocation.
type capture struct {
	bin  string
	argv []string
	env  []string
}

func (cp *capture) run(_ context.Context, bin string, argv, env []string) error {
	cp.bin = bin
	cp.argv = argv
	cp.env = env
	return nil
}

func runArgv(t *testing.T, src string, cp *capture, argv ...string) error {
	t.Helper()
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	root := &cli.Command{Name: "ward"}
	if err := Mount(root, Config{Guardfile: gf, Run: cp.run}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return root.Run(context.Background(), append([]string{"ward"}, argv...))
}

func TestMountsOnlyGrantedSubcommands(t *testing.T) {
	gf, err := Parse([]byte(gitGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	root, err := Build(Config{Guardfile: gf})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range []string{"status", "log", "commit", "push"} {
		if findChild(root, want) == nil {
			t.Errorf("missing granted leaf %q", want)
		}
	}
	// deny-by-default: rebase was never granted. The `never` is asserted by its
	// refusal in never_test.go, since an unmounted path proves nothing about it.
	if findChild(root, "rebase") != nil {
		t.Error("ungranted subcommand mounted")
	}
}

func TestExecsFixedBinAndArgs(t *testing.T) {
	var cp capture
	if err := runArgv(t, gitGuardfile, &cp, "git", "log", "--oneline", "-5"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "git" {
		t.Errorf("bin = %q, want git", cp.bin)
	}
	if got := strings.Join(cp.argv, " "); got != "log --oneline -5" {
		t.Errorf("argv = %q, want log --oneline -5", got)
	}
}

func TestDenyFlagRefused(t *testing.T) {
	var cp capture
	err := runArgv(t, gitGuardfile, &cp, "git", "commit", "-m", "x", "--no-verify")
	if err == nil {
		t.Fatal("expected a deny-flag refusal, got nil")
	}
	if cp.bin != "" {
		t.Errorf("denied invocation still executed: %s %v", cp.bin, cp.argv)
	}
}

// COI-1899: git takes -n for --no-verify, a bundle like -an, and --no-verif.
func TestDenyFlagRefusesEverySpelling(t *testing.T) {
	for _, flag := range []string{"--no-verify", "-n", "-an", "-nm", "--no-verif", "--no-v", "--no-verify=1"} {
		t.Run(flag, func(t *testing.T) {
			var cp capture
			err := runArgv(t, gitGuardfile, &cp, "git", "commit", flag, "x")
			if err == nil {
				t.Fatalf("git commit %s was forwarded, want a deny-flag refusal", flag)
			}
			if !strings.Contains(err.Error(), "is denied for `commit`") {
				t.Errorf("refusal = %q, want the deny-flag text", err)
			}
			if cp.bin != "" {
				t.Errorf("denied invocation still executed: %s %v", cp.bin, cp.argv)
			}
		})
	}
}

func TestDenyFlagLeavesUnrelatedFlagsAlone(t *testing.T) {
	for _, flag := range []string{"-m", "-a", "--amend", "--no-edit", "--message=verify", "-s"} {
		t.Run(flag, func(t *testing.T) {
			var cp capture
			if err := runArgv(t, gitGuardfile, &cp, "git", "commit", flag, "x"); err != nil {
				t.Fatalf("git commit %s refused: %v", flag, err)
			}
		})
	}
}

// No alias table is readable, so an unaccounted-for deny-flag fails to parse.
func TestDenyFlagWithoutAliasesFailsToParse(t *testing.T) {
	cases := map[string]struct{ node, want string }{
		"no aliases declared": {`deny-flag "--no-verify"`, "alias"},
		"unknown property":    {`deny-flag "--no-verify" alias="-n"`, "unknown property"},
		"not a flag":          {`deny-flag "no-verify" aliases="none"`, "flag spelling"},
		"alias not a flag":    {`deny-flag "--no-verify" aliases="n"`, "flag spelling"},
		"empty alias":         {`deny-flag "--no-verify" aliases="-n,"`, "flag spelling"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte("wrap ward git {\n exec git\n can run commit { " + tc.node + " }\n}"))
			if err == nil {
				t.Fatal("Parse accepted a deny-flag that does not account for its aliases")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDenyFlagNoneDeniesOnlyTheNamedSpelling(t *testing.T) {
	src := "wrap ward git {\n exec git\n can run commit { deny-flag \"--amend\" aliases=\"none\" }\n}"
	var cp capture
	if err := runArgv(t, src, &cp, "git", "commit", "--amend"); err == nil {
		t.Fatal("--amend was forwarded")
	}
	if err := runArgv(t, src, &cp, "git", "commit", "-n"); err != nil {
		t.Fatalf("-n refused under a deny-flag that named only --amend: %v", err)
	}
}

func TestAllowlistMode(t *testing.T) {
	var cp capture
	if err := runArgv(t, gitGuardfile, &cp, "git", "push", "--force-with-lease"); err != nil {
		t.Fatalf("allowlisted flag refused: %v", err)
	}
	err := runArgv(t, gitGuardfile, &cp, "git", "push", "--force")
	if err == nil {
		t.Fatal("expected an allowlist refusal for --force, got nil")
	}
}

func TestArgvPrefixIsFixed(t *testing.T) {
	var cp capture
	if err := runArgv(t, adminGuardfile, &cp, "ops", "forgejo", "admin", "admin", "user", "list"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "ssh" {
		t.Errorf("bin = %q, want ssh", cp.bin)
	}
	want := "kai@kai-server k3s kubectl -n forgejo exec deploy/forgejo -- forgejo admin user list"
	if got := strings.Join(cp.argv, " "); got != want {
		t.Errorf("argv = %q,\nwant   %q", got, want)
	}
}

func TestMultiWordLeafWithFlag(t *testing.T) {
	var cp capture
	if err := runArgv(t, adminGuardfile, &cp, "ops", "forgejo", "admin", "doctor", "check", "--run", "storages"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Join(cp.argv, " "); !strings.HasSuffix(got, "forgejo doctor check --run storages") {
		t.Errorf("argv = %q, want the doctor check suffix", got)
	}
}

func TestComposesWithVerbWrap(t *testing.T) {
	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(func() { _ = w.Close() })
	gf, err := Parse([]byte(gitGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var cp capture
	root := &cli.Command{Name: "ward"}
	cfg := Config{
		Guardfile: gf,
		Run:       cp.run,
		Wrap:      func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) },
	}
	if err := Mount(root, cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := root.Run(context.Background(), []string{"ward", "git", "status"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, _ := os.ReadFile(w.Path)
	if !strings.Contains(string(data), "ward.git.status") {
		t.Errorf("audit row missing the dotted verb name; got:\n%s", string(data))
	}
}

const awsGuardfile = `wrap ward ops aws {
	exec aws

	can run "*" {
		describe "open passthrough"
	}
}`

// TestWildcardPassthrough proves `can run *` mounts the group itself as one
// open passthrough: any service/operation reaches the binary verbatim.
func TestWildcardPassthrough(t *testing.T) {
	var cp capture
	if err := runArgv(t, awsGuardfile, &cp, "ops", "aws", "sts", "get-caller-identity"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "aws" || strings.Join(cp.argv, " ") != "sts get-caller-identity" {
		t.Errorf("invocation = %s %v, want aws sts get-caller-identity", cp.bin, cp.argv)
	}
}

// awsWhenGuardfile is the shipped shape: a self-describing `deny-when` over
// the wrapped binary's argv.
const awsWhenGuardfile = `wrap ward ops aws {
	exec aws

	can run "*" {
		deny-when any-arg matches \
			"*secret*" "*tfstate*"
		describe "open passthrough; sensitive tokens denied pre-send"
	}
}`

// TestDenyWhenDeniesSensitiveToken proves the deny-when guard refuses an
// invocation naming a token that matches a sensitive glob, pre-send.
func TestDenyWhenDeniesSensitiveToken(t *testing.T) {
	var cp capture
	err := runArgv(t, awsWhenGuardfile, &cp, "ops", "aws", "s3", "ls", "s3://prod-secrets-bucket")
	if err == nil {
		t.Fatal("expected the sensitive read to be denied, got nil")
	}
	if cp.bin != "" {
		t.Errorf("denied invocation still executed: %s %v", cp.bin, cp.argv)
	}
	if !strings.Contains(err.Error(), "*secret*") {
		t.Errorf("denial should name the matched pattern: %v", err)
	}
}

// TestWhenKwargAllowGuard proves `when <flag> matches`: the call passes only
// when the named kwarg's value matches an allowed glob.
func TestWhenKwargAllowGuard(t *testing.T) {
	const src = `wrap ward ops aws {
		exec aws
		can run secretsmanager get-secret-value {
			when secret-id matches "readonly-*"
		}
	}`
	var cp capture
	if err := runArgv(t, src, &cp, "ops", "aws", "secretsmanager", "get-secret-value", "--secret-id", "readonly-keys"); err != nil {
		t.Fatalf("allowed kwarg value refused: %v", err)
	}
	if got := strings.Join(cp.argv, " "); got != "secretsmanager get-secret-value --secret-id readonly-keys" {
		t.Errorf("argv = %q, want the get-secret-value passthrough", got)
	}
	err := runArgv(t, src, &cp, "ops", "aws", "secretsmanager", "get-secret-value", "--secret-id", "prod-db")
	if err == nil {
		t.Fatal("expected a non-matching kwarg to be denied, got nil")
	}
}

// TestWhenPositionalIndexSelector proves `argN` reads one positional by index,
// over the caller args after the matched subcommand path.
func TestWhenPositionalIndexSelector(t *testing.T) {
	const src = `wrap ward ops aws {
		exec aws
		can run "s3 ls" {
			when arg0 matches "s3://public-*"
		}
	}`
	var cp capture
	if err := runArgv(t, src, &cp, "ops", "aws", "s3", "ls", "s3://public-assets"); err != nil {
		t.Fatalf("allowed positional refused: %v", err)
	}
	if err := runArgv(t, src, &cp, "ops", "aws", "s3", "ls", "s3://prod-secrets"); err == nil {
		t.Fatal("expected a non-matching positional to be denied, got nil")
	}
}

// TestUnknownGateFailsClosed proves a typo'd gate name refuses to build.
func TestUnknownGateFailsClosed(t *testing.T) {
	gf, err := Parse([]byte(`wrap ward ops aws {
		exec aws
		can run "*" { gate aws-reed {} }
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Build(Config{Guardfile: gf}); err == nil {
		t.Fatal("expected an unknown-gate build error, got nil")
	}
}

// TestWildcardMustBeOnlyGrant proves mixing `can run *` with named grants
// refuses to build.
func TestWildcardMustBeOnlyGrant(t *testing.T) {
	gf, err := Parse([]byte(`wrap ward ops aws {
		exec aws
		can run "*"
		can run s3
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Build(Config{Guardfile: gf}); err == nil {
		t.Fatal("expected a wildcard-exclusivity build error, got nil")
	}
}

func TestParseFailsClosed(t *testing.T) {
	cases := []string{
		`wrap ward git { can run status }`,                           // no exec block
		`wrap ward git { exec git }`,                                 // no grants
		`wrap ward git { exec git; can status }`,                     // missing `run`
		`wrap ward git { exec git; can run commit { env "X=1" } }`,   // unknown policy node
		`wrap ward git { exec git; unknown-node x; can run status }`, // unknown wrap child
		`wrap ward git { exec git; can run status; doc-link "x" }`,   // retired node
	}
	for _, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("expected parse failure for %q", src)
		}
	}
}

// agentGuardfile exercises the per-grant `argv` override: a friendly leaf name
// (launch/headless/whoami) decoupled from the executed invocation.
const agentGuardfile = `wrap ward-kdl agents claude {
	exec claude

	can run launch {
		argv
		describe "interactive session (bare binary)"
	}
	can run headless {
		argv "-p"
		describe "non-interactive print mode"
	}
	can run whoami {
		argv "login" "status"
	}
	can run doctor
}`

func TestArgvOverrideBareLaunch(t *testing.T) {
	var cp capture
	if err := runArgv(t, agentGuardfile, &cp, "agents", "claude", "launch"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "claude" {
		t.Errorf("bin = %q, want claude", cp.bin)
	}
	// the friendly leaf name `launch` must NOT leak into argv; an empty
	// override runs the bare binary.
	if len(cp.argv) != 0 {
		t.Errorf("argv = %v, want empty (bare launch)", cp.argv)
	}
}

func TestArgvOverrideBareLaunchForwardsArgs(t *testing.T) {
	var cp capture
	if err := runArgv(t, agentGuardfile, &cp, "agents", "claude", "launch", "--model", "opus"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Join(cp.argv, " "); got != "--model opus" {
		t.Errorf("argv = %q, want %q", got, "--model opus")
	}
}

func TestArgvOverrideFlagShape(t *testing.T) {
	var cp capture
	if err := runArgv(t, agentGuardfile, &cp, "agents", "claude", "headless", "write a test"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Join(cp.argv, " "); got != "-p write a test" {
		t.Errorf("argv = %q, want %q", got, "-p write a test")
	}
}

func TestArgvOverrideMultiToken(t *testing.T) {
	var cp capture
	if err := runArgv(t, agentGuardfile, &cp, "agents", "claude", "whoami"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Join(cp.argv, " "); got != "login status" {
		t.Errorf("argv = %q, want %q", got, "login status")
	}
}

func TestNoArgvOverrideKeepsSubcommand(t *testing.T) {
	var cp capture
	if err := runArgv(t, agentGuardfile, &cp, "agents", "claude", "doctor"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Join(cp.argv, " "); got != "doctor" {
		t.Errorf("argv = %q, want %q (subcommand kept when no override)", got, "doctor")
	}
}

func TestArgvOverrideParseFailsClosed(t *testing.T) {
	cases := []string{
		`wrap ward agents claude { exec claude; can run "*" { argv "-p" } }`,                 // argv on wildcard
		`wrap ward agents claude { exec claude; can run launch { argv "-p" { mode "x" } } }`, // argv block
	}
	for _, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("expected parse failure for %q", src)
		}
	}
}

const embeddedGuardfile = `wrap ward-kdl ops measure {
	exec python3
	can run storage {
		argv "-I"
		embed "scripts/storage_measure.py"
		argv "--format" "text"
		sealed
	}
}`

func TestEmbedResolvesAbsoluteArgvAtDeclarationPosition(t *testing.T) {
	gf, err := Parse([]byte(embeddedGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := strings.Join(gf.Grants[0].ExecArgv(), " "); got != "-I <embedded:scripts/storage_measure.py> --format text" {
		t.Errorf("symbolic argv = %q", got)
	}
	if got := gf.EmbedPaths(); len(got) != 1 || got[0] != "scripts/storage_measure.py" {
		t.Errorf("EmbedPaths = %v", got)
	}

	materialized := filepath.Join(t.TempDir(), "storage_measure.py")
	var cp capture
	root := &cli.Command{Name: "ward-kdl"}
	if err := Mount(root, Config{
		Guardfile:     gf,
		EmbeddedFiles: map[string]string{"scripts/storage_measure.py": materialized},
		Run:           cp.run,
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := root.Run(context.Background(), []string{"ward-kdl", "ops", "measure", "storage"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"-I", materialized, "--format", "text"}
	if strings.Join(cp.argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %v, want %v", cp.argv, want)
	}
}

func TestEmbedFailsClosedWithoutAbsoluteMaterialization(t *testing.T) {
	gf, err := Parse([]byte(embeddedGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, files := range map[string]map[string]string{
		"missing":  nil,
		"relative": {"scripts/storage_measure.py": "runtime/storage_measure.py"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Build(Config{Guardfile: gf, EmbeddedFiles: files}); err == nil {
				t.Fatal("expected embedded-file resolution to fail closed")
			}
		})
	}
}

func TestEmbedFailsClosedWithInvalidArgvIndex(t *testing.T) {
	gf, err := Parse([]byte(embeddedGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gf.Grants[0].EmbeddedArgs[0].Index = len(gf.Grants[0].Argv)
	if _, err := Build(Config{
		Guardfile:     gf,
		EmbeddedFiles: map[string]string{"scripts/storage_measure.py": filepath.Join(t.TempDir(), "storage_measure.py")},
	}); err == nil {
		t.Fatal("expected invalid embedded argv index to fail closed")
	}
}

func TestEmbedSourcePathFailsClosed(t *testing.T) {
	for _, source := range []string{"", "/tmp/x.py", "../x.py", "scripts/../x.py", "./x.py", `scripts\x.py`} {
		src := fmt.Sprintf(`wrap ward ops measure { exec python3; can run storage { embed %q; sealed } }`, source)
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("expected source %q to fail closed", source)
		}
	}
	if _, err := Parse([]byte(`wrap ward ops measure { exec python3; can run storage { embed "x.py" { mode "read" }; sealed } }`)); err == nil {
		t.Error("expected an embed block to fail closed")
	}
}

// sealedGuardfile exercises the `sealed` clause: a strictly-single-resource
// verb whose pinned `argv` forwards exactly, refusing any trailing caller arg.
const sealedGuardfile = `wrap ward-kdl ops forgejo {
	exec kubectl

	can run "read runner-token" {
		argv "get" "secret" "forgejo-runner-secrets" "-n" "forgejo" "-o" "go-template={{.data.api-token | base64decode}}"
		sealed
		describe "read only the forgejo runner secret; no pivot"
	}
}`

// TestSealedForwardsPinnedArgvWithNoArgs proves a sealed verb with no trailing
// tokens runs its pinned argv verbatim.
func TestSealedForwardsPinnedArgvWithNoArgs(t *testing.T) {
	var cp capture
	if err := runArgv(t, sealedGuardfile, &cp, "ops", "forgejo", "read", "runner-token"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "kubectl" {
		t.Errorf("bin = %q, want kubectl", cp.bin)
	}
	want := "get secret forgejo-runner-secrets -n forgejo -o go-template={{.data.api-token | base64decode}}"
	if got := strings.Join(cp.argv, " "); got != want {
		t.Errorf("argv = %q,\nwant   %q", got, want)
	}
}

// TestSealedRefusesTrailingArg proves any caller-supplied token is refused,
// blocking the widening `read othersecret -n kube-system` pivot.
func TestSealedRefusesTrailingArg(t *testing.T) {
	var cp capture
	err := runArgv(t, sealedGuardfile, &cp, "ops", "forgejo", "read", "runner-token", "othersecret", "-n", "kube-system")
	if err == nil {
		t.Fatal("expected a sealed verb to refuse trailing args")
	}
	if cp.bin != "" {
		t.Errorf("sealed refusal still executed: %s %v", cp.bin, cp.argv)
	}
}

// TestSealedWithoutArgvIsParseError proves `sealed` without a pinned `argv`
// fails closed at parse time (nothing to seal).
func TestSealedWithoutArgvIsParseError(t *testing.T) {
	src := `wrap ward-kdl ops forgejo { exec kubectl; can run read { sealed } }`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("expected a parse failure for `sealed` without `argv`")
	}
}

// inspectGuardfile is the `allow` inspect-list sugar: one wrap opening N
// read-only binaries as a flat list, each an independent open passthrough.
const inspectGuardfile = `wrap ward-kdl inspect {
	allow ls cat head grep find stat jq \
	      ps df du free uptime who id env which uname
}`

// TestAllowMountsEveryBinary proves each `allow` entry mounts as its own leaf.
func TestAllowMountsEveryBinary(t *testing.T) {
	gf, err := Parse([]byte(inspectGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	root, err := Build(Config{Guardfile: gf})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range []string{"ls", "cat", "grep", "jq", "uname"} {
		if findChild(root, want) == nil {
			t.Errorf("missing inspect leaf %q", want)
		}
	}
	// deny-by-default: a binary never listed must not mount.
	if findChild(root, "rm") != nil || findChild(root, "kubectl") != nil {
		t.Error("unlisted binary mounted")
	}
}

// TestAllowFunnelsEachBinary proves a listed binary runs verbatim with its args:
// the leaf execs that binary, not the group, and every arg passes through.
func TestAllowFunnelsEachBinary(t *testing.T) {
	var cp capture
	if err := runArgv(t, inspectGuardfile, &cp, "inspect", "grep", "-rn", "TODO", "pkg/"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "grep" {
		t.Errorf("bin = %q, want grep", cp.bin)
	}
	if got := strings.Join(cp.argv, " "); got != "-rn TODO pkg/" {
		t.Errorf("argv = %q, want -rn TODO pkg/", got)
	}

	// a second binary in the same wrap is independently reachable
	if err := runArgv(t, inspectGuardfile, &cp, "inspect", "cat", "go.mod"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "cat" || strings.Join(cp.argv, " ") != "go.mod" {
		t.Errorf("invocation = %s %v, want cat go.mod", cp.bin, cp.argv)
	}
}

// TestAllowAuditNameNamesTheBinary proves the dotted audit name composes the
// wrap group with the funnel's binary, so each leaf is distinct in the log.
func TestAllowAuditNameNamesTheBinary(t *testing.T) {
	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(func() { _ = w.Close() })
	gf, err := Parse([]byte(inspectGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var cp capture
	root := &cli.Command{Name: "ward-kdl"}
	cfg := Config{Guardfile: gf, Run: cp.run, Wrap: func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) }}
	if err := Mount(root, cfg); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := root.Run(context.Background(), []string{"ward-kdl", "inspect", "df", "-h"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, _ := os.ReadFile(w.Path)
	if !strings.Contains(string(data), "ward-kdl.inspect.df") {
		t.Errorf("audit row missing the per-binary dotted verb name; got:\n%s", string(data))
	}
}

// TestAllowWrapGuardAppliesToEveryLeaf proves a wrap-level `deny-when` composes
// onto all generated funnels: the read-only floor over the whole inspect list.
func TestAllowWrapGuardAppliesToEveryLeaf(t *testing.T) {
	const src = `wrap ward-kdl inspect {
		allow cat grep
		deny-when any-arg matches "*secret*" "*.key"
	}`
	var cp capture
	// a benign read passes on either binary
	if err := runArgv(t, src, &cp, "inspect", "cat", "README.md"); err != nil {
		t.Fatalf("benign cat refused: %v", err)
	}
	// the guard refuses a sensitive arg on cat...
	cp = capture{}
	if err := runArgv(t, src, &cp, "inspect", "cat", "id_rsa.key"); err == nil {
		t.Fatal("expected the wrap guard to deny cat id_rsa.key, got nil")
	}
	if cp.bin != "" {
		t.Errorf("denied invocation still executed: %s %v", cp.bin, cp.argv)
	}
	// ...and equally on grep, proving it composed onto every leaf
	if err := runArgv(t, src, &cp, "inspect", "grep", "x", "prod-secrets.txt"); err == nil {
		t.Fatal("expected the wrap guard to deny grep over a secret path, got nil")
	}
}

// TestAllowParseFailsClosed proves the inspect-list validation rules.
func TestAllowParseFailsClosed(t *testing.T) {
	cases := []string{
		`wrap ward-kdl inspect { allow }`,                           // empty list
		`wrap ward-kdl inspect { allow ls; exec cat }`,              // exclusive with exec
		`wrap ward-kdl inspect { allow ls; can run status }`,        // exclusive with can run
		`wrap ward-kdl inspect { allow ls /usr/bin/cat }`,           // path, not a bare name
		`wrap ward-kdl inspect { allow "rm;dd" }`,                   // shell metacharacter
		`wrap ward-kdl inspect { allow cat cat }`,                   // duplicate binary
		`wrap ward-kdl inspect { allow ls { describe "x" } }`,       // block body, not a flat list
		`wrap ward git { exec git; deny-when any-arg matches "*" }`, // wrap guard without allow
	}
	for _, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("expected parse failure for %q", src)
		}
	}
}

// envGuardfile injects an opaque host via a provider plus a literal: the
// OLLAMA_HOST-to-tower shape.
const envGuardfile = `wrap ward-kdl agents ollama {
	exec ollama {
		env "OLLAMA_HOST" { value ssm "/coilysiren/ollama/host" }
		env "OLLAMA_KEEP_ALIVE" "30m"
	}
	can run list
}`

func TestEnvResolvesProviderAndLiteral(t *testing.T) {
	gf, err := Parse([]byte(envGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var cp capture
	root := &cli.Command{Name: "ward-kdl"}
	provs := map[string]valuesource.Provider{
		"ssm": func(_ context.Context, addr string) (string, error) {
			if addr != "/coilysiren/ollama/host" {
				t.Fatalf("ssm address = %q", addr)
			}
			return "http://tower:11434", nil
		},
	}
	if err := Mount(root, Config{Guardfile: gf, Run: cp.run, Providers: provs}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := root.Run(context.Background(), []string{"ward-kdl", "agents", "ollama", "list"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := map[string]string{}
	for _, kv := range cp.env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["OLLAMA_HOST"] != "http://tower:11434" {
		t.Errorf("OLLAMA_HOST = %q (resolved provider value), env = %v", got["OLLAMA_HOST"], cp.env)
	}
	if got["OLLAMA_KEEP_ALIVE"] != "30m" {
		t.Errorf("OLLAMA_KEEP_ALIVE = %q (literal), env = %v", got["OLLAMA_KEEP_ALIVE"], cp.env)
	}
}

func TestEnvUnresolvedProviderFailsClosed(t *testing.T) {
	gf, err := Parse([]byte(envGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var cp capture
	root := &cli.Command{Name: "ward-kdl"}
	// No `ssm` provider registered -> resolution must fail before any exec.
	if err := Mount(root, Config{Guardfile: gf, Run: cp.run}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	err = root.Run(context.Background(), []string{"ward-kdl", "agents", "ollama", "list"})
	if err == nil {
		t.Fatal("expected an env-resolution failure, got nil")
	}
	if cp.bin != "" {
		t.Errorf("exec ran despite unresolved env: %s %v", cp.bin, cp.argv)
	}
}

func TestEnvParseFailsClosed(t *testing.T) {
	cases := []string{
		`wrap ward x { exec foo { env "BAR" } can run baz }`,                        // neither literal nor value
		`wrap ward x { exec foo { env "BAR" { value ssm } } can run baz }`,          // value missing address
		`wrap ward x { exec foo { env { value ssm "/p" } } can run baz }`,           // env missing name
		`wrap ward x { exec foo { env "BAR" "lit" { value ssm "/p" } } can run b }`, // literal and block both
	}
	for _, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("expected parse failure for %q", src)
		}
	}
}

// sshGuardfile is the shipped passthrough shape: a default-allow ssh funnel, an
// rm deny on the funnelled args, and a host gate keyed on the local hostname.
const sshGuardfile = `wrap ward-kdl ssh {
	passthrough ssh
	never pass rm
	only pass when shell hostname is "mac*" "win*"
}`

// runArgvHost mounts src with a fixed hostname for the `shell hostname` gate,
// recording the resolved invocation in cp.
func runArgvHost(t *testing.T, src, hostname string, cp *capture, argv ...string) error {
	t.Helper()
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	host := func(_ context.Context, _ []string) (string, error) { return hostname, nil }
	root := &cli.Command{Name: "ward"}
	if err := Mount(root, Config{Guardfile: gf, Run: cp.run, Host: host}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return root.Run(context.Background(), append([]string{"ward"}, argv...))
}

// TestPassthroughDesugarsToWildcard proves `passthrough <bin>` mounts one open
// funnel: any args reach the binary verbatim when the host gate allows.
func TestPassthroughDesugarsToWildcard(t *testing.T) {
	var cp capture
	if err := runArgvHost(t, sshGuardfile, "mac-mini", &cp, "ssh", "kai@box", "uptime"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "ssh" || strings.Join(cp.argv, " ") != "kai@box uptime" {
		t.Errorf("invocation = %s %v, want ssh kai@box uptime", cp.bin, cp.argv)
	}
}

// TestPassthroughArgvPrefix proves `passthrough tailscale ssh` fixes the `ssh`
// subcommand prefix ahead of the caller args.
func TestPassthroughArgvPrefix(t *testing.T) {
	const src = `wrap ward-kdl tailscale ssh {
		passthrough tailscale ssh
		never pass rm
		only pass when shell hostname is "mac*"
	}`
	var cp capture
	if err := runArgvHost(t, src, "mac-mini", &cp, "tailscale", "ssh", "kai@box", "uptime"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.bin != "tailscale" || strings.Join(cp.argv, " ") != "ssh kai@box uptime" {
		t.Errorf("invocation = %s %v, want tailscale ssh kai@box uptime", cp.bin, cp.argv)
	}
}

// TestNeverPassDeniesRm proves the wrap-level `never pass rm` refuses an rm
// positional before any exec.
func TestNeverPassDeniesRm(t *testing.T) {
	var cp capture
	err := runArgvHost(t, sshGuardfile, "mac-mini", &cp, "ssh", "kai@box", "rm", "-rf", "/x")
	if err == nil {
		t.Fatal("expected rm to be denied, got nil")
	}
	if cp.bin != "" {
		t.Errorf("denied invocation still executed: %s %v", cp.bin, cp.argv)
	}
}

// TestHostGateAllowsWorkstation proves the host gate passes a matching hostname.
func TestHostGateAllowsWorkstation(t *testing.T) {
	var cp capture
	if err := runArgvHost(t, sshGuardfile, "macbook-pro", &cp, "ssh", "kai@box"); err != nil {
		t.Fatalf("workstation host refused: %v", err)
	}
}

// TestHostGateDeniesServer proves the host gate fails closed on a non-matching
// hostname (a server originating ssh), before any exec.
func TestHostGateDeniesServer(t *testing.T) {
	var cp capture
	err := runArgvHost(t, sshGuardfile, "kai-server", &cp, "ssh", "kai@box")
	if err == nil {
		t.Fatal("expected a server host to be denied, got nil")
	}
	if cp.bin != "" {
		t.Errorf("denied invocation still executed: %s %v", cp.bin, cp.argv)
	}
	if !strings.Contains(err.Error(), "host gate") {
		t.Errorf("denial should name the host gate: %v", err)
	}
}

// TestHostResolverErrorFailsClosed proves a `shell` source that errors refuses
// the call rather than silently allowing it.
func TestHostResolverErrorFailsClosed(t *testing.T) {
	gf, err := Parse([]byte(sshGuardfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var cp capture
	boom := func(context.Context, []string) (string, error) { return "", context.DeadlineExceeded }
	root := &cli.Command{Name: "ward"}
	if err := Mount(root, Config{Guardfile: gf, Run: cp.run, Host: boom}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := root.Run(context.Background(), []string{"ward", "ssh", "kai@box"}); err == nil {
		t.Fatal("expected a resolver error to fail closed, got nil")
	}
	if cp.bin != "" {
		t.Errorf("call ran despite an unresolved host gate: %s %v", cp.bin, cp.argv)
	}
}

// TestPassthroughParseFailsClosed proves the funnel sugar rejects malformed or
// conflicting shapes.
func TestPassthroughParseFailsClosed(t *testing.T) {
	cases := []string{
		`wrap ward x { passthrough }`,                                    // no binary
		`wrap ward x { exec foo; passthrough foo }`,                      // exec + passthrough
		`wrap ward x { passthrough foo; can run bar }`,                   // funnel + grant
		`wrap ward x { passthrough foo; only pass shell hostname }`,      // only pass without when
		`wrap ward x { passthrough foo; never pass when shell is "a" }`,  // shell source, no command
		`wrap ward x { passthrough foo; only pass when shell hostname }`, // no comparator/patterns
		`wrap ward x { passthrough foo; only pass }`,                     // only pass with nothing
	}
	for _, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("expected parse failure for %q", src)
		}
	}
}
