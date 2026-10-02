// Package specverb is the runtime engine of the spec-driven verb design: it
// builds a guarded cli tree from a Guardfile + spec. See docs/specverb.md.
package specverb

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/stepflow"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/urfave/cli/v3"
)

// Config is everything the engine needs to build a command tree.
type Config struct {
	// Guardfile is the parsed L2 policy: the command path, auth, and grants.
	Guardfile *guardfile.Guardfile

	// Spec is the raw Swagger 2.0 document bytes (embedded by the consumer).
	Spec []byte

	// Wrap adapts a verb.Spec into a guarded cli.ActionFunc (the audit + argv
	// pipeline). nil mounts the bare action, for doc rendering only.
	Wrap func(verb.Spec) cli.ActionFunc

	// Providers registers the value resolvers a `value <provider>` source names;
	// umbra merges its built-ins (env, file, literal). See specverb-policy.md.
	Providers map[string]Provider

	// HTTPClient fires the live request. nil uses http.DefaultClient.
	HTTPClient *http.Client

	// BaseURL overrides the Guardfile base-url. "" uses the Guardfile value.
	BaseURL string

	// stepRun overrides the transport firing an action's steps; nil uses the HTTP
	// runtime. A test seam for a fake step runner.
	stepRun stepflow.Runner
}

// opDescriptor is the per-operation payload; it now lives urfave/cli-free in
// opcore, aliased here so every reference stays mechanical.
type opDescriptor = opcore.Descriptor

// fieldFlag is one spec input promoted to a typed CLI flag; moved to opcore
// alongside the descriptor and aliased here.
type fieldFlag = opcore.Field

// Build assembles the guarded command tree and returns the Guardfile group's
// leaf command (e.g. `forgejo`). Fails closed: an unresolvable grant is an error.
func Build(cfg Config) (*cli.Command, error) {
	if cfg.Guardfile == nil {
		return nil, fmt.Errorf("specverb: Config.Guardfile is nil")
	}
	gf := cfg.Guardfile
	if len(gf.Group) == 0 {
		return nil, fmt.Errorf("specverb: Guardfile has no command group")
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = gf.BaseURL
	}

	rt := newRuntime(cfg, gf, baseURL)

	fetchDescs, err := resolveFetchDescriptors(gf)
	if err != nil {
		return nil, err
	}
	hasSpecDriven := len(gf.Grants) > 0 || len(gf.Actions) > 0 || len(gf.Restrict) > 0
	if hasSpecDriven {
		return buildSpecDriven(rt, cfg, gf, fetchDescs)
	}
	return buildFetchOnly(rt, gf, fetchDescs)
}

func buildSpecDriven(rt *runtime, cfg Config, gf *guardfile.Guardfile, fetchDescs []fetchDescriptor) (*cli.Command, error) {
	spec, err := parseSwagger(cfg.Spec)
	if err != nil {
		return nil, err
	}
	gf, err = expandWildcards(spec, gf)
	if err != nil {
		return nil, err
	}
	descs, err := resolveDescriptors(spec, gf)
	if err != nil {
		return nil, err
	}
	if len(descs) == 0 {
		return nil, fmt.Errorf("specverb: Guardfile mounted no verbs (no `can` grants resolved)")
	}
	actionDescs, err := resolveActions(spec, gf, grantedGrants(gf))
	if err != nil {
		return nil, err
	}
	mountActions, namedActions := splitMountActions(actionDescs)
	descs = suppressShadowed(descs, mountActions)
	groupCmds := rt.buildGroups(descs, denyDescriptors(gf))
	groupCmds = rt.mountShadowLeaves(groupCmds, mountActions)
	if ag := rt.buildActionGroup(namedActions); ag != nil {
		groupCmds = append(groupCmds, ag)
	}
	if fg := rt.buildFetchGroup(fetchDescs); fg != nil {
		groupCmds = append(groupCmds, fg)
	}
	surface := buildSurface(gf, baseURLDisplay(gf, rt.BaseURL), descs, actionDescs, fetchDescs)
	groupCmds = append(groupCmds, rt.buildDescribeLeaf(gf, surface))
	return &cli.Command{
		Name:     gf.Group[len(gf.Group)-1],
		Usage:    fmt.Sprintf("spec-driven %s verbs", strings.Join(gf.Group, " ")),
		Commands: groupCmds,
	}, nil
}

