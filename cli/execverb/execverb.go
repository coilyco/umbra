// The execverb engine: builds a guarded cli tree from an exec-dialect
// Guardfile, one passthrough leaf per `can run` grant. See docs/execverb.md.

package execverb

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/valuesource"
	"github.com/urfave/cli/v3"
)

// Runner fires the resolved command; env is the `NAME=VALUE` overrides to layer
// over the inherited environment. Injected for tests; nil execs for real.
type Runner func(ctx context.Context, bin string, argv, env []string) error

// HostResolver resolves a `shell <cmd>` selector: it execs cmd and returns the
// trimmed stdout. Injected for tests; nil execs for real. An error fails closed.
type HostResolver func(ctx context.Context, cmd []string) (string, error)

// Config is everything the engine needs to build a command tree.
type Config struct {
	// Guardfile is the parsed exec-dialect policy.
	Guardfile *Guardfile

	// Wrap adapts a verb.Spec into a guarded cli.ActionFunc (the audit + argv
	// pipeline). nil mounts the bare action, for doc rendering only.
	Wrap func(verb.Spec) cli.ActionFunc

	// Providers registers the value resolvers a guardfile `env` source names;
	// umbra merges its built-ins (env, file, literal). Resolved at exec time.
	Providers map[string]valuesource.Provider

	// EmbeddedFiles maps each build-time `embed` source to the absolute runtime
	// path materialized by umbra. Missing or relative entries fail closed.
	EmbeddedFiles map[string]string

	// Run fires the command. nil execs for real.
	Run Runner

	// RunCapture fires an action step and captures its output (the streamed Run
	// seam cannot feed step data-flow). nil execs for real. Injected for tests.
	RunCapture CaptureRunner

	// Host resolves `shell <cmd>` selectors for wrap-level guards. nil execs for
	// real (cli/exec). Injected for tests.
	Host HostResolver
}

// Build assembles the guarded command tree and returns the Guardfile group's
// leaf command (e.g. `git`). Fails closed: a malformed grant is an error.
func Build(cfg Config) (*cli.Command, error) {
	gf := cfg.Guardfile
	if gf == nil {
		return nil, fmt.Errorf("execverb: Config.Guardfile is nil")
	}
	var err error
	gf, err = resolveEmbeddedFiles(gf, cfg.EmbeddedFiles)
	if err != nil {
		return nil, err
	}
	wrap, run, host := cfg.defaults()
	if gf.Replace {
		// A replacement wins PATH under the wrapped tool's own name, so a bare
		// name handed to exec resolves back to this process. See realbin.go.
		run = occludeRunner(run)
	}
	providers := valuesource.Merge(cfg.Providers)
	root := &cli.Command{
		Name:  gf.Group[len(gf.Group)-1],
		Usage: fmt.Sprintf("guarded %s verbs (exec dialect)", strings.Join(gf.Group, " ")),
	}
	if err := validateRefusals(gf); err != nil {
		return nil, err
	}
	if len(gf.Allow) > 0 {
		return buildAllow(root, gf, wrap, run, host, providers)
	}
	if done, out, err := mountGrants(root, gf, wrap, run, host, providers); done || err != nil {
		return out, err
	}
	if err := mountWithheld(root, gf, wrap); err != nil {
		return nil, err
	}
	mountNeverRules(root, gf, wrap)
	capture := cfg.RunCapture
	if capture == nil {
		capture = realCapture
	}
	if gf.Replace {
		capture = occludeCapture(capture)
	}
	if err := mountActions(root, gf, wrap, capture, host, providers); err != nil {
		return nil, err
	}
	return root, nil
}

// defaults fills the three injectable seams a caller may leave nil: the verb
// wrapper, the process runner, and the host resolver.
func (cfg Config) defaults() (func(verb.Spec) cli.ActionFunc, Runner, HostResolver) {
	wrap := cfg.Wrap
	if wrap == nil {
		wrap = func(s verb.Spec) cli.ActionFunc { return s.Action }
	}
	run := cfg.Run
	if run == nil {
		run = realRunner
	}
	host := cfg.Host
	if host == nil {
		host = realHost
	}
	return wrap, run, host
}

