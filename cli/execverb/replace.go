// Occluded replacement wrappers: the generated binary is installed under the
// wrapped tool's own name rather than as a verb under a driver's tree. See
// docs/execverb-replacement.md.

package execverb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kdl "github.com/calico32/kdl-go"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/urfave/cli/v3"
)

// IdentifyEnv asks a replacement to state what it is and exit. It cannot be a
// flag: every flag belongs to the tool being occluded.
const IdentifyEnv = "UMBRA_IDENTIFY"

// parseReplace reads `replace` or `replace "<name>"`, whose optional argument
// overrides the name taken from the wrapped binary.
func (gf *Guardfile) parseReplace(n *kdl.Node) error {
	if len(n.Properties()) > 0 {
		return fmt.Errorf("execverb: `replace` takes no properties (fail-closed)")
	}
	if len(n.Children().Nodes) > 0 {
		return fmt.Errorf("execverb: `replace` takes no body (fail-closed)")
	}
	args := stringArgs(n)
	if len(args) > 1 {
		return fmt.Errorf("execverb: `replace` expects at most one occluded name, got %d (fail-closed)", len(args))
	}
	gf.Replace = true
	if len(args) == 1 {
		gf.Occlude = args[0]
	}
	return nil
}

// resolveOcclusion fixes the name a replacement is installed under, defaulting
// to the wrapped binary's basename rather than the wrap path's last segment.
func (gf *Guardfile) resolveOcclusion() error {
	if !gf.Replace {
		return nil
	}
	name := gf.Occlude
	if name == "" {
		name = filepath.Base(gf.Bin)
	}
	if err := validOccludedName(name); err != nil {
		return err
	}
	gf.Occlude = name
	return nil
}

// validOccludedName rejects anything that is not a bare basename: the name
// becomes a filename on a PATH directory.
func validOccludedName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("execverb: `replace` resolved an empty occluded name (fail-closed)")
	case name == "." || name == "..":
		return fmt.Errorf("execverb: `replace` name %q is a path segment rather than a binary (fail-closed)", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("execverb: `replace` name %q contains a path separator; a replacement is installed by bare name on a PATH directory (fail-closed)", name)
	}
	return nil
}

// OccludedName is the binary name a replacement stands in front of, empty when
// the guardfile declares no `replace`.
func (gf *Guardfile) OccludedName() string { return gf.Occlude }

// BuildReplacement builds the guarded tree rooted at the wrapped tool, so a
// grant is a top-level verb. Refuses a guardfile declaring no `replace`.
func BuildReplacement(cfg Config) (*cli.Command, error) {
	gf := cfg.Guardfile
	if gf == nil {
		return nil, fmt.Errorf("execverb: Config.Guardfile is nil")
	}
	if !gf.Replace {
		return nil, fmt.Errorf("execverb: guardfile %q declares no `replace`, so it cannot build a replacement (fail-closed)", strings.Join(gf.Group, " "))
	}
	root, err := Build(cfg)
	if err != nil {
		return nil, err
	}
	root.Name = gf.Occlude
	root.Usage = fmt.Sprintf("%s, occluded by umbra to the verbs %s grants", gf.Occlude, strings.Join(gf.Group, " "))
	return root, nil
}

// Identify writes what stands in the wrapped tool's place, the only surface
// that says umbra is here.
func Identify(w *os.File, gf *Guardfile, version string) {
	_, _ = fmt.Fprintf(w, "umbra replacement for %q\n", gf.Occlude)
	_, _ = fmt.Fprintf(w, "  guardfile: %s\n", strings.Join(gf.Group, " "))
	_, _ = fmt.Fprintf(w, "  wrapped binary: %s\n", gf.Bin)
	_, _ = fmt.Fprintf(w, "  driver version: %s\n", version)
	resolved, err := ResolveReal(gf.Bin)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  real binary: unresolved (%v)\n", err)
		return
	}
	_, _ = fmt.Fprintf(w, "  real binary: %s\n", resolved)
}

// RefuseUngranted refuses a verb the guardfile does not grant. It cannot tell a
// denied verb from a misspelt one, so it says only that the name is not granted.
func RefuseUngranted(gf *Guardfile, name string) error {
	// The recovery rides in the message too: a generated binary prints the error
	// text and nothing else, so a hint alone never reaches the reader.
	return exitcode.New(exitcode.PolicyDenied, "policy_denied",
		fmt.Errorf("`%s %s` is not granted; run `%s --help` for the verbs this binary has", gf.Occlude, name, gf.Occlude),
		fmt.Sprintf("run `%s --help` for the verbs this guardfile grants, or `%s=1 %s` for what stands here", gf.Occlude, IdentifyEnv, gf.Occlude)).
		WithReason("this binary is an umbra replacement occluding " + gf.Occlude + ": it mounts the verbs the guardfile grants and nothing else, " +
			"so an unmounted name is refused rather than forwarded")
}

// RefuseRootFlag answers a flag the root does not define. A replacement
// registers none of its own, and `--version` is the flag that finds this first.
func RefuseRootFlag(gf *Guardfile, err error) error {
	return exitcode.New(exitcode.PolicyDenied, "policy_denied",
		fmt.Errorf("%w; this binary registers no flags of its own, so a flag goes after the verb that takes it", err),
		fmt.Sprintf("`%s=1 %s` states what stands here", IdentifyEnv, gf.Occlude)).
		WithReason("an umbra replacement occludes " + gf.Occlude + " and mounts only the verbs its guardfile grants, so a flag the tool would accept " +
			"before a verb is not registered here rather than silently passed through")
}

// replacementNotFound is the CommandNotFound handler a replacement installs.
func replacementNotFound(gf *Guardfile) func(context.Context, *cli.Command, string) {
	return func(_ context.Context, _ *cli.Command, name string) {
		err := RefuseUngranted(gf, name)
		fmt.Fprintf(os.Stderr, "%s: %v\n", gf.Occlude, err)
		os.Exit(exitcode.Of(err))
	}
}

// InstallRefusal sets the refusal on root and every group under it: urfave
// resolves a subcommand against the group that owns it.
func InstallRefusal(root *cli.Command, gf *Guardfile) {
	installUnmatched(root, replacementNotFound(gf))
}

func installUnmatched(root *cli.Command, handler func(context.Context, *cli.Command, string)) {
	var walk func(cmds []*cli.Command)
	walk = func(cmds []*cli.Command) {
		for _, c := range cmds {
			c.CommandNotFound = handler
			walk(c.Commands)
		}
	}
	root.CommandNotFound = handler
	walk(root.Commands)
}
