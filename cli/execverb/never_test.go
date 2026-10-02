package execverb

import (
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/exitcode"
)

const neverText = "is never allowed by this guardfile"

// assertNever holds a call to the rule's own refusal: exit code, text, and no
// spawn. Exit 2 alone would pass on a refusal that came from somewhere else.
func assertNever(t *testing.T, src string, argv ...string) {
	t.Helper()
	var cp capture
	err := runArgv(t, src, &cp, argv...)
	if err == nil {
		t.Fatalf("%v ran %s %v, want a never refusal", argv, cp.bin, cp.argv)
	}
	if c := exitcode.From(err); c == nil || c.Code() != exitcode.PolicyDenied {
		t.Errorf("%v: err = %v, want exit code %d policy_denied", argv, err, exitcode.PolicyDenied)
	}
	if !strings.Contains(err.Error(), neverText) {
		t.Errorf("%v: err = %q, want the never rule's own text", argv, err)
	}
	if cp.bin != "" {
		t.Errorf("%v: refused call still spawned %s %v", argv, cp.bin, cp.argv)
	}
}

// TestNeverBeatsCoveringGrant is umbra#8120: the parent grant used to forward
// the never path to the binary.
func TestNeverBeatsCoveringGrant(t *testing.T) {
	const src = `wrap ward git {
		exec git
		can run reflog
		never run "reflog expire"
	}`
	assertNever(t, src, "git", "reflog", "expire", "--all")
	// A flag ahead of the next word skips routing, so the grant's own check holds it.
	assertNever(t, src, "git", "reflog", "--verbose", "expire")
	var cp capture
	if err := runArgv(t, src, &cp, "git", "reflog", "show"); err != nil {
		t.Fatalf("reflog show refused: %v", err)
	}
}

func TestNeverUncoveredNamesItsRule(t *testing.T) {
	assertNever(t, gitGuardfile, "git", "reflog", "expire")
}

func TestNeverBesideTwoWordGrant(t *testing.T) {
	assertNever(t, `wrap ward redis {
		exec redis-cli
		can run "config get"
		never run "config set"
	}`, "redis", "config", "set", "maxmemory", "1")
}

func TestNeverUnderWildcard(t *testing.T) {
	const src = `wrap ward git {
		exec git
		can run "*"
		never run gc
	}`
	assertNever(t, src, "git", "gc", "--prune=now")
	var cp capture
	if err := runArgv(t, src, &cp, "git", "status"); err != nil {
		t.Fatalf("status refused under the funnel: %v", err)
	}
}

// TestNeverLineIsLoadBearing is the negative control: without the line the same
// call must stop reading as a never refusal.
func TestNeverLineIsLoadBearing(t *testing.T) {
	var cp capture
	err := runArgv(t, `wrap ward git {
		exec git
		can run reflog
	}`, &cp, "git", "reflog", "expire", "--all")
	if err != nil || cp.bin != "git" {
		t.Fatalf("without the never line the call should reach git: err=%v ran=%q", err, cp.bin)
	}
}

func TestNeverContradictionsFailClosed(t *testing.T) {
	for name, src := range map[string]string{
		"same path granted":  "wrap ward git {\n exec git\n can run gc\n never run gc\n}",
		"same path withheld": "wrap ward git {\n exec git\n can run status\n withhold gc {\n reason \"no\"\n }\n never run gc\n}",
		"wildcard never":     "wrap ward git {\n exec git\n can run status\n never run \"*\"\n}",
		"beside allow":       "wrap ward inspect {\n allow cat\n never run cat\n}",
	} {
		t.Run(name, func(t *testing.T) {
			gf, err := Parse([]byte(src))
			if err == nil {
				_, err = Build(Config{Guardfile: gf})
			}
			if err == nil {
				t.Fatal("want a fail-closed error")
			}
		})
	}
}