// resolveEmbeddedFiles clones only the grant layer and resolves symbolic embed
// slots for execution, preserving the source guardfile for help and describe.
func resolveEmbeddedFiles(gf *Guardfile, files map[string]string) (*Guardfile, error) {
	if len(gf.EmbedPaths()) == 0 {
		return gf, nil
	}
	resolved := *gf
	resolved.Grants = append([]Grant(nil), gf.Grants...)
	for i := range resolved.Grants {
		grant := &resolved.Grants[i]
		if len(grant.EmbeddedArgs) == 0 {
			continue
		}
		grant.runtimeArgv = append([]string(nil), grant.Argv...)
		for _, embedded := range grant.EmbeddedArgs {
			if embedded.Index < 0 || embedded.Index >= len(grant.runtimeArgv) {
				return nil, fmt.Errorf("execverb: grant %q: embedded file %q has invalid argv index %d (fail-closed)", grant.subcommandLabel(), embedded.Source, embedded.Index)
			}
			resolvedPath, ok := files[embedded.Source]
			if !ok || resolvedPath == "" {
				return nil, fmt.Errorf("execverb: grant %q: embedded file %q was not materialized (fail-closed)", grant.subcommandLabel(), embedded.Source)
			}
			if !filepath.IsAbs(resolvedPath) {
				return nil, fmt.Errorf("execverb: grant %q: embedded file %q resolved to non-absolute path %q (fail-closed)", grant.subcommandLabel(), embedded.Source, resolvedPath)
			}
			grant.runtimeArgv[embedded.Index] = resolvedPath
		}
	}
	return &resolved, nil
}

// mountGrants mounts every named grant; a wildcard funnel takes over the group
// (done=true) and cannot coexist with actions.
func mountGrants(root *cli.Command, gf *Guardfile, wrap func(verb.Spec) cli.ActionFunc, run Runner, host HostResolver, providers map[string]valuesource.Provider) (bool, *cli.Command, error) {
	for _, g := range gf.Grants {
		if g.Wildcard {
			if len(gf.Grants) != 1 {
				return true, nil, fmt.Errorf("execverb: `can run *` must be the only grant (fail-closed)")
			}
			if len(gf.Actions) > 0 {
				return true, nil, fmt.Errorf("execverb: `action` needs named `can run` grants, not a wildcard funnel (fail-closed)")
			}
			out, err := mountWildcard(root, gf, g, wrap, run, host, providers)
			return true, out, err
		}
		if err := mountGrant(root, gf, g, wrap, run, host, providers); err != nil {
			return true, nil, err
		}
	}
	return false, root, nil
}

// mountWildcard turns the group itself into one open passthrough leaf; the
// grant's gates and flag policy still decide whether the call happens.
func mountWildcard(root *cli.Command, gf *Guardfile, g Grant, wrap func(verb.Spec) cli.ActionFunc, run Runner, host HostResolver, providers map[string]valuesource.Provider) (*cli.Command, error) {
	leaf, err := wildcardLeaf(root.Name, gf, g, wrap, run, host, providers)
	if err != nil {
		return nil, err
	}
	root.Usage = leaf.Usage
	root.SkipFlagParsing = true
	root.Action = leaf.Action
	return root, nil
}

// wildcardLeaf builds one open-passthrough leaf named name for grant g of gf,
// shared by the group-as-funnel (`can run *`) and inspect-list (`allow`) paths.
func wildcardLeaf(name string, gf *Guardfile, g Grant, wrap func(verb.Spec) cli.ActionFunc, run Runner, host HostResolver, providers map[string]valuesource.Provider) (*cli.Command, error) {
	gates, err := buildGates(g)
	if err != nil {
		return nil, err
	}
	return &cli.Command{
		Name:            name,
		Usage:           leafUsage(gf, g),
		SkipFlagParsing: true,
		Action: wrap(verb.Spec{
			Name: strings.Join(gf.Group, "."),
			ArgsFunc: func(c *cli.Command) (map[string]string, []string) {
				return nil, c.Args().Slice()
			},
			Action: actionFor(gf, g, gates, run, host, providers),
		}),
	}, nil
}

// buildAllow desugars an `allow` list into one open-passthrough leaf per binary,
// each a wildcard funnel under the wrap group. The inspect-list sugar: docs/execverb.md.
func buildAllow(root *cli.Command, gf *Guardfile, wrap func(verb.Spec) cli.ActionFunc, run Runner, host HostResolver, providers map[string]valuesource.Provider) (*cli.Command, error) {
	root.Usage = fmt.Sprintf("guarded %s read-only passthroughs (%d binaries)", strings.Join(gf.Group, " "), len(gf.Allow))
	for _, bin := range gf.Allow {
		member := allowMember(gf, bin)
		leaf, err := wildcardLeaf(bin, member, member.Grants[0], wrap, run, host, providers)
		if err != nil {
			return nil, err
		}
		root.Commands = append(root.Commands, leaf)
	}
	return root, nil
}

