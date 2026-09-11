// Default-allow: the wrapped tool's whole surface passes except what the
// guardfile names. The inversion of umbra's usual shape, so it is declared
// rather than inferred. See docs/execverb-default-allow.md.

package execverb

import (
	"context"
	"fmt"
	"os"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/valuesource"
	kdl "github.com/calico32/kdl-go"
	"github.com/urfave/cli/v3"
)

// DefaultAllow is the parsed `default-allow` declaration: an unnamed verb is
// forwarded to the real binary rather than refused. Reason is required.
type DefaultAllow struct {
	Declared bool
	Reason   string
}

// parseDefaultAllow reads `default-allow { reason "..." }`. The reason is
// mandatory, and docs/execverb-default-allow.md says why.
func parseDefaultAllow(n *kdl.Node) (DefaultAllow, error) {
	da := DefaultAllow{Declared: true}
	if len(n.Arguments()) > 0 {
		return da, fmt.Errorf("execverb: `default-allow` takes no arguments, only a `reason` child (fail-closed)")
	}
	if len(n.Properties()) > 0 {
		return da, fmt.Errorf("execverb: `default-allow` takes no properties (fail-closed)")
	}
	for _, c := range n.Children().Nodes {
		if c.Name() != "reason" {
			return da, fmt.Errorf("execverb: unknown `default-allow` child %q (want reason; fail-closed)", c.Name())
		}
		if len(c.Arguments()) != 1 {
			return da, fmt.Errorf("execverb: `default-allow`: `reason` takes one string")
		}
		if da.Reason != "" {
			return da, fmt.Errorf("execverb: `default-allow` has a duplicate `reason`")
		}
		da.Reason = c.Arguments()[0].String()
	}
	if da.Reason == "" {
		return da, fmt.Errorf("execverb: `default-allow` needs a `reason`: it inverts the fail-closed default, " +
			"so the guardfile has to say why this tool's unnamed surface is safe to forward")
	}
	return da, nil
}

// Fallback forwards an unmatched call to the real binary. Not a hole: the
// wrap-level guards and env injections a granted leaf faces apply here too.
type Fallback struct {
	// open is the declaration itself. A Fallback that is not open refuses,
	// which keeps the closed default the shape a caller gets by doing nothing.
	open      bool
	gf        *Guardfile
	run       Runner
	host      HostResolver
	providers map[string]valuesource.Provider
}

// Open reports whether this fallback forwards. A closed one refuses.
func (f *Fallback) Open() bool { return f != nil && f.open }

// NewFallback builds the forwarder. A guardfile declaring no default-allow
// yields a closed one, which refuses.
func NewFallback(cfg Config) (*Fallback, error) {
	gf := cfg.Guardfile
	if gf == nil {
		return nil, fmt.Errorf("execverb: Config.Guardfile is nil")
	}
	if !gf.DefaultAllow.Declared {
		return &Fallback{gf: gf}, nil
	}
	_, run, host := cfg.defaults()
	if gf.Replace {
		// A replacement wins PATH under the tool's own name, so a bare name
		// handed to exec resolves back to this process. See realbin.go.
		run = occludeRunner(run)
	}
	return &Fallback{open: true, gf: gf, run: run, host: host, providers: valuesource.Merge(cfg.Providers)}, nil
}

// Forward runs argv against the real binary. argv is the caller's, minus the
// program name: a replacement is the tool, so its argv is the tool's argv.
func (f *Fallback) Forward(ctx context.Context, argv []string) error {
	// The synthesized grant carries no flag policy and no subcommand of its
	// own, so the only refusals it can meet are the wrap-level ones.
	g := Grant{Wildcard: true}
	if err := checkCallPolicy(ctx, f.gf, g, nil, argv, f.host); err != nil {
		return err
	}
	env, err := resolveEnv(ctx, f.gf, f.providers)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "check the env value provider address and credentials")
	}
	full := append(append([]string{}, f.gf.ArgvPrefix...), argv...)
	if err := f.run(ctx, f.gf.Bin, full, env); err != nil {
		return exitcode.New(exitcode.UpstreamFailed, "upstream_failed", err, "the wrapped command failed")
	}
	return nil
}

// InstallFallback sets what happens to an unmounted name: forwarded under
// default-allow, refused otherwise. Every group, not just the root.
func InstallFallback(root *cli.Command, gf *Guardfile, fb *Fallback) {
	if !fb.Open() {
		InstallRefusal(root, gf)
		return
	}
	handler := func(ctx context.Context, _ *cli.Command, _ string) {
		if err := fb.Forward(ctx, os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", gf.label(), err)
			os.Exit(exitcode.Of(err))
		}
		os.Exit(0)
	}
	// A group parses its own flags before it ever reaches CommandNotFound, so a
	// partially-named group would kill an unnamed sibling's flag on the way in.
	usage := func(ctx context.Context, _ *cli.Command, _ error, _ bool) error {
		return fb.Forward(ctx, os.Args[1:])
	}
	var walk func(cmds []*cli.Command)
	walk = func(cmds []*cli.Command) {
		for _, c := range cmds {
			c.CommandNotFound = handler
			c.OnUsageError = usage
			walk(c.Commands)
		}
	}
	root.CommandNotFound = handler
	walk(root.Commands)
}

// RootFlagFallback answers a flag the root does not define: forwarded under
// default-allow, since a pre-verb flag is part of the unnamed surface.
func RootFlagFallback(ctx context.Context, gf *Guardfile, fb *Fallback, err error) error {
	if !fb.Open() {
		return RefuseRootFlag(gf, err)
	}
	return fb.Forward(ctx, os.Args[1:])
}

// label names this guardfile in an error: the occluded tool when it replaces
// one, the wrap path otherwise.
func (gf *Guardfile) label() string {
	if gf.Occlude != "" {
		return gf.Occlude
	}
	return gf.Bin
}
