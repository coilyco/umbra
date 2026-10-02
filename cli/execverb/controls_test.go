package execverb

import (
	"context"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/negcontrol"
)

func controlsFor(t *testing.T, src string) []negcontrol.Control {
	t.Helper()
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cs, err := NegativeControls(context.Background(), gf)
	if err != nil {
		t.Fatalf("NegativeControls: %v", err)
	}
	return cs
}

func TestNegativeControlsHoldForEnforcedRules(t *testing.T) {
	cs := controlsFor(t, `wrap ward gh {
		exec gh
		replace
		default-allow { reason "test fixture: one boundary named" }
		withhold pr merge { reason "merges stay with the lane" }
		never run "repo delete"
	}`)
	if len(cs) != 2 {
		t.Fatalf("got %d controls, want one per rule: %+v", len(cs), cs)
	}
	for _, c := range cs {
		if !c.Holds() {
			t.Errorf("%s %q does not hold: observed %s, without it %s", c.Kind, c.Rule, c.Observed, c.Without)
		}
		// A replacement under default-allow forwards the call once either rule is gone.
		if c.Without != "reached the upstream" {
			t.Errorf("%s %q without the rule: %s, want the call to reach the binary", c.Kind, c.Rule, c.Without)
		}
	}
}

// The umbra#8120 shape: a covering parent grant. The control must see the
// never path reach the binary once the rule is gone.
func TestNegativeControlUnderCoveringGrant(t *testing.T) {
	cs := controlsFor(t, `wrap ward git {
		exec git
		can run reflog
		never run "reflog expire"
	}`)
	if len(cs) != 1 || !cs[0].Holds() || cs[0].Without != "reached the upstream" {
		t.Fatalf("controls = %+v, want one holding control whose removal spawns", cs)
	}
}

// The gate as a test: a rule the run cannot hold is named, never dropped.
func TestSummarizeControlsNamesEveryFailure(t *testing.T) {
	held, failed := negcontrol.Summarize([]negcontrol.Control{
		{Kind: "never", Rule: "a", Refused: true, LoadBearing: true},
		{Kind: "never", Rule: "b", Refused: true, LoadBearing: false, Observed: "exit 2: x", Without: "exit 2: x"},
		{Kind: "withhold", Rule: "c", Refused: false, LoadBearing: true, Observed: "reached the upstream"},
	})
	if held != 1 || len(failed) != 2 {
		t.Fatalf("held=%d failed=%v, want 1 held and both failures named", held, failed)
	}
	if !strings.Contains(failed[0], "removing it changes nothing") || !strings.Contains(failed[1], "not refused by its own text") {
		t.Errorf("failures = %v, want each reason stated", failed)
	}
}