// allowMember synthesizes the per-binary open-passthrough Guardfile for one
// `allow` entry, carrying the wrap-level guards onto its single wildcard grant.
func allowMember(gf *Guardfile, bin string) *Guardfile {
	return &Guardfile{
		Group:  append(append([]string{}, gf.Group...), bin),
		Bin:    bin,
		Grants: []Grant{{Wildcard: true, Whens: append([]WhenClause{}, gf.WrapWhens...)}},
	}
}

// Mount builds the guarded group and grafts it onto root, generating the
// intermediate path groups the Guardfile names, mirroring specverb.Mount.
func Mount(root *cli.Command, cfg Config) error {
	if root == nil {
		return fmt.Errorf("execverb: Mount root is nil")
	}
	group, err := Build(cfg)
	if err != nil {
		return err
	}
	path := cfg.Guardfile.Group
	parent := root
	if len(path) > 1 {
		for _, seg := range path[1 : len(path)-1] {
			parent = findOrCreateGroup(parent, seg)
		}
	}
	parent.Commands = append(parent.Commands, group)
	return nil
}

// mountGrant places one grant's leaf at its subcommand path under root.
func mountGrant(root *cli.Command, gf *Guardfile, g Grant, wrap func(verb.Spec) cli.ActionFunc, run Runner, host HostResolver, providers map[string]valuesource.Provider) error {
	if len(g.Subcommand) == 0 {
		return fmt.Errorf("execverb: grant with empty subcommand")
	}
	parent := root
	for _, seg := range g.Subcommand[:len(g.Subcommand)-1] {
		parent = findOrCreateGroup(parent, seg)
	}
	leafName := g.Subcommand[len(g.Subcommand)-1]
	if findChild(parent, leafName) != nil {
		return fmt.Errorf("execverb: duplicate grant for subcommand %q", strings.Join(g.Subcommand, " "))
	}
	verbName := strings.Join(gf.Group, ".") + "." + strings.Join(g.Subcommand, ".")
	gates, err := buildGates(g)
	if err != nil {
		return err
	}
	parent.Commands = append(parent.Commands, &cli.Command{
		Name:            leafName,
		Usage:           leafUsage(gf, g),
		SkipFlagParsing: true, // every arg passes through to the wrapped binary
		Action: wrap(verb.Spec{
			Name: verbName,
			ArgsFunc: func(c *cli.Command) (map[string]string, []string) {
				return nil, c.Args().Slice()
			},
			Action: actionFor(gf, g, gates, run, host, providers),
		}),
	})
	return nil
}

// gateFunc is one built preflight gate: a non-nil error denies the call.
type gateFunc func(argv []string) error

// gateRegistry maps Guardfile gate names onto builders. Unknown names fail
// closed at build time, so a typo can never become a silently absent gate.
var gateRegistry = map[string]func(GateSpec) gateFunc{}

// buildGates resolves a grant's gate specs against the registry, fail-closed.
func buildGates(g Grant) ([]gateFunc, error) {
	var gates []gateFunc
	for _, gs := range g.Gates {
		build, ok := gateRegistry[gs.Name]
		if !ok {
			return nil, fmt.Errorf("execverb: grant %q: unknown gate %q (fail-closed)", g.subcommandLabel(), gs.Name)
		}
		gates = append(gates, build(gs))
	}
	return gates, nil
}

// actionFor returns the leaf action: gates, wrap-level + grant guards, flag
// policy, env resolution, then exec with the fixed, immutable argv.
func actionFor(gf *Guardfile, g Grant, gates []gateFunc, run Runner, host HostResolver, providers map[string]valuesource.Provider) cli.ActionFunc {
	return func(ctx context.Context, c *cli.Command) error {
		args, err := applyPins(c.Args().Slice(), g)
		if err != nil {
			return refused(err, "this flag is pinned by the Guardfile and cannot be overridden")
		}
		if err := checkCallPolicy(ctx, gf, g, gates, args, host); err != nil {
			return err
		}
		env, err := resolveEnv(ctx, gf, providers)
		if err != nil {
			return exitcode.New(exitcode.Internal, "internal", err, "check the env value provider address and credentials")
		}
		// After every gate: a resolved value must not be what the guards read.
		resolved, written, err := resolveFlagValues(ctx, args, g, providers)
		if err != nil {
			return exitcode.New(exitcode.Internal, "internal", err, "check the resolve-flag value source and its permissions")
		}
		defer unlinkAll(written)
		argv := append(append(append([]string{}, gf.argvPrefixFor(g)...), g.executionArgv()...), resolved...)
		if err := run(ctx, g.ExecBin(gf.Bin), argv, env); err != nil {
			return exitcode.New(exitcode.UpstreamFailed, "upstream_failed", err, "the wrapped command failed")
		}
		return nil
	}
}