func buildFetchOnly(rt *runtime, gf *guardfile.Guardfile, fetchDescs []fetchDescriptor) (*cli.Command, error) {
	if len(fetchDescs) == 0 {
		return nil, fmt.Errorf("specverb: Guardfile mounted no verbs (no `can` grants resolved)")
	}
	groupCmds := []*cli.Command{}
	if fg := rt.buildFetchGroup(fetchDescs); fg != nil {
		groupCmds = append(groupCmds, fg)
	}
	surface := buildSurface(gf, baseURLDisplay(gf, rt.BaseURL), nil, nil, fetchDescs)
	groupCmds = append(groupCmds, rt.buildDescribeLeaf(gf, surface))
	return &cli.Command{
		Name:     gf.Group[len(gf.Group)-1],
		Usage:    fmt.Sprintf("%s verbs", strings.Join(gf.Group, " ")),
		Commands: groupCmds,
	}, nil
}

// newRuntime assembles the per-tree runtime: the urfave/cli-free opcore core
// plus the CLI-layer wrap pipeline and step transport (the runtime itself).
func newRuntime(cfg Config, gf *guardfile.Guardfile, baseURL string) *runtime {
	rt := &runtime{
		Runtime: opcore.NewRuntime(opcore.RuntimeConfig{
			BaseURL:      baseURL,
			Auth:         gf.Auth,
			Providers:    mergeProviders(cfg.Providers),
			Client:       cfg.HTTPClient,
			Restrict:     gf.Restrict,
			AllowMeta:    gf.AllowMeta,
			BaseURLValue: gf.BaseURLValue,
		}),
		wrap:    cfg.Wrap,
		stepRun: cfg.stepRun,
	}
	if rt.stepRun == nil {
		rt.stepRun = rt // the HTTP transport is the default step runner
	}
	if rt.wrap == nil {
		// identity: bare action, no audit pipeline. Doc-render path only.
		rt.wrap = func(s verb.Spec) cli.ActionFunc { return s.Action }
	}
	return rt
}

// splitMountActions partitions resolved actions into those that shadow a leaf
// path (`action <verb> <resource>`) and the rest (named `action` leaves).
func splitMountActions(actions []actionDescriptor) (mount, named []actionDescriptor) {
	for _, a := range actions {
		if a.isMount() {
			mount = append(mount, a)
		} else {
			named = append(named, a)
		}
	}
	return mount, named
}

