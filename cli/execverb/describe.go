// The describe model for the exec dialect: the in-engine view of the mounted
// surface and source for live describe output. See docs/execverb.md.

package execverb

import (
	"fmt"
	"strings"
)

// Surface is the in-engine model of a mounted exec surface: the wrapped binary,
// the fixed argv prefix, and every granted subcommand leaf.
type Surface struct {
	Group       []string    `json:"group"`                 // command path, e.g. ["<cli>","ops","<tool>"]
	Description string      `json:"description,omitempty"` // the guardfile's top-level `description` prose, "" when none
	Bin         string      `json:"bin"`                   // the wrapped binary, fixed at parse; empty for an inspect list
	ArgvPrefix  []string    `json:"argv_prefix,omitempty"` // unoverridable leading argv
	Env         []string    `json:"env,omitempty"`         // env injections as "NAME = provider address" (source, never the resolved value)
	Inspect     bool        `json:"inspect,omitempty"`     // the `allow` inspect-list shape: each grant funnels its own binary, no single Bin
	Guards      []string    `json:"guards,omitempty"`      // wrap-level guards (the passthrough host gate), applied to every leaf
	Grants      []GrantInfo `json:"grants"`                // every mounted leaf, in mount order

	// DefaultAllow is the `default-allow` reason, empty when the wrap is closed.
	// Without it a reader reads the grants as the whole surface.
	DefaultAllow string `json:"default_allow,omitempty"`
}

// GrantInfo is one mounted grant: its CLI placement, the real invocation it
// authorizes, its flag policy, and any gates/guards that can still refuse it.
type GrantInfo struct {
	Name       string   `json:"name"`                  // dotted audit name, e.g. <cli>.ops.<tool>.<sub>.<verb>
	Subcommand []string `json:"subcommand,omitempty"`  // e.g. ["s3","ls"]; empty for the wildcard
	Bin        string   `json:"bin,omitempty"`         // the funnel's own binary, set only for an inspect-list (`allow`) leaf
	Exec       []string `json:"exec,omitempty"`        // tokens appended after the binary: the argv override when set, else the subcommand
	Wildcard   bool     `json:"wildcard"`              // `can run *`: the whole binary passes through
	Sealed     bool     `json:"sealed,omitempty"`      // `sealed`: pinned argv forwards exactly, no trailing caller args
	Describe   string   `json:"describe,omitempty"`    // Guardfile describe note
	AllowFlags []string `json:"allow_flags,omitempty"` // non-empty: strict flag allowlist
	DenyFlags  []string `json:"deny_flags,omitempty"`  // default-allow minus these
	Gates      []string `json:"gates,omitempty"`       // registered preflight gate names
	Guards     []string `json:"guards,omitempty"`      // rendered when/deny-when guards
}

// Describe builds the surface model for an exec Guardfile without mounting a
// command tree, so the driver and docs can read it directly.
func Describe(gf *Guardfile) *Surface {
	if gf == nil {
		return &Surface{}
	}
	if len(gf.Allow) > 0 {
		return describeAllow(gf)
	}
	s := &Surface{Group: gf.Group, Description: gf.Description, Bin: gf.Bin, ArgvPrefix: gf.ArgvPrefix}
	s.DefaultAllow = gf.DefaultAllow.Reason
	for _, e := range gf.Env {
		s.Env = append(s.Env, e.Name+" = "+strings.TrimSpace(e.Provider+" "+e.Address))
	}
	for _, wc := range gf.Whens {
		s.Guards = append(s.Guards, guardSentence(wc))
	}
	for _, g := range gf.Grants {
		s.Grants = append(s.Grants, grantInfo(gf, g))
	}
	return s
}

// describeAllow renders the `allow` inspect-list surface: one open-passthrough
// leaf per binary (each its own Bin), carrying the wrap-level guards.
func describeAllow(gf *Guardfile) *Surface {
	s := &Surface{Group: gf.Group, Description: gf.Description, Inspect: true}
	for _, bin := range gf.Allow {
		member := allowMember(gf, bin)
		gi := grantInfo(member, member.Grants[0])
		gi.Bin = bin
		gi.Subcommand = []string{bin}
		s.Grants = append(s.Grants, gi)
	}
	return s
}

// grantInfo flattens one Grant into its describe view, naming the gates and
// rendering the guards in the same vocabulary the Guardfile uses.
func grantInfo(gf *Guardfile, g Grant) GrantInfo {
	name := strings.Join(gf.Group, ".")
	if !g.Wildcard {
		name += "." + strings.Join(g.Subcommand, ".")
	}
	gi := GrantInfo{
		Name:       name,
		Subcommand: g.Subcommand,
		Exec:       g.ExecArgv(),
		Wildcard:   g.Wildcard,
		Sealed:     g.Sealed,
		Describe:   g.Describe,
		AllowFlags: g.AllowFlags,
		DenyFlags:  g.DenyFlags,
	}
	for _, gs := range g.Gates {
		gi.Gates = append(gi.Gates, gs.Name)
	}
	for _, wc := range g.Whens {
		gi.Guards = append(gi.Guards, guardSentence(wc))
	}
	return gi
}

