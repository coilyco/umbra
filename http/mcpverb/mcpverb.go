// The mcpverb engine: builds a guarded cli tree from an mcp-dialect Guardfile
// plus the committed tool lock, one leaf per `can call` grant.
package mcpverb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/http/respfmt"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/urfave/cli/v3"
)

// Universal flags every mounted leaf carries, matching the spec dialect so an
// operator's muscle memory carries across transports.
const (
	flagDryRun = "dry-run"
	flagQuery  = "query"
	flagOutput = "output"
)

// Config is everything the engine needs to build a command tree.
type Config struct {
	// Guardfile is the parsed mcp-dialect policy.
	Guardfile *Guardfile

	// Tools is the committed lock: the upstream surface as `umbra lock` froze
	// it. Mounting reads it and never reaches the network, so a build is offline.
	Tools []mcpclient.Tool

	// Wrap adapts a verb.Spec into a guarded cli.ActionFunc (the audit + argv
	// pipeline). nil mounts the bare action, for doc rendering only.
	Wrap func(verb.Spec) cli.ActionFunc

	// Providers registers the value resolvers a `value <provider>` source names;
	// umbra merges its built-ins (env, file, literal).
	Providers map[string]opcore.Provider
}

// Descriptors resolves the guardfile against the lock into the pair a non-CLI
// consumer wants: the per-leaf descriptors and the request runtime config.
func Descriptors(cfg Config) ([]opcore.Descriptor, opcore.RuntimeConfig, error) {
	if cfg.Guardfile == nil {
		return nil, opcore.RuntimeConfig{}, fmt.Errorf("mcpverb: nil Guardfile")
	}
	byName := map[string]mcpclient.Tool{}
	names := make([]string, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		byName[t.Name] = t
		names = append(names, t.Name)
	}
	grants, err := cfg.Guardfile.Granted(names)
	if err != nil {
		return nil, opcore.RuntimeConfig{}, err
	}
	descs := make([]opcore.Descriptor, 0, len(grants))
	for _, g := range grants {
		d, derr := descriptorFor(cfg.Guardfile.Group, g, byName[g.Tool])
		if derr != nil {
			return nil, opcore.RuntimeConfig{}, derr
		}
		descs = append(descs, d)
	}
	return descs, runtimeConfig(cfg), nil
}

// descriptorFor lowers one grant plus its locked tool into a descriptor.
func descriptorFor(group []string, g Grant, tool mcpclient.Tool) (opcore.Descriptor, error) {
	fields, err := FieldsFor(tool)
	if err != nil {
		return opcore.Descriptor{}, err
	}
	if err := checkSelectors(g, fields); err != nil {
		return opcore.Descriptor{}, err
	}
	leaf := g.LeafName()
	describe := g.Describe
	if describe == "" {
		describe = tool.Description
	}
	return opcore.Descriptor{
		VerbName:    strings.Join(group, ".") + "." + leaf,
		Leaf:        leaf,
		Grant:       "can call " + g.Tool,
		Describe:    describe,
		Destructive: g.Destructive,
		FailWhen:    g.FailWhen,
		// Forwarded verbatim for a served surface, never interpreted here.
		Meta: tool.Meta,
		// Every input is a key of the tool's single argument object, so they all
		// land in the body slot. MCP has no path or query to split them across.
		BodyFlags: fields,
		MCP: &opcore.MCPCall{
			Tool:     g.Tool,
			Allow:    g.Allow,
			Deny:     g.Deny,
			PostCall: g.PostCall,
		},
	}, nil
}

// checkSelectors refuses a guard naming an argument the locked tool does not
// have.
func checkSelectors(g Grant, fields []opcore.Field) error {
	known := map[string]bool{}
	for _, f := range fields {
		known[f.Name] = true
	}
	for _, set := range []struct {
		mode  string
		rules []opcore.ProxyRule
	}{{"allow", g.Allow}, {"deny", g.Deny}} {
		for _, r := range set.rules {
			if !known[r.Field] {
				return fmt.Errorf("mcpverb: %s call %s: %s names %q, which is not an argument of that tool (have: %s)",
					g.Modal, g.Tool, set.mode, r.Field, strings.Join(fieldNames(fields), ", "))
			}
		}
	}
	return nil
}

// fieldNames lists a tool's argument names for an error message.
func fieldNames(fields []opcore.Field) []string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

// runtimeConfig carries the declared upstream and gates into the request runtime.
func runtimeConfig(cfg Config) opcore.RuntimeConfig {
	s := cfg.Guardfile.Server
	env := make([]opcore.MCPEnv, 0, len(s.Env))
	for _, e := range s.Env {
		env = append(env, opcore.MCPEnv{Name: e.Name, Value: e.Value})
	}
	return opcore.RuntimeConfig{
		Providers: cfg.Providers,
		Restrict:  cfg.Guardfile.Restrict,
		MCP: opcore.MCPUpstream{
			Kind:     s.Kind,
			Command:  s.Command,
			Argv:     s.Argv,
			Env:      env,
			URL:      s.URL,
			URLValue: s.URLValue,
			Auth:     s.Auth,
		},
	}
}

