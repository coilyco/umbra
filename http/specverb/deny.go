// Deny enforcement: a `cannot`/`never` grant blocks a (verb,resource) class and
// mounts a teaching leaf that fails closed with its message. A deny beats an allow.

package specverb

import (
	"context"
	"fmt"
	"strings"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/urfave/cli/v3"
)

// denyDescriptor is one resolved deny: its CLI placement and the teaching message
// shown when the blocked leaf is invoked.
type denyDescriptor struct {
	VerbName string
	Group    string
	Leaf     string
	Message  string
}

// deniedKeys maps each (verb,resource) a cannot/never grant blocks to that grant,
// so resolveDescriptors can drop a matching `can` (defense in depth: deny wins).
func deniedKeys(gf *guardfile.Guardfile) map[grantKey]guardfile.Grant {
	keys := map[grantKey]guardfile.Grant{}
	for _, g := range gf.Grants {
		if g.Modal == "cannot" || g.Modal == "never" {
			keys[grantKey{Verb: g.Verb, Resource: g.Resource}] = g
		}
	}
	return keys
}

// denyDescriptors resolves every cannot/never grant into a teaching leaf (first-seen
// order); a deny an `override can` lifts mounts none. See specverb-policy.md.
func denyDescriptors(gf *guardfile.Guardfile) []denyDescriptor {
	overridden := overriddenKeys(gf)
	var out []denyDescriptor
	for _, g := range gf.Grants {
		if g.Modal != "cannot" && g.Modal != "never" {
			continue
		}
		if overridden[grantKey{Verb: g.Verb, Resource: g.Resource}] {
			continue // crossed by an override; the allow leaf takes this path
		}
		out = append(out, denyDescriptor{
			VerbName: strings.Join(gf.Group, ".") + "." + g.Resource + "." + g.Verb,
			Group:    g.Resource,
			Leaf:     g.Verb,
			Message:  denyMessage(g),
		})
	}
	return out
}

// denyMessage is the teaching error a deny surfaces: the authored `message "..."`,
// or a generic fail-closed sentence naming the blocked verb+resource.
func denyMessage(g guardfile.Grant) string {
	if g.Message != "" {
		return g.Message
	}
	return fmt.Sprintf("denied by policy: this guardrail forbids `%s %s`", g.Verb, g.Resource)
}

// buildDenyLeaf mounts a denied (verb,resource) as a leaf that fails closed with
// the teaching message, so an operator who reaches for it learns why, not "?".
func (rt *runtime) buildDenyLeaf(d denyDescriptor) *cli.Command {
	return &cli.Command{
		Name:        d.Leaf,
		Usage:       "denied by policy",
		Description: d.Message,
		Action: rt.wrap(verb.Spec{
			Name:       d.VerbName,
			SkipPolicy: true, // the deny is the policy; there is no argv to gate
			Action: func(context.Context, *cli.Command) error {
				return denyError(d.Message)
			},
		}),
	}
}

// denyError is the refusal a deny leaf returns, shared with the negative controls.
func denyError(message string) error {
	return exitcode.New(exitcode.PolicyDenied, "policy_denied",
		fmt.Errorf("%s", message), "this operation is blocked by a guardrail and cannot be run")
}