// refused codes a Guardfile refusal as PolicyDenied, the one code audit logs as
// reject. An unknown verb stays UserError: that binary will not guess which.
func refused(err error, hint string) error {
	return exitcode.New(exitcode.PolicyDenied, "policy_denied", err, hint)
}

// checkCallPolicy runs every refusal a call must survive, in order: the gates,
// the wrap-level host guards, the grant's own argv guards, flag policy, sealed.
func checkCallPolicy(ctx context.Context, gf *Guardfile, g Grant, gates []gateFunc, args []string, host HostResolver) error {
	if err := checkNeverRules(gf, g, args); err != nil {
		return err
	}
	for _, gate := range gates {
		if err := gate(args); err != nil {
			return refused(err, "this call is refused by a Guardfile gate")
		}
	}
	// wrap-level guards (the passthrough host gate) apply to every leaf, then
	// the grant's own argv guards.
	if err := checkWhens(ctx, gf.Whens, g, args, host); err != nil {
		return refused(err, "this call is refused by a Guardfile guard")
	}
	if err := checkWhens(ctx, g.Whens, g, args, host); err != nil {
		return refused(err, "this call is refused by a Guardfile guard")
	}
	if err := checkFlagPolicy(args, g); err != nil {
		return refused(err, "this flag is refused by the Guardfile policy")
	}
	if g.Sealed && len(args) > 0 {
		err := fmt.Errorf("`%s` is sealed: it forwards its pinned command exactly and accepts no trailing arguments", g.subcommandLabel())
		return refused(err, "this call is refused by the Guardfile: sealed verbs take no caller arguments")
	}
	return nil
}

// resolveEnv reads each env injection through its provider into `NAME=VALUE`
// overrides. Fails closed: a missing provider or error aborts before any exec.
func resolveEnv(ctx context.Context, gf *Guardfile, providers map[string]valuesource.Provider) ([]string, error) {
	if len(gf.Env) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(gf.Env))
	for _, e := range gf.Env {
		v, err := valuesource.Resolve(ctx, providers, e.Provider, e.Address)
		if err != nil {
			return nil, fmt.Errorf("resolve env %s from %s %s: %w", e.Name, e.Provider, e.Address, err)
		}
		out = append(out, e.Name+"="+v)
	}
	return out, nil
}

// checkFlagPolicy enforces the grant's flag rules over the caller args:
// strict allowlist when AllowFlags is set, otherwise default-allow minus denials.
func checkFlagPolicy(args []string, g Grant) error {
	allow := map[string]bool{}
	for _, f := range g.AllowFlags {
		allow[f] = true
	}
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		if d, ok := deniedBy(name, g.DenyFlags); ok {
			if d == name {
				return fmt.Errorf("flag %q is denied for `%s`", name, strings.Join(g.Subcommand, " "))
			}
			return fmt.Errorf("flag %q is denied for `%s`: it stands for %q", name, strings.Join(g.Subcommand, " "), d)
		}
		if len(allow) > 0 && !allow[name] {
			return fmt.Errorf("flag %q is not in the allowlist for `%s`", name, strings.Join(g.Subcommand, " "))
		}
	}
	return nil
}

// checkWhens enforces a set of when/deny-when (or wrap-level never/only pass)
// guards; the first to refuse stops the call before any exec.
func checkWhens(ctx context.Context, whens []WhenClause, g Grant, args []string, host HostResolver) error {
	for _, wc := range whens {
		if err := evalWhen(ctx, wc, g, args, host); err != nil {
			return err
		}
	}
	return nil
}

// evalWhen applies a guard's match rule: only/when refuses on no match, never/
// deny-when on a match. Selector is argv or a `shell <cmd>` fact. See docs.
func evalWhen(ctx context.Context, wc WhenClause, g Grant, args []string, host HostResolver) error {
	values, err := selectorValues(ctx, wc, g, args, host)
	if err != nil {
		return err // a shell source that fails to resolve fails the guard closed
	}
	val, pat, matched := firstMatch(values, wc.Patterns)
	if wc.Deny {
		if matched {
			return fmt.Errorf("%s denied: %s %q matched %q", whenLabel(wc, g), selectorName(wc), val, pat)
		}
		return nil
	}
	if !matched {
		return fmt.Errorf("%s denied: %s did not match any allowed pattern %v (fail-closed)", whenLabel(wc, g), selectorName(wc), wc.Patterns)
	}
	return nil
}

