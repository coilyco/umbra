package execverb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/exitcode"
)

// A replacement is installed under the wrapped tool's own name, so the caller
// types `git commit` and the guardfile is the whole tool they can see.
const replaceGuardfile = `wrap aosguard git {
    exec git
    replace
    can run commit { deny-flag "--no-verify" }
    can run status
}`

func parseOrFail(t *testing.T, src string) *Guardfile {
	t.Helper()
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return gf
}

func TestReplaceOccludesTheWrappedBinaryName(t *testing.T) {
	gf := parseOrFail(t, replaceGuardfile)
	if !gf.Replace {
		t.Fatal("`replace` must mark the guardfile as a replacement")
	}
	// The default comes from the wrapped binary, not the wrap path's last
	// segment: a replacement stands in front of a binary, not a verb group.
	if got := gf.OccludedName(); got != "git" {
		t.Errorf("occluded name = %q, want %q", got, "git")
	}
}

func TestReplaceNameOverrideWins(t *testing.T) {
	gf := parseOrFail(t, `wrap aosguard vcs {
    exec "/opt/homebrew/bin/git"
    replace "git"
    can run status
}`)
	if got := gf.OccludedName(); got != "git" {
		t.Errorf("occluded name = %q, want %q", got, "git")
	}
}

func TestReplaceDefaultsToTheBasenameOfAPinnedPath(t *testing.T) {
	gf := parseOrFail(t, `wrap aosguard git {
    exec "/opt/homebrew/bin/git"
    replace
    can run status
}`)
	if got := gf.OccludedName(); got != "git" {
		t.Errorf("occluded name = %q, want %q", got, "git")
	}
}

func TestReplaceFailsClosed(t *testing.T) {
	for name, src := range map[string]string{
		"with an allow inspect list": `wrap aosguard read {
    allow grep cat
    replace
}`,
		"with a path separator in the name": `wrap aosguard git {
    exec git
    replace "../../evil"
    can run status
}`,
		"with more than one name": `wrap aosguard git {
    exec git
    replace "git" "hub"
    can run status
}`,
		"with a body": `wrap aosguard git {
    exec git
    replace { reason "x" }
    can run status
}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatal("want a fail-closed parse error, got none")
			}
		})
	}
}

func TestBuildReplacementMountsGrantsAtTheRoot(t *testing.T) {
	gf := parseOrFail(t, replaceGuardfile)
	root, err := BuildReplacement(Config{Guardfile: gf, Run: func(context.Context, string, []string, []string) error { return nil }})
	if err != nil {
		t.Fatalf("BuildReplacement: %v", err)
	}
	if root.Name != "git" {
		t.Errorf("root command = %q, want the occluded tool %q", root.Name, "git")
	}
	// A grant is a top-level verb here, because the binary itself is the tool.
	for _, verb := range []string{"commit", "status"} {
		if findChild(root, verb) == nil {
			t.Errorf("granted verb %q must mount at the root of a replacement", verb)
		}
	}
}

func TestBuildReplacementRefusesAGuardfileWithoutReplace(t *testing.T) {
	gf := parseOrFail(t, `wrap aosguard git {
    exec git
    can run status
}`)
	if _, err := BuildReplacement(Config{Guardfile: gf}); err == nil {
		t.Fatal("a guardfile declaring no `replace` must not build a replacement")
	}
}

func TestUngrantedVerbRefusesStatingUmbra(t *testing.T) {
	gf := parseOrFail(t, replaceGuardfile)
	err := RefuseUngranted(gf, "rebase")
	if got := exitcode.Of(err); got != exitcode.PolicyDenied {
		t.Errorf("exit code = %d, want PolicyDenied (%d)", got, exitcode.PolicyDenied)
	}
	// Under occlusion the caller cannot see the tool, so the refusal has to
	// name what stands here and how to ask it what it is.
	for _, want := range []string{"not granted", "--help", IdentifyEnv} {
		if !strings.Contains(err.Error()+hintOf(err), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
}

// hintOf reads the recovery sentence off a coded error, empty when absent.
func hintOf(err error) string {
	var coded *exitcode.CodedError
	if errors.As(err, &coded) {
		return coded.HintText()
	}
	return ""
}
