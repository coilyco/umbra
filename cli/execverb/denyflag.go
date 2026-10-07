package execverb

import (
	"fmt"
	"strings"

	kdl "github.com/calico32/kdl-go"
)

// parseDenyFlag reads `deny-flag "<spelling>" aliases="<a,b>|none"` into every
// spelling refused, primary first. A bare deny-flag fails to parse: docs/execverb.md.
func parseDenyFlag(c *kdl.Node, grant string) ([]string, error) {
	primary := c.Arguments()[0].String()
	if err := checkSpelling(primary); err != nil {
		return nil, fmt.Errorf("execverb: grant %q: deny-flag %q: %w", grant, primary, err)
	}
	raw, ok := "", false
	for k, v := range c.Properties() {
		if k != "aliases" {
			return nil, fmt.Errorf("execverb: grant %q: deny-flag %q: unknown property %q (want aliases; fail-closed)", grant, primary, k)
		}
		raw, ok = v.String(), true
	}
	if !ok {
		return nil, fmt.Errorf("execverb: grant %q: deny-flag %q does not say what else the wrapped binary accepts for it. "+
			`umbra cannot read that binary's alias table, so name the other spellings (aliases="-n") or state there are none (aliases="none"): `+
			"an unlisted alias is a way around the denial (fail-closed)", grant, primary)
	}
	out := []string{primary}
	if raw == "none" {
		return out, nil
	}
	for _, alias := range strings.Split(raw, ",") {
		alias = strings.TrimSpace(alias)
		if err := checkSpelling(alias); err != nil {
			return nil, fmt.Errorf("execverb: grant %q: deny-flag %q: alias %q: %w", grant, primary, alias, err)
		}
		out = append(out, alias)
	}
	return out, nil
}

// checkSpelling refuses a denied spelling the argv scan could never match: it
// scans only tokens that start with a dash, so anything else denies nothing.
func checkSpelling(s string) error {
	if len(s) < 2 || s[0] != '-' || s == "--" || strings.ContainsAny(s, "= \t") {
		return fmt.Errorf("must be a flag spelling such as --name or -n, with no `=` or space")
	}
	return nil
}

// deniedBy reports which denied spelling a flag name stands for: exact, a short
// flag inside a bundle, or a long flag cut short. See docs/execverb.md.
func deniedBy(name string, deny []string) (string, bool) {
	for _, d := range deny {
		switch {
		case name == d:
			return d, true
		case isShortFlag(d) && isShortCluster(name) && strings.ContainsRune(name[1:], rune(d[1])):
			return d, true
		case strings.HasPrefix(d, "--") && strings.HasPrefix(name, "--") && len(name) > 2 && len(name) < len(d) && strings.HasPrefix(d, name):
			return d, true
		}
	}
	return "", false
}

func isShortFlag(s string) bool { return len(s) == 2 && s[0] == '-' && s[1] != '-' }

func isShortCluster(s string) bool { return len(s) > 2 && s[0] == '-' && s[1] != '-' }
