package execverb

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/exitcode"
)

const pathsWithinGuardfile = `wrap ward fs rm {
    exec rm
    can run "*" {
        gate paths-within {
            operands "positional"
            root-env "TEST_FS_ROOTS"
            reason "work on a copy inside the shadow"
        }
    }
}`

// shadow builds <tmp>/shadow/{projects,home} and a sibling <tmp>/outside, and
// points the gate's variables at them.
func shadow(t *testing.T) (inside, outside string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inside = filepath.Join(tmp, "shadow")
	outside = filepath.Join(tmp, "outside")
	for _, d := range []string{filepath.Join(inside, "projects"), filepath.Join(inside, "home"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TEST_FS_ROOTS", inside)
	return inside, outside
}

func gateFor(t *testing.T, body string) gateFunc {
	t.Helper()
	gf, err := Parse([]byte(`wrap ward fs tool { exec tool; can run "*" { gate paths-within { ` + body + ` } } }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gates, err := buildGates(gf.Grants[0])
	if err != nil {
		t.Fatalf("buildGates: %v", err)
	}
	return gates[0]
}

func TestPathsWithinIsInertUntilAVariableIsSet(t *testing.T) {
	t.Setenv("TEST_FS_ROOTS", "")
	gate := gateFor(t, `operands "positional"; root-env "TEST_FS_ROOTS"`)
	if err := gate([]string{"-rf", "/etc"}); err != nil {
		t.Fatalf("unset roots must pass everything, got %v", err)
	}
}

func TestPathsWithinPositional(t *testing.T) {
	inside, outside := shadow(t)
	gate := gateFor(t, `operands "positional"; root-env "TEST_FS_ROOTS"`)

	link := filepath.Join(inside, "projects", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(inside, "projects"))

	cases := []struct {
		name string
		argv []string
		ok   bool
	}{
		{"inside file", []string{"-rf", filepath.Join(inside, "projects", "x")}, true},
		{"inside nonexistent parent", []string{filepath.Join(inside, "nope", "deeper", "x")}, true},
		{"relative inside", []string{"-f", "x"}, true},
		{"the root itself", []string{"-r", inside}, true},
		{"outside", []string{"-rf", filepath.Join(outside, "x")}, false},
		{"one of many outside", []string{filepath.Join(inside, "a"), filepath.Join(outside, "b")}, false},
		{"after double dash", []string{"--", "-x", filepath.Join(outside, "b")}, false},
		{"dotdot out", []string{filepath.Join(inside, "projects", "..", "..", "outside", "x")}, false},
		{"parent symlink leaves", []string{filepath.Join(link, "x")}, false},
		{"leaf symlink is acted on, not followed", []string{link}, true},
		{"trailing slash follows the link", []string{link + "/"}, false},
		{"sibling sharing a prefix", []string{inside + "-evil/x"}, false},
		{"slash", []string{"/"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := gate(tc.argv)
			if tc.ok && err != nil {
				t.Fatalf("want pass, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("want refusal for %v", tc.argv)
			}
		})
	}
}

func TestPathsWithinRefusalNamesThePathAndTheReason(t *testing.T) {
	_, outside := shadow(t)
	gate := gateFor(t, `operands "positional"; root-env "TEST_FS_ROOTS"; reason "ask Kai"`)
	err := gate([]string{filepath.Join(outside, "x")})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(outside, "x")) || !strings.Contains(err.Error(), "ask Kai") {
		t.Fatalf("refusal = %v, want the path and the reason", err)
	}
}

func TestPathsWithinDropsRootsThatWouldWidenTheBoundary(t *testing.T) {
	_, outside := shadow(t)
	gate := gateFor(t, `operands "positional"; root-env "TEST_FS_ROOTS"`)

	t.Setenv("TEST_FS_ROOTS", "/")
	if err := gate([]string{filepath.Join(outside, "x")}); err == nil {
		t.Fatal("a root of / must be dropped, leaving nothing inside")
	}
	home, _ := os.UserHomeDir()
	t.Setenv("TEST_FS_ROOTS", home)
	if err := gate([]string{filepath.Join(home, "x")}); err == nil {
		t.Fatal("a root of the home directory must be dropped")
	}
	t.Setenv("TEST_FS_ROOTS", "/"+string(os.PathListSeparator)+outside)
	if err := gate([]string{filepath.Join(outside, "x")}); err != nil {
		t.Fatalf("the surviving root still counts, got %v", err)
	}
}

func TestPathsWithinParentEnvAndFixedRoots(t *testing.T) {
	inside, outside := shadow(t)
	scratch := filepath.Join(outside, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_FS_ROOTS", "")
	t.Setenv("TEST_SHADOW_PROJECTS", filepath.Join(inside, "projects"))
	gate := gateFor(t, `operands "positional"; root-parent-env "TEST_SHADOW_PROJECTS"; root "`+scratch+`"; root "`+filepath.Join(outside, "absent")+`"`)

	for _, ok := range []string{
		filepath.Join(inside, "home", "x"),
		filepath.Join(inside, "seat", "x"),
		filepath.Join(scratch, "x"),
	} {
		if err := gate([]string{ok}); err != nil {
			t.Errorf("%s should pass: %v", ok, err)
		}
	}
	for _, bad := range []string{filepath.Join(outside, "x"), filepath.Join(outside, "absent", "x")} {
		if err := gate([]string{bad}); err == nil {
			t.Errorf("%s should be refused: a fixed root that is not a directory counts for nothing", bad)
		}
	}
}

func TestPathsWithinMvTargetSpellings(t *testing.T) {
	inside, outside := shadow(t)
	gate := gateFor(t, `operands "positional-and-target"; root-env "TEST_FS_ROOTS"`)
	src := filepath.Join(inside, "f")
	out := filepath.Join(outside, "d")

	refused := [][]string{
		{"-t", out, src},
		{"-t" + out, src},
		{"-vt", out, src},
		{"-vt" + out, src},
		{"--target-directory", out, src},
		{"--target-directory=" + out, src},
		{"--target=" + out, src},
		{src, out},
		{"-S", ".bak", src, out},
	}
	for _, argv := range refused {
		if err := gate(argv); err == nil {
			t.Errorf("mv %v should be refused", argv)
		}
	}
	passed := [][]string{
		{src, filepath.Join(inside, "g")},
		{"-t", filepath.Join(inside, "d"), src},
		{"-S", ".bak", src, filepath.Join(inside, "g")},
		{"--suffix=.bak", src, filepath.Join(inside, "g")},
		{"-T", src, filepath.Join(inside, "g")},
	}
	for _, argv := range passed {
		if err := gate(argv); err != nil {
			t.Errorf("mv %v should pass: %v", argv, err)
		}
	}
}

func TestPathsWithinFind(t *testing.T) {
	inside, outside := shadow(t)
	gate := gateFor(t, `operands "find"; root-env "TEST_FS_ROOTS"`)

	t.Chdir(outside)
	// A find that only reads passes anywhere, including with no start path.
	for _, argv := range [][]string{
		{outside, "-name", "x"},
		{"-name", "x"},
		{"/", "-type", "f", "-print"},
	} {
		if err := gate(argv); err != nil {
			t.Errorf("read-only find %v should pass: %v", argv, err)
		}
	}
	refused := [][]string{
		{outside, "-delete"},
		{"-delete"},
		{"-L", outside, "-delete"},
		{"-H", "-O2", outside, "-exec", "rm", "{}", ";"},
		{"-D", "tree", outside, "-delete"},
		{inside, outside, "-delete"},
		{inside, "-name", "x", "-fprint", filepath.Join(outside, "out")},
		{inside, "-fprintf", filepath.Join(outside, "out"), "%p"},
	}
	for _, argv := range refused {
		if err := gate(argv); err == nil {
			t.Errorf("find %v should be refused", argv)
		}
	}
	for _, argv := range [][]string{
		{inside, "-delete"},
		{"-L", inside, "-name", "x", "-exec", "rm", "{}", ";"},
		{inside, "-name", "x", "-fprint", filepath.Join(inside, "out")},
	} {
		if err := gate(argv); err != nil {
			t.Errorf("find %v writes only inside and should pass: %v", argv, err)
		}
	}
}

func TestPathsWithinOperandParsing(t *testing.T) {
	cases := []struct {
		kind string
		argv []string
		want []string
	}{
		{operandsPositional, []string{"-rf", "a", "--", "-b", "c"}, []string{"a", "-b", "c"}},
		{operandsPositional, []string{"-t", "a"}, []string{"a"}},
		{operandsWithTarget, []string{"-t", "d", "a"}, []string{"d", "a"}},
		{operandsWithTarget, []string{"-vtd", "a"}, []string{"d", "a"}},
		{operandsWithTarget, []string{"--target-directory=d", "a"}, []string{"d", "a"}},
		{operandsWithTarget, []string{"-S", "~", "a", "b"}, []string{"a", "b"}},
		{operandsFind, []string{"a", "-name", "x"}, nil},
		{operandsFind, []string{"a", "b", "-delete"}, []string{"a", "b"}},
		{operandsFind, []string{"-delete"}, []string{"."}},
	}
	for _, tc := range cases {
		got := pathsWithin{operands: tc.kind}.operandsOf(tc.argv)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %v = %v, want %v", tc.kind, tc.argv, got, tc.want)
		}
	}
}

func TestPathsWithinMisconfigurationFailsTheBuild(t *testing.T) {
	cases := map[string]string{
		"no operands":     `root-env "X"`,
		"bad operands":    `operands "everything"; root-env "X"`,
		"no root source":  `operands "positional"`,
		"unknown option":  `operands "positional"; root-env "X"; roots "Y"`,
		"two operands":    `operands "positional"; operands "find"; root-env "X"`,
		"pattern is text": `operands "positional"; root-env "X"; pattern "*"`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			gf, err := Parse([]byte(`wrap ward fs tool { exec tool; can run "*" { gate paths-within { ` + body + ` } } }`))
			if err != nil {
				return // refused at parse is as closed as at build
			}
			if _, err := buildGates(gf.Grants[0]); err == nil {
				t.Fatal("expected the build to fail closed")
			}
		})
	}
}

// Through Mount: a refusal is policy_denied, audits as reject, and reaches no
// binary. The granted call is the control, so the gate cannot be refusing all.
func TestPathsWithinThroughAMountedFunnel(t *testing.T) {
	inside, outside := shadow(t)

	records, cp, err := runAudited(t, pathsWithinGuardfile, "fs", "rm", "-rf", filepath.Join(outside, "x"))
	coded := exitcode.From(err)
	if coded == nil || coded.Code() != exitcode.PolicyDenied {
		t.Fatalf("outside = %v, want policy_denied", err)
	}
	if cp.bin != "" {
		t.Errorf("refused call reached %s %v", cp.bin, cp.argv)
	}
	if len(records) != 1 || records[0].Decision != audit.DecisionReject {
		t.Errorf("records = %+v, want one reject row", records)
	}
	if !strings.Contains(err.Error(), "work on a copy inside the shadow") {
		t.Errorf("error %q lacks the guardfile's reason", err)
	}

	records, cp, err = runAudited(t, pathsWithinGuardfile, "fs", "rm", "-rf", filepath.Join(inside, "x"))
	if err != nil {
		t.Fatalf("inside = %v, want pass", err)
	}
	if cp.bin != "rm" || !reflect.DeepEqual(cp.argv, []string{"-rf", filepath.Join(inside, "x")}) {
		t.Errorf("ran %s %v, want rm with the caller's argv", cp.bin, cp.argv)
	}
	if len(records) != 1 || records[0].Decision != audit.DecisionAccept {
		t.Errorf("records = %+v, want one accept row", records)
	}

	t.Setenv("TEST_FS_ROOTS", "")
	if _, cp, err = runAudited(t, pathsWithinGuardfile, "fs", "rm", "-rf", filepath.Join(outside, "x")); err != nil || cp.bin != "rm" {
		t.Fatalf("with no roots set the funnel must forward, got %v / %q", err, cp.bin)
	}
}
