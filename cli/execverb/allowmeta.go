// Allow-metacharacters: a wrap whose argv reaches the binary through execve,
// never a shell, may carry the bytes the metacharacter gate refuses. See
// docs/execverb-allow-metacharacters.md.

package execverb

import (
	"fmt"

	kdl "github.com/calico32/kdl-go"
)

// AllowMeta is the parsed `allow-metacharacters` declaration. Reason is required.
type AllowMeta struct {
	Declared bool
	Reason   string
}

// parseAllowMeta reads `allow-metacharacters { reason "..." }`. The reason is
// mandatory, as default-allow's is: an undefended lifted gate reads as an accident.
func parseAllowMeta(n *kdl.Node) (AllowMeta, error) {
	am := AllowMeta{Declared: true}
	if len(n.Arguments()) > 0 {
		return am, fmt.Errorf("execverb: `allow-metacharacters` takes no arguments in the exec dialect, only a `reason` child (fail-closed)")
	}
	if len(n.Properties()) > 0 {
		return am, fmt.Errorf("execverb: `allow-metacharacters` takes no properties (fail-closed)")
	}
	for _, c := range n.Children().Nodes {
		if c.Name() != "reason" {
			return am, fmt.Errorf("execverb: unknown `allow-metacharacters` child %q (want reason; fail-closed)", c.Name())
		}
		if len(c.Arguments()) != 1 {
			return am, fmt.Errorf("execverb: `allow-metacharacters`: `reason` takes one string")
		}
		if am.Reason != "" {
			return am, fmt.Errorf("execverb: `allow-metacharacters` has a duplicate `reason`")
		}
		am.Reason = c.Arguments()[0].String()
	}
	if am.Reason == "" {
		return am, fmt.Errorf("execverb: `allow-metacharacters` needs a `reason`: it lifts the shell-metacharacter gate, " +
			"so the guardfile has to say why this binary never hands argv to a shell")
	}
	return am, nil
}
