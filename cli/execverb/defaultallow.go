// Default-allow: the wrapped tool's whole surface passes except what the
// guardfile names. The inversion of umbra's usual shape, so it is declared
// rather than inferred. See docs/execverb-default-allow.md.

package execverb

import (
	"context"
	"fmt"
	"os"
	"strings"

	kdl "github.com/calico32/kdl-go"
	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/valuesource"
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
	wrap      func(verb.Spec) cli.ActionFunc
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
	wrap, run, host := cfg.defaults()
	if !gf.DefaultAllow.Declared {
		return &Fallback{gf: gf, wrap: wrap}, nil
	}
	if gf.Replace {
		// A replacement wins PATH under the tool's own name, so a bare name
		// handed to exec resolves back to this process. See realbin.go.
		run = occludeRunner(run)
	}
	return &Fallback{open: true, gf: gf, wrap: wrap, run: run, host: host, providers: valuesource.Merge(cfg.Providers)}, nil
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
		installUnmatched(root, func(ctx context.Context, cmd *cli.Command, name string) {
			err := fb.refuse(ctx, cmd, name, RefuseUngranted(gf, name))
			fmt.Fprintf(os.Stderr, "%s: %v\n", gf.Occlude, err)
			os.Exit(exitcode.Of(err))
		})
		return
	}
	installForward(root, fb, func() []string { return os.Args[1:] }, func(err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", gf.label(), err)
		}
		os.Exit(exitcode.Of(err))
	})
}

// installForward is InstallFallback's open half with argv (read at call time)
// and the exit injected, so negative controls observe the binary's own path.
func installForward(root *cli.Command, fb *Fallback, argv func() []string, exit func(error)) {
	handler := func(ctx context.Context, cmd *cli.Command, _ string) {
		exit(fb.forwardAudited(ctx, cmd, argv()))
	}
	// A group parses its own flags before it ever reaches CommandNotFound, so a
	// partially-named group would kill an unnamed sibling's flag on the way in.
	usage := func(ctx context.Context, cmd *cli.Command, _ error, _ bool) error {
		return fb.forwardAudited(ctx, cmd, argv())
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
		return fb.refuse(ctx, nil, "(root flag)", RefuseRootFlag(gf, err))
	}
	return fb.forwardAudited(ctx, nil, os.Args[1:])
}

// label names this guardfile in an error: the occluded tool when it replaces
// one, the wrap path otherwise.
func (gf *Guardfile) label() string {
	if gf.Occlude != "" {
		return gf.Occlude
	}
	return gf.Bin
}

// refuse returns err through the grant pipeline, so a refusal that reached no
// mounted leaf still writes its reject row (umbra#8121).
func (f *Fallback) refuse(ctx context.Context, cmd *cli.Command, name string, err error) error {
	if f == nil || f.wrap == nil {
		return err
	}
	return f.wrap(verb.Spec{Name: auditVerb(f.gf, []string{name}), SkipPolicy: true,
		Action: func(context.Context, *cli.Command) error { return err }})(ctx, cmd)
}

// forwardAudited is Forward through the grant pipeline, so a forwarded call
// writes its accept or reject row like a granted leaf (umbra#8162).
func (f *Fallback) forwardAudited(ctx context.Context, cmd *cli.Command, argv []string) error {
	if f.wrap == nil {
		return f.Forward(ctx, argv)
	}
	return f.wrap(verb.Spec{Name: auditVerb(f.gf, []string{forwardedVerb(argv)}), SkipPolicy: true,
		Action: func(ctx context.Context, _ *cli.Command) error { return f.Forward(ctx, argv) }})(ctx, cmd)
}

// forwardedVerb names a forwarded call by its first positional word, the
// subcommand the unnamed surface was asked for.
func forwardedVerb(argv []string) string {
	for _, a := range argv {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return "(root)"
}
