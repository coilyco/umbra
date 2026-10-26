package execverb

import (
	"fmt"
	"strings"
)

// FlagPin fixes one flag's value for a grant: umbra supplies it, and a caller
// passing a different value is refused. See docs/execverb.md.
type FlagPin struct {
	Flag  string
	Value string
}

// applyPins refuses a conflicting value in any occurrence, then supplies each
// pin the caller omitted. Runs before the gates, so a guard reads what will run.
func applyPins(args []string, g Grant) ([]string, error) {
	if len(g.Pins) == 0 {
		return args, nil
	}
	out := append([]string{}, args...)
	for _, p := range g.Pins {
		got := flagValues(out, p.Flag)
		if len(got) == 0 {
			out = append(out, p.Flag, p.Value)
			continue
		}
		for _, v := range got {
			if v != p.Value {
				return nil, fmt.Errorf(
					"`%s` pins %s to %q, and this call passes %q. Drop the flag and let the guardfile supply it",
					g.subcommandLabel(), p.Flag, p.Value, v)
			}
		}
	}
	return out, nil
}

// parsePin reads a `pin "<--flag>" "<value>"` clause: the flag umbra supplies
// and the only value it may carry.
func parsePin(g *Grant, values []string) error {
	if len(values) != 2 {
		return fmt.Errorf("`pin` wants a flag and its value, e.g. `pin \"--type\" \"SecureString\"` (got %d argument(s))", len(values))
	}
	flag, value := values[0], values[1]
	if !strings.HasPrefix(flag, "--") {
		return fmt.Errorf("`pin` takes a long flag starting with `--` (got %q)", flag)
	}
	if value == "" {
		return fmt.Errorf("`pin %s` needs a value: pinning a flag to nothing states no constraint", flag)
	}
	for _, existing := range g.Pins {
		if existing.Flag == flag {
			return fmt.Errorf("duplicate `pin` for %s: a flag has one pinned value", flag)
		}
	}
	g.Pins = append(g.Pins, FlagPin{Flag: flag, Value: value})
	g.ValueFlags = append(g.ValueFlags, flag)
	return nil
}