// selectorValues resolves a guard's value(s): an argv slot for an argv selector,
// or the trimmed stdout of a `shell <cmd>` source resolved once via host.
func selectorValues(ctx context.Context, wc WhenClause, g Grant, args []string, host HostResolver) ([]string, error) {
	if len(wc.SourceCmd) == 0 {
		return resolveSelector(wc.Selector, args, g.ValueFlags...), nil
	}
	out, err := host(ctx, wc.SourceCmd)
	if err != nil {
		return nil, fmt.Errorf("resolving `shell %s` failed", strings.Join(wc.SourceCmd, " "))
	}
	return []string{out}, nil
}

// selectorName renders a guard's selector for an error: the shell source for an
// ambient guard, else the argv selector slot.
func selectorName(wc WhenClause) string {
	if len(wc.SourceCmd) > 0 {
		return "shell " + strings.Join(wc.SourceCmd, " ")
	}
	return wc.Selector
}

// whenLabel renders the subject of a guard denial: the host gate for an ambient
// (`shell <cmd>`) guard, else the grant's subcommand path.
func whenLabel(wc WhenClause, g Grant) string {
	if len(wc.SourceCmd) > 0 {
		return "host gate"
	}
	return "`" + g.subcommandLabel() + "`"
}

// firstMatch returns the first (value, pattern) pair where a selector value
// matches a glob, case-insensitively, with globMatch's `*` semantics.
func firstMatch(values, patterns []string) (val, pat string, ok bool) {
	for _, v := range values {
		for _, p := range patterns {
			if globMatch(strings.ToLower(p), strings.ToLower(v)) {
				return v, p, true
			}
		}
	}
	return "", "", false
}

// resolveSelector reads the argv slot a selector names: `any-arg`, `argN`, or
// the `--<selector>` flag value. Selector forms: docs/execverb.md.
func resolveSelector(sel string, args []string, valueFlags ...string) []string {
	switch {
	case sel == "any-arg":
		return positionals(args, valueFlags...)
	case strings.HasPrefix(sel, "arg") && isAllDigits(sel[len("arg"):]):
		idx, _ := strconv.Atoi(sel[len("arg"):])
		pos := positionals(args, valueFlags...)
		if idx >= 0 && idx < len(pos) {
			return []string{pos[idx]}
		}
		return nil
	default:
		if v, ok := flagValue(args, "--"+sel); ok {
			return []string{v}
		}
		return nil
	}
}

// isAllDigits reports whether s is one or more ASCII digits (the `argN` index).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// flagValue returns flag's value in args (`--flag value` or `--flag=value`).
// The bool reports presence; a valueless `--flag` is present, empty.
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
		if strings.HasPrefix(a, flag+"=") {
			return a[len(flag)+1:], true
		}
	}
	return "", false
}

// leafUsage renders the one-line help: the real invocation (ExecArgv reflects
// any `argv` override, so it shows what runs) plus the policy.
func leafUsage(gf *Guardfile, g Grant) string {
	invocation := append(append([]string{}, gf.argvPrefixFor(g)...), g.ExecArgv()...)
	u := fmt.Sprintf("exec: %s %s", g.ExecBin(gf.Bin), strings.TrimSpace(strings.Join(invocation, " ")))
	if g.Wildcard {
		u += " <args...> (open passthrough)"
	}
	if g.Describe != "" {
		u += " - " + g.Describe
	}
	return u
}

// realRunner execs bin with inherited stdio, the production Runner. env layers
// the resolved `NAME=VALUE` overrides on top of the inherited environment.
func realRunner(ctx context.Context, bin string, argv, env []string) error {
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.Run()
}

// realHost resolves a `shell <cmd>` selector by execing the command directly
// (no shell interpretation) and returning its trimmed stdout. The real resolver.
func realHost(ctx context.Context, cmd []string) (string, error) {
	if len(cmd) == 0 {
		return "", fmt.Errorf("empty shell selector source")
	}
	out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// findChild returns parent's child named name, nil when absent.
func findChild(parent *cli.Command, name string) *cli.Command {
	for _, c := range parent.Commands {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// findOrCreateGroup returns parent's child named name, creating an empty group
// for an intermediate path segment so it is mounted once.
func findOrCreateGroup(parent *cli.Command, name string) *cli.Command {
	if c := findChild(parent, name); c != nil {
		return c
	}
	g := &cli.Command{Name: name, Usage: name + " operations"}
	parent.Commands = append(parent.Commands, g)
	return g
}
