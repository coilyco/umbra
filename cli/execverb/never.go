package execverb

import (
	"context"
	"fmt"
	"strings"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/urfave/cli/v3"
)

// NeverRule is one `never run <subcommand...>` line: a path refused ahead of
// every grant, including a grant that covers it. See docs/execverb.md.
type NeverRule struct {
	Subcommand []string
}

// Label renders the refused path for help and error text.
func (r NeverRule) Label() string { return strings.Join(r.Subcommand, " ") }

func (r NeverRule) refusal() error {
	return exitcode.New(exitcode.PolicyDenied, "policy_denied",
		fmt.Errorf("`%s` is never allowed by this guardfile", r.Label()),
		"this verb is a `never` in the Guardfile and reaches no binary")
}

// validateNeverRules refuses a `never run` the engine could not enforce, or one that
// contradicts a grant or stub for the same path.
func validateNeverRules(gf *Guardfile) error {
	if len(gf.NeverRules) == 0 {
		return nil
	}
	if len(gf.Allow) > 0 {
		return fmt.Errorf("execverb: `never run` cannot sit beside an `allow` funnel, which takes the whole binary; use `never pass` (fail-closed)")
	}
	taken := map[string]string{}
	for _, g := range gf.Grants {
		taken[strings.Join(g.Subcommand, " ")] = "grants"
	}
	for _, w := range gf.Withheld {
		taken[w.Label()] = "withholds"
	}
	for _, r := range gf.NeverRules {
		if what, ok := taken[r.Label()]; ok {
			return fmt.Errorf("execverb: `never run %q` names a verb this guardfile also %s: keep one", r.Label(), what)
		}
		taken[r.Label()] = "refuses with never run"
	}
	return nil
}

// mountNeverRules mounts each rule as a leaf that refuses every call, so the refusal
// names the rule rather than reading as an ungranted verb.
func mountNeverRules(root *cli.Command, gf *Guardfile, wrap func(verb.Spec) cli.ActionFunc) {
	for _, r := range gf.NeverRules {
		parent := root
		for _, seg := range r.Subcommand[:len(r.Subcommand)-1] {
			parent = findOrCreateGroup(parent, seg)
		}
		rule := r
		parent.Commands = append(parent.Commands, &cli.Command{
			Name:            r.Subcommand[len(r.Subcommand)-1],
			Usage:           "NOT AVAILABLE - never allowed by policy.",
			SkipFlagParsing: true,
			// Through the grant pipeline, so the refusal writes its reject row (umbra#8121).
			Action: wrap(verb.Spec{Name: auditVerb(gf, rule.Subcommand), SkipPolicy: true,
				Action: func(context.Context, *cli.Command) error { return rule.refusal() }}),
		})
	}
}

// checkNeverRules refuses a call on a covering grant whose positionals continue into
// a `never` path, which routing misses when a flag precedes the next word.
func checkNeverRules(gf *Guardfile, g Grant, args []string) error {
	var pos []string
	for _, r := range gf.NeverRules {
		if len(r.Subcommand) <= len(g.Subcommand) || !hasPrefix(r.Subcommand, g.Subcommand) {
			continue
		}
		if pos == nil {
			pos = positionals(args, g.ValueFlags...)
		}
		if hasPrefix(pos, r.Subcommand[len(g.Subcommand):]) {
			return r.refusal()
		}
	}
	return nil
}

func hasPrefix(s, prefix []string) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i := range prefix {
		if s[i] != prefix[i] {
			return false
		}
	}
	return true
}

// validateRefusals checks the two stated refusals, withhold stubs and never rules.
func validateRefusals(gf *Guardfile) error {
	if err := validateWithheld(gf); err != nil {
		return err
	}
	return validateNeverRules(gf)
}

// auditVerb names an audit row the way a grant's is named.
func auditVerb(gf *Guardfile, sub []string) string {
	return strings.Join(gf.Group, ".") + "." + strings.Join(sub, ".")
}