// suppressShadowed drops every generated descriptor a mount action shadows, so the
// action - not the bare leaf - mounts at that path. The grant is untouched.
func suppressShadowed(descs []opDescriptor, mount []actionDescriptor) []opDescriptor {
	if len(mount) == 0 {
		return descs
	}
	shadowed := map[grantKey]bool{}
	for _, a := range mount {
		shadowed[grantKey{Verb: a.MountVerb, Resource: a.MountResource}] = true
	}
	out := descs[:0:0]
	for _, d := range descs {
		if shadowed[grantKey{Verb: d.Leaf, Resource: d.Group}] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// mountShadowLeaves grafts each mount action into its resource group as that
// group's verb leaf, creating the group when no generated leaf seeded it.
func (rt *runtime) mountShadowLeaves(groupCmds []*cli.Command, mount []actionDescriptor) []*cli.Command {
	for _, ad := range mount {
		grp := findCommand(groupCmds, ad.MountResource)
		if grp == nil {
			grp = &cli.Command{Name: ad.MountResource, Usage: fmt.Sprintf("%s operations", ad.MountResource)}
			groupCmds = append(groupCmds, grp)
		}
		leaf := rt.buildActionLeaf(ad)
		leaf.Name = ad.MountVerb // mount at the verb, not the synthesized action name
		grp.Commands = append(grp.Commands, leaf)
	}
	return groupCmds
}

// findCommand returns the command named name among cmds, or nil.
func findCommand(cmds []*cli.Command, name string) *cli.Command {
	for _, c := range cmds {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Mount builds the guarded group and grafts it onto root, generating the
// intermediate path groups the Guardfile names. See docs/specverb.md.
func Mount(root *cli.Command, cfg Config) error {
	if root == nil {
		return fmt.Errorf("specverb: Mount root is nil")
	}
	group, err := Build(cfg)
	if err != nil {
		return err
	}
	// Group [ward, ops, forgejo]: index 0 is root, the last is group.Name, the
	// middle is the path to find-or-create under root.
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
// command for it when absent so an intermediate path segment is mounted once.
func findOrCreateGroup(parent *cli.Command, name string) *cli.Command {
	for _, c := range parent.Commands {
		if c.Name == name {
			return c
		}
	}
	g := &cli.Command{Name: name, Usage: name + " operations"}
	parent.Commands = append(parent.Commands, g)
	return g
}

// baseURLDisplay is the base-url describe shows: the provider+address for a
// value-resolved host (stays offline), else the resolved static base.
func baseURLDisplay(gf *guardfile.Guardfile, static string) string {
	if !gf.BaseURLValue.IsZero() {
		return "(resolved from " + gf.BaseURLValue.String() + ")"
	}
	return static
}

// grantKey is the (verb-class, resource) pair an authored grant names: the CLI
// placement (Resource is the group, Verb is the leaf).
type grantKey struct {
	Verb     string
	Resource string
}

// overriddenKeys maps each (verb, resource) an `override can` re-grants, so a deny
// it lifts mounts no teaching leaf and the override's allow leaf stands instead.
func overriddenKeys(gf *guardfile.Guardfile) map[grantKey]bool {
	keys := map[grantKey]bool{}
	for _, g := range gf.Grants {
		if g.Override {
			keys[grantKey{Verb: g.Verb, Resource: g.Resource}] = true
		}
	}
	return keys
}

// grantedGrants maps each `can` grant by its (verb, resource) placement, so an
// action can recover the grant - and its op - for a leaf it polls or calls.
func grantedGrants(gf *guardfile.Guardfile) map[grantKey]guardfile.Grant {
	denied := deniedKeys(gf)
	keys := map[grantKey]guardfile.Grant{}
	for _, g := range gf.Grants {
		if g.Modal != "can" {
			continue
		}
		k := grantKey{Verb: g.Verb, Resource: g.Resource}
		if _, blocked := denied[k]; blocked && !g.Override {
			continue // a denied leaf is not pollable - unless an override crosses the deny
		}
		keys[k] = g
	}
	return keys
}

// resolveDescriptors resolves every `can` grant into a concrete descriptor; a deny
// drops a matching plain `can` but an `override` crosses it. See specverb-policy.md.
func resolveDescriptors(spec *spec, gf *guardfile.Guardfile) ([]opDescriptor, error) {
	denied := deniedKeys(gf)
	var descs []opDescriptor
	for _, g := range gf.Grants {
		if g.Modal != "can" {
			continue
		}
		if _, blocked := denied[grantKey{Verb: g.Verb, Resource: g.Resource}]; blocked && !g.Override {
			continue // a cannot/never beats a plain allow; only an override survives it
		}
		desc, err := resolveDescriptor(spec, gf.Group, g)
		if err != nil {
			return nil, err
		}
		descs = append(descs, desc)
	}
	return descs, nil
}

// buildGroups buckets the descriptors into resource-group commands in first-seen
// order. Deny leaves mount beside the allowed leaves. See docs/specverb.md.
func (rt *runtime) buildGroups(descs []opDescriptor, denies []denyDescriptor) []*cli.Command {
	groups := map[string]*cli.Command{}
	var order []string
	groupFor := func(name string) *cli.Command {
		grp, ok := groups[name]
		if !ok {
			grp = &cli.Command{Name: name, Usage: fmt.Sprintf("%s operations", name)}
			groups[name] = grp
			order = append(order, name)
		}
		return grp
	}
	for _, desc := range descs {
		grp := groupFor(desc.Group)
		grp.Commands = append(grp.Commands, rt.buildLeaf(desc))
	}
	for _, d := range denies {
		grp := groupFor(d.Group)
		grp.Commands = append(grp.Commands, rt.buildDenyLeaf(d))
	}
	out := make([]*cli.Command, 0, len(order))
	for _, name := range order {
		out = append(out, groups[name])
	}
	return out
}

// resolveDescriptor turns one grant into a concrete descriptor via resolveOp,
// failing closed (unresolvable verb+resource, or an op the spec lacks).
func resolveDescriptor(spec *spec, group []string, g guardfile.Grant) (opDescriptor, error) {
	opID, err := resolveOp(spec, g)
	if err != nil {
		return opDescriptor{}, err
	}
	method, path, op, err := spec.findOp(opID)
	if err != nil {
		return opDescriptor{}, err
	}
	bodySchema, _ := spec.bodySchema(op)
	desc := opDescriptor{
		VerbName:    strings.Join(group, ".") + "." + g.Resource + "." + g.Verb,
		Group:       g.Resource,
		Leaf:        g.Verb,
		Method:      method,
		Path:        path,
		PathParams:  pathParamsInOrder(path),
		BodyFlags:   bodyFlagsFrom(bodySchema),
		QueryFlags:  queryFlagsFrom(op),
		FormFlags:   formFlagsFrom(op),
		FixedBody:   g.FixedBody,
		Destructive: opcore.DestructiveVerb(g.Verb),
		Grant:       formatGrant(g),
		Describe:    g.Describe,
		RawResponse: rawResponseOp(op.op),
	}
	if len(desc.FixedBody) > 0 {
		// a state toggle owns its body: the spec's edit fields stay unmounted
		desc.BodyFlags = nil
	}
	if err := checkFlagCollisions(desc); err != nil {
		return opDescriptor{}, err
	}
	return desc, nil
}

// checkFlagCollisions is opcore's shared reserved-flag guard, aliased so the
// resolved source fails closed identically to the inline one.
var checkFlagCollisions = opcore.CheckFlagCollisions

// formatGrant renders the authorizing grant sentence for help and describe,
// e.g. {can, delete, repos, [created-by-me]} -> "can delete repos created-by-me".
func formatGrant(g guardfile.Grant) string {
	resource := g.Resource
	if g.Wildcard {
		resource = `"*"` // a wildcard-expanded grant reads as the verb-global rule that authorized it
	}
	var parts []string
	if g.Override {
		parts = append(parts, "override") // an escalation reads as `override can <verb> <resource>`
	}
	parts = append(parts, g.Modal, g.Verb, resource)
	parts = append(parts, g.Qualifiers...)
	return strings.Join(parts, " ")
}

// bodyFlagsFrom promotes a body schema's scalar and array-of-scalar properties
// to typed flags; required-ness is enforced at assembly, not the CLI layer.
func bodyFlagsFrom(schema *openapi3.Schema) []fieldFlag {
	if schema == nil {
		return nil
	}
	required := requiredSet(schema.Required)
	// Stable order: sortedSchemaNames sorts property names so the flag set and
	// help are deterministic across runs (Go map iteration is randomized).
	var flags []fieldFlag
	for _, name := range sortedSchemaNames(schema.Properties) {
		ref := schema.Properties[name]
		if ref == nil || ref.Value == nil {
			continue
		}
		if f, ok := fieldFlagFor(name, ref.Value, required[name]); ok {
			flags = append(flags, f)
		}
	}
	return flags
}

// fieldFlagFor lowers one body property to a flag; ok=false for shapes only
// --body-file can express (nested objects, arrays of objects).
func fieldFlagFor(name string, prop *openapi3.Schema, required bool) (fieldFlag, bool) {
	f := fieldFlag{Name: name, Required: required, Desc: prop.Description}
	t := schemaType(prop)
	switch {
	case isScalar(t):
		f.Type = t
	case t == "array" && prop.Items != nil && prop.Items.Value != nil:
		switch it := schemaType(prop.Items.Value); {
		case isScalar(it):
			f.Type = "array"
			f.Items = it
		case it == "":
			// An untyped item is a genuine union, so neither string nor integer
			// is right. See itemsAny in request.go for how it is encoded.
			f.Type = "array"
			f.Items = itemsAny
		default:
			return fieldFlag{}, false
		}
	default:
		return fieldFlag{}, false
	}
	return f, true
}

// queryFlagsFrom promotes an operation's scalar query parameters to flags.
func queryFlagsFrom(op operation) []fieldFlag {
	var flags []fieldFlag
	for _, p := range queryParamsOf(op) {
		flags = append(flags, fieldFlag{
			Name:     p.Name,
			Type:     p.Type,
			Required: p.Required,
			Desc:     p.Description,
		})
	}
	return flags
}

// formFlagsFrom promotes an operation's formData parameters to flags; a "file"
// param mounts as a path-taking string flag the action streams into multipart.
func formFlagsFrom(op operation) []fieldFlag {
	var flags []fieldFlag
	for _, p := range formParamsOf(op) {
		flags = append(flags, fieldFlag{
			Name:     p.Name,
			Type:     p.Type,
			Required: p.Required,
			Desc:     p.Description,
		})
	}
	return flags
}

// isScalar reports whether a swagger type lowers to a single CLI flag value.
func isScalar(t string) bool {
	switch t {
	case "string", "boolean", "integer", "number":
		return true
	}
	return false
}