// guardSentence renders one when/deny-when (or never pass/only pass) guard as a
// readable clause, naming the shell source for an ambient guard.
func guardSentence(wc WhenClause) string {
	verb := "requires"
	if wc.Deny {
		verb = "denies when"
	}
	selector := wc.Selector
	if len(wc.SourceCmd) > 0 {
		selector = "shell " + strings.Join(wc.SourceCmd, " ")
	}
	clause := fmt.Sprintf("%s %s matches %s", verb, selector, strings.Join(wc.Patterns, " or "))
	if wc.Describe != "" {
		clause += " - " + wc.Describe
	}
	return clause
}

// Markdown renders the exec dialect's pulled describe surface.
func (s *Surface) Markdown() string {
	if s.Inspect {
		return s.markdownInspect()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", strings.Join(s.Group, " "))
	if s.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", s.Description)
	}
	invocation := strings.TrimSpace(s.Bin + " " + strings.Join(s.ArgvPrefix, " "))
	fmt.Fprintf(&b, "Exec-dialect CLI. Every verb runs `%s` with the granted subcommand (or its `argv` override) appended; the binary and its prefix are fixed and the caller can never substitute them.\n", invocation)
	if len(s.Env) > 0 {
		fmt.Fprintf(&b, "\nEnv set on the process (resolved at exec time): %s.\n", joinCode(s.Env))
	}
	if len(s.Guards) > 0 {
		b.WriteString("\nWrap-level guards (enforced on every verb, before any exec):\n\n")
		for _, guard := range s.Guards {
			fmt.Fprintf(&b, "- %s\n", guard)
		}
	}

	prefix := strings.Join(s.Group, " ")
	for _, g := range s.Grants {
		leaf := strings.Join(g.Subcommand, " ")
		if g.Wildcard {
			leaf = "* (open passthrough)"
		}
		heading := fmt.Sprintf("## %s %s", prefix, leaf)
		if g.Describe != "" {
			heading += " - " + g.Describe
		}
		fmt.Fprintf(&b, "\n%s\n\n", heading)
		fmt.Fprintf(&b, "`%s`\n", strings.TrimSpace(invocation+" "+strings.Join(g.Exec, " ")))
		if g.Sealed {
			b.WriteString("\nSealed: the pinned command forwards exactly; no trailing caller arguments are accepted.\n")
		}
		writeFlagPolicy(&b, g)
		writeGuards(&b, g)
	}
	return b.String()
}

// markdownInspect renders the `allow` inspect-list surface: each read-only
// binary is its own open passthrough, with any wrap-level guard noted per leaf.
func (s *Surface) markdownInspect() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", strings.Join(s.Group, " "))
	if s.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", s.Description)
	}
	fmt.Fprintf(&b, "Inspect-list CLI: %d read-only binaries, each an independent open passthrough. Every leaf runs its named binary verbatim with the caller's args; the binary set is fixed at parse and the caller can never reach one not listed.\n", len(s.Grants))
	prefix := strings.Join(s.Group, " ")
	for _, g := range s.Grants {
		heading := fmt.Sprintf("## %s %s", prefix, g.Bin)
		if g.Describe != "" {
			heading += " - " + g.Describe
		}
		fmt.Fprintf(&b, "\n%s\n\n", heading)
		fmt.Fprintf(&b, "`%s <args...>` (open passthrough)\n", g.Bin)
		writeGuards(&b, g)
	}
	return b.String()
}

// writeFlagPolicy states the grant's flag rule: a strict allowlist, a denylist,
// or unrestricted passthrough.
func writeFlagPolicy(b *strings.Builder, g GrantInfo) {
	switch {
	case len(g.AllowFlags) > 0:
		fmt.Fprintf(b, "\nFlags: only %s allowed (strict allowlist).\n", joinCode(g.AllowFlags))
	case len(g.DenyFlags) > 0:
		fmt.Fprintf(b, "\nFlags: all allowed except %s.\n", joinCode(g.DenyFlags))
	default:
		b.WriteString("\nFlags: unrestricted passthrough.\n")
	}
}

// writeGuards renders the preflight gates and argv guards that can refuse a
// call before it reaches the binary.
func writeGuards(b *strings.Builder, g GrantInfo) {
	if len(g.Gates) == 0 && len(g.Guards) == 0 {
		return
	}
	b.WriteString("\nPreflight:\n\n")
	for _, name := range g.Gates {
		fmt.Fprintf(b, "- gate `%s`\n", name)
	}
	for _, guard := range g.Guards {
		fmt.Fprintf(b, "- %s\n", guard)
	}
}

// joinCode renders a flag list as comma-separated code spans.
func joinCode(flags []string) string {
	quoted := make([]string, len(flags))
	for i, f := range flags {
		quoted[i] = "`" + f + "`"
	}
	return strings.Join(quoted, ", ")
}
