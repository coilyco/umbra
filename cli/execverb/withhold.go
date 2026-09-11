package execverb

import (
	"context"
	"fmt"
	"strings"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
	kdl "github.com/calico32/kdl-go"
	"github.com/urfave/cli/v3"
)

// WithheldStub is one deliberately-omitted verb, stated so it can be seen.
// See docs/execverb.md.
type WithheldStub struct {
	Subcommand  []string
	Reason      string
	Alternative []string
}

// Label renders the withheld verb's path for help and error text.
func (w WithheldStub) Label() string { return strings.Join(w.Subcommand, " ") }

// parseWithhold reads a `withhold <subcommand...> { reason ...; alternative ... }`
// node. Absence alone cannot say whether a verb is refused or simply missing.
func parseWithhold(n *kdl.Node) (WithheldStub, error) {
	var w WithheldStub
	for _, a := range n.Arguments() {
		w.Subcommand = append(w.Subcommand, a.String())
	}
	if len(w.Subcommand) == 0 {
		return w, fmt.Errorf("execverb: `withhold` needs the subcommand it occupies, e.g. `withhold repo delete`")
	}
	if len(n.Properties()) > 0 {
		return w, fmt.Errorf("execverb: `withhold %s` takes no properties, only `reason` and `alternative` children (fail-closed)", w.Label())
	}
	for _, c := range n.Children().Nodes {
		if err := applyWithholdChild(&w, c); err != nil {
			return w, err
		}
	}
	// Required: a stub that does not say why is the silence it replaces, louder.
	if w.Reason == "" {
		return w, fmt.Errorf("execverb: `withhold %s` needs a `reason`: a stub with no stated reason restates the absence it replaces", w.Label())
	}
	return w, nil
}

func applyWithholdChild(w *WithheldStub, c *kdl.Node) error {
	switch c.Name() {
	case "reason":
		if len(c.Arguments()) != 1 {
			return fmt.Errorf("execverb: `withhold %s`: `reason` takes one string", w.Label())
		}
		if w.Reason != "" {
			return fmt.Errorf("execverb: `withhold %s` has a duplicate `reason`", w.Label())
		}
		w.Reason = c.Arguments()[0].String()
		return nil
	case "alternative":
		if len(c.Arguments()) == 0 {
			return fmt.Errorf("execverb: `withhold %s`: `alternative` names a granted verb", w.Label())
		}
		if len(w.Alternative) > 0 {
			return fmt.Errorf("execverb: `withhold %s` has a duplicate `alternative`", w.Label())
		}
		for _, a := range c.Arguments() {
			w.Alternative = append(w.Alternative, a.String())
		}
		return nil
	default:
		return fmt.Errorf("execverb: unknown `withhold` child %q (want reason | alternative; fail-closed)", c.Name())
	}
}

// validateWithheld refuses a stub that shadows a granted verb, or names an
// alternative no grant mints. Both send a caller somewhere that does not exist.
func validateWithheld(gf *Guardfile) error {
	if len(gf.Withheld) == 0 {
		return nil
	}
	// A funnel turns the group itself into the leaf, so anything mounted
	// beneath it is unreachable rather than merely redundant.
	if len(gf.Allow) > 0 {
		return fmt.Errorf("execverb: `withhold` needs named `can run` grants: an `allow` funnel takes the whole group, so a stub under it is unreachable (fail-closed)")
	}
	granted := map[string]bool{}
	for _, g := range gf.Grants {
		if g.Wildcard {
			return fmt.Errorf("execverb: `withhold` needs named `can run` grants, not a wildcard funnel: the funnel takes the whole group, so a stub under it is unreachable (fail-closed)")
		}
		granted[strings.Join(g.Subcommand, " ")] = true
	}
	seen := map[string]bool{}
	for _, w := range gf.Withheld {
		label := w.Label()
		if seen[label] {
			return fmt.Errorf("execverb: duplicate `withhold` for %q", label)
		}
		seen[label] = true
		// A stub shadowing a live grant would advertise a working verb as
		// refused, and the grant would look revoked with nothing revoking it.
		if granted[label] {
			return fmt.Errorf("execverb: `withhold %s` names a verb this guardfile grants: remove the grant or the stub, not both", label)
		}
		alt := strings.Join(w.Alternative, " ")
		// Under default-allow every unnamed verb is reachable, so an
		// alternative outside the grants is a live path rather than a dead one.
		if alt != "" && !granted[alt] && !gf.DefaultAllow.Declared {
			return fmt.Errorf("execverb: `withhold %s` names alternative %q, which this guardfile does not grant", label, alt)
		}
	}
	return nil
}

// mountWithheld mounts each stub as a real leaf that refuses every call. It
// spawns nothing and holds no credential: it converts silence into a statement.
func mountWithheld(root *cli.Command, gf *Guardfile) error {
	for _, w := range gf.Withheld {
		parent := root
		for _, seg := range w.Subcommand[:len(w.Subcommand)-1] {
			parent = findOrCreateGroup(parent, seg)
		}
		leafName := w.Subcommand[len(w.Subcommand)-1]
		if findChild(parent, leafName) != nil {
			return fmt.Errorf("execverb: `withhold %s` collides with a mounted command", w.Label())
		}
		parent.Commands = append(parent.Commands, &cli.Command{
			Name:            leafName,
			Usage:           withheldUsage(w),
			SkipFlagParsing: true,
			Action:          withheldAction(w),
		})
	}
	return nil
}

// withheldUsage leads with the refusal, so a reader scanning only the first
// clause of --help still rules the verb out.
func withheldUsage(w WithheldStub) string {
	var b strings.Builder
	b.WriteString("NOT AVAILABLE - withheld by policy. ")
	b.WriteString(w.Reason)
	if len(w.Alternative) > 0 {
		b.WriteString(" Use `")
		b.WriteString(strings.Join(w.Alternative, " "))
		b.WriteString("` instead.")
	}
	return b.String()
}

// withheldAction refuses every call as policy_denied, so a caller checking the
// exit code reads a refusal rather than a failure to spawn.
func withheldAction(w WithheldStub) cli.ActionFunc {
	return func(context.Context, *cli.Command) error {
		hint := "this verb is withheld by the Guardfile and reaches no binary"
		if len(w.Alternative) > 0 {
			hint = fmt.Sprintf("this verb is withheld by the Guardfile; use `%s` instead", strings.Join(w.Alternative, " "))
		}
		return exitcode.New(exitcode.PolicyDenied, "policy_denied",
			fmt.Errorf("`%s` is withheld: %s", w.Label(), w.Reason), hint).
			WithReason("a withheld verb is a stated absence rather than a missing one: it is declared in the guardfile " +
				"so a caller can tell policy from an unimplemented feature, and it reaches no binary and holds no credential")
	}
}