// Build assembles the guarded command tree and returns the wrap group's leaf
// command. Fails closed: a grant naming a tool absent from the lock is an error.
func Build(cfg Config) (*cli.Command, error) {
	descs, rc, err := Descriptors(cfg)
	if err != nil {
		return nil, err
	}
	rt := &runtime{Runtime: opcore.NewRuntime(rc), wrap: cfg.Wrap}
	if rt.wrap == nil {
		rt.wrap = func(s verb.Spec) cli.ActionFunc { return s.Action }
	}
	group := &cli.Command{
		Name:  cfg.Guardfile.Group[len(cfg.Guardfile.Group)-1],
		Usage: groupUsage(cfg.Guardfile),
	}
	for _, d := range descs {
		group.Commands = append(group.Commands, rt.buildLeaf(d))
	}
	return group, nil
}

// Mount builds the guarded group and grafts it onto root, generating the
// intermediate path groups the Guardfile names, mirroring specverb.Mount.
func Mount(root *cli.Command, cfg Config) error {
	if root == nil {
		return fmt.Errorf("mcpverb: Mount root is nil")
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

// findOrCreateGroup returns parent's child named name, creating an empty group
// for it when absent so an intermediate path segment mounts once.
func findOrCreateGroup(parent *cli.Command, name string) *cli.Command {
	for _, c := range parent.Commands {
		if c.Name == name {
			return c
		}
	}
	group := &cli.Command{Name: name}
	parent.Commands = append(parent.Commands, group)
	return group
}

// groupUsage names the upstream in the group's one-line help, so an operator
// reading `--help` sees which server the leaves underneath actually reach.
func groupUsage(gf *Guardfile) string {
	if gf.Description != "" {
		return gf.Description
	}
	switch gf.Server.Kind {
	case "stdio":
		return "guarded MCP tools from " + gf.Server.Command
	case "http":
		if gf.Server.URL != "" {
			return "guarded MCP tools from " + gf.Server.URL
		}
	}
	return "guarded MCP tools"
}

// runtime binds the request runtime to the audit wrapper, mirroring specverb's.
type runtime struct {
	*opcore.Runtime
	wrap func(verb.Spec) cli.ActionFunc
}

// buildLeaf renders one descriptor as a guarded command. Every tool input is a
// flag, since an MCP tool takes one argument object and nothing positional.
func (rt *runtime) buildLeaf(desc opcore.Descriptor) *cli.Command {
	flags := []cli.Flag{
		&cli.BoolFlag{Name: flagDryRun, Usage: "print the resolved tool call without firing it"},
		&cli.StringFlag{Name: flagQuery, Usage: "JMESPath projection applied to the result"},
		&cli.StringFlag{Name: flagOutput, Usage: "output format: yaml | yaml-stream | json | text | table"},
	}
	flags = append(flags, fieldFlags(desc.BodyFlags)...)

	usage := "call " + desc.MCP.Tool
	if desc.Destructive {
		usage += " (destructive)"
	}
	return &cli.Command{
		Name:        desc.Leaf,
		Usage:       usage,
		Description: desc.Describe,
		Flags:       flags,
		Action: rt.wrap(verb.Spec{
			Name:     desc.VerbName,
			ArgsFunc: argsFuncFor(desc),
			Action:   rt.actionFor(desc),
		}),
	}
}

// fieldFlags maps each tool input to its typed cli.Flag. Nothing is marked
// CLI-required: the binder enforces requiredness with the schema's own message.
func fieldFlags(fields []opcore.Field) []cli.Flag {
	var flags []cli.Flag
	for _, f := range fields {
		usage := f.Desc
		switch f.Type {
		case "boolean":
			flags = append(flags, &cli.BoolFlag{Name: f.Name, Usage: usage})
		case "integer":
			flags = append(flags, &cli.IntFlag{Name: f.Name, Usage: usage})
		case "number":
			flags = append(flags, &cli.FloatFlag{Name: f.Name, Usage: usage})
		case "array":
			switch f.Items {
			case "integer":
				flags = append(flags, &cli.IntSliceFlag{Name: f.Name, Usage: usage})
			case "number":
				flags = append(flags, &cli.FloatSliceFlag{Name: f.Name, Usage: usage})
			default:
				flags = append(flags, &cli.StringSliceFlag{Name: f.Name, Usage: usage})
			}
		case "object":
			// A nested object has no flat flag form, so it arrives as JSON and is
			// parsed at bind time rather than being silently unreachable.
			flags = append(flags, &cli.StringFlag{Name: f.Name, Usage: objectUsage(usage)})
		default:
			flags = append(flags, &cli.StringFlag{Name: f.Name, Usage: usage})
		}
	}
	return flags
}

// objectUsage says how a nested object is supplied, since the type alone does not.
func objectUsage(usage string) string {
	if usage == "" {
		return "JSON object"
	}
	return usage + " (JSON object)"
}

// argsFuncFor exposes the set string inputs to the shell-metacharacter gate.
func argsFuncFor(desc opcore.Descriptor) func(*cli.Command) (map[string]string, []string) {
	return func(c *cli.Command) (map[string]string, []string) {
		named := map[string]string{}
		for _, f := range desc.BodyFlags {
			if !c.IsSet(f.Name) {
				continue
			}
			for i, v := range stringValues(c, f) {
				if len(stringValues(c, f)) == 1 {
					named[f.Name] = v
					continue
				}
				named[fmt.Sprintf("%s[%d]", f.Name, i)] = v
			}
		}
		return named, c.Args().Slice()
	}
}

// actionFor is the generic action bound to one descriptor.
func (rt *runtime) actionFor(desc opcore.Descriptor) cli.ActionFunc {
	return func(ctx context.Context, c *cli.Command) error {
		if extra := c.Args().Slice(); len(extra) > 0 {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("%s takes no positional arguments, got %v", desc.Leaf, extra),
				"every tool input is a flag; run with --help to list them")
		}
		args, err := bindArgs(c, desc)
		if err != nil {
			return err
		}
		if err := rt.checkRestrictions(desc, args); err != nil {
			return err
		}
		if c.Bool(flagDryRun) {
			return renderDryRun(desc, args, c.String(flagOutput))
		}
		op := opcore.Operation{Desc: desc, RT: rt.Runtime}
		resp, err := op.Execute(ctx, opcore.Args{Body: args})
		if err != nil {
			return err
		}
		return render(resp, c.String(flagQuery), c.String(flagOutput))
	}
}

// bindArgs collects the set flags into the tool's argument object, coercing
// each through the shared binder so a typed schema stays typed on the wire.
func bindArgs(c *cli.Command, desc opcore.Descriptor) (map[string]any, error) {
	args := map[string]any{}
	for _, f := range desc.BodyFlags {
		if !c.IsSet(f.Name) {
			continue
		}
		v, err := bindField(c, f)
		if err != nil {
			return nil, exitcode.New(exitcode.UserError, "user_error", err,
				fmt.Sprintf("check the value passed to --%s", f.Name))
		}
		args[f.Name] = v
	}
	for _, f := range desc.BodyFlags {
		if f.Required && !c.IsSet(f.Name) {
			return nil, exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("%s requires --%s", desc.Leaf, f.Name),
				"the upstream tool's schema marks this input required")
		}
	}
	return args, nil
}

// bindField reads one set flag as the JSON value its schema type calls for.
func bindField(c *cli.Command, f opcore.Field) (any, error) {
	switch f.Type {
	case "boolean":
		return c.Bool(f.Name), nil
	case "integer":
		return c.Int(f.Name), nil
	case "number":
		return c.Float(f.Name), nil
	case "object":
		return opcore.CoerceScalar("object", c.String(f.Name))
	case "array":
		return opcore.CoerceItems(itemsOf(f), c.StringSlice(f.Name))
	default:
		return c.String(f.Name), nil
	}
}

// itemsOf names an array's element type, defaulting to string.
func itemsOf(f opcore.Field) string {
	if f.Items != "" {
		return f.Items
	}
	if f.Item != nil {
		return f.Item.Type
	}
	return "string"
}

// stringValues renders one set flag for the policy gate.
func stringValues(c *cli.Command, f opcore.Field) []string {
	switch f.Type {
	case "boolean":
		return []string{fmt.Sprintf("%t", c.Bool(f.Name))}
	case "integer":
		return []string{fmt.Sprintf("%d", c.Int(f.Name))}
	case "number":
		return []string{fmt.Sprintf("%g", c.Float(f.Name))}
	case "array":
		return c.StringSlice(f.Name)
	default:
		return []string{c.String(f.Name)}
	}
}

// checkRestrictions applies the wrap-level allowlists to the bound arguments,
// which are this dialect's scoping surface where the spec one gates a path.
func (rt *runtime) checkRestrictions(desc opcore.Descriptor, args map[string]any) error {
	if len(rt.Restrict) == 0 {
		return nil
	}
	names := make([]string, 0, len(desc.BodyFlags))
	values := make([]string, 0, len(desc.BodyFlags))
	for _, f := range desc.BodyFlags {
		raw, ok := args[f.Name]
		if !ok {
			continue
		}
		names = append(names, f.Name)
		values = append(values, fmt.Sprint(raw))
	}
	return rt.CheckRestrictions(names, values)
}

// renderDryRun prints the resolved call without firing it: the upstream tool
// and the exact arguments, which is the whole of what would be sent.
func renderDryRun(desc opcore.Descriptor, args map[string]any, output string) error {
	preview, err := json.Marshal(map[string]any{"tool": desc.MCP.Tool, "arguments": args})
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	rendered, err := respfmt.Render(preview, "", output)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	fmt.Print(string(rendered))
	return nil
}

// render writes the call's result through the shared response renderer.
func render(resp opcore.Response, query, output string) error {
	rendered, err := respfmt.Render(resp.Raw, query, output)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "the result was not projectable")
	}
	if len(rendered) == 0 {
		fmt.Println("ok: the tool returned no content")
		return nil
	}
	fmt.Print(string(rendered))
	return nil
}
