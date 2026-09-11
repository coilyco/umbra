// Package codegen renders a complete consumer `main.go` from a KDL Guardfile,
// so a consumer never hand-writes the specverb wiring. See docs/specverb.md.
package codegen

import (
	"bytes"
	"fmt"
	"go/format"
	"net/url"
	"strings"
	"text/template"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/guardfile"
)

// Transport is the dialect one merged member speaks: spec (HTTP/specverb),
// exec (wrapped binary/execverb), or mcp (upstream MCP server/mcpverb).
const (
	TransportSpec = "spec"
	TransportExec = "exec"
	TransportMCP  = "mcp"
)

// Params is the per-consumer data the template binds to, derived from the
// Guardfile. The spec-lock fields are empty for an exec member. See specverb.md.
type Params struct {
	Transport     string // TransportSpec | TransportExec
	Binary        string // binary name, e.g. ward (Guardfile group[0])
	GuardfileName string // embedded Guardfile filename, e.g. forgejo.guardfile.kdl
	SpecLockName  string // committed gzip lock filename, e.g. forgejo.swagger.lock.json.gz
	SpecURL       string // upstream Swagger URL; the `umbra lock` source and bootstrap fallback
	SpecEnvVar    string // env var overriding the embedded lock, e.g. WARD_KDL_SPEC

	// Providers are the value-source provider names this member's guardfile uses,
	// so the codegen wires exactly the resolvers in play. Empty for an exec member.
	Providers []string

	// ProviderDecls are the member's `provider <name> { exec ... }` declarations,
	// the consumer-supplied resolvers for any non-builtin provider it names.
	ProviderDecls []guardfile.ProviderDecl

	// EmbeddedFiles are fixed build-time sources used as typed argv by an exec
	// member. Empty for spec members.
	EmbeddedFiles []EmbeddedFile

	// Replace marks an exec member that declares `replace`: the generated binary
	// is the wrapped tool rather than a verb group under a driver.
	Replace bool

	// Occluded is the binary name a replacement is installed under. Empty unless
	// Replace is set. See docs/execverb-replacement.md.
	Occluded string
}

// EmbeddedFile maps the guardfile-relative source key used by execverb to the
// project-relative artifact name embedded in generated Go.
type EmbeddedFile struct {
	Source string
	Name   string
}

// Plan derives the per-consumer Params from gf without rendering, the shared
// derivation behind both Render and the driver. guardfileName feeds the embed.
func Plan(gf *guardfile.Guardfile, guardfileName string) (Params, error) {
	if gf == nil || len(gf.Group) == 0 {
		return Params{}, fmt.Errorf("codegen: Guardfile has no command group")
	}
	// A base-url-from-value member has no committed host to derive a fetch URL from.
	// Its vendored spec is read locally at lock, so an empty SpecURL is correct.
	var specURL string
	switch {
	case gf.BaseURL != "":
		var err error
		if specURL, err = deriveSpecURL(gf.BaseURL); err != nil {
			return Params{}, err
		}
	case !gf.BaseURLValue.IsZero():
		specURL = ""
	default:
		return Params{}, fmt.Errorf("codegen: guardfile %q needs a `base-url` or `base-url { value ... }`", guardfileName)
	}
	binary := gf.Group[0]
	return Params{
		Transport:     TransportSpec,
		Binary:        binary,
		GuardfileName: guardfileName,
		SpecLockName:  deriveLockName(gf.Spec),
		SpecURL:       specURL,
		// Keyed on the full wrap group, not the binary, so two specs merged
		// into one binary get distinct overrides (see docs/umbra-cli.md).
		SpecEnvVar:    strings.ToUpper(strings.ReplaceAll(strings.Join(gf.Group, "_"), "-", "_")) + "_SPEC",
		Providers:     gf.Providers(),
		ProviderDecls: gf.ProviderDecls,
	}, nil
}

// PlanMCP derives an mcp member's Params from its wrap group and the provider
// names its transport uses. No SpecURL: `mcp <transport>` already says where.
func PlanMCP(group, providers []string, guardfileName string, decls []guardfile.ProviderDecl) (Params, error) {
	if len(group) == 0 {
		return Params{}, fmt.Errorf("codegen: mcp Guardfile has no command group")
	}
	return Params{
		Transport:     TransportMCP,
		Binary:        group[0],
		GuardfileName: guardfileName,
		SpecLockName:  group[len(group)-1] + ".tools.lock.json.gz",
		Providers:     providers,
		ProviderDecls: decls,
	}, nil
}

// PlanExec derives an exec member's Params from its wrap group and the provider
// names its env injections use; no upstream spec. See umbra-cli.md.
func PlanExec(group, providers []string, guardfileName string, decls []guardfile.ProviderDecl) (Params, error) {
	if len(group) == 0 {
		return Params{}, fmt.Errorf("codegen: exec Guardfile has no command group")
	}
	return Params{
		Transport:     TransportExec,
		Binary:        group[0],
		GuardfileName: guardfileName,
		Providers:     providers,
		ProviderDecls: decls,
	}, nil
}

// SetParams binds the merged-binary template: one shared Binary and N mounts.
// HasSpec/HasExec/HasMCP gate the per-transport imports the template emits.
type SetParams struct {
	Binary    string
	Mounts    []Params
	HasSpec   bool
	HasExec   bool
	HasMCP    bool
	HasEmbeds bool
	// ExecProviders are the consumer-declared resolvers actually named by some
	// member, deduped. umbra itself ships none. See docs/value-providers.md.
	ExecProviders []guardfile.ProviderDecl

	// Replace roots the binary at the wrapped tool, so a grant is a top-level
	// verb. Only ever set with exactly one mount.
	Replace bool

	// Occluded is the tool name the replacement stands in front of.
	Occluded string
}

// PlanSet derives the merged params for guardfiles that share a binary name. It
// fails closed on an empty set or a Group[0] disagreement (one binary per merge).
func PlanSet(gfs []*guardfile.Guardfile, names []string) (SetParams, error) {
	if len(gfs) != len(names) {
		return SetParams{}, fmt.Errorf("codegen: %d guardfiles but %d names", len(gfs), len(names))
	}
	if len(gfs) == 0 {
		return SetParams{}, fmt.Errorf("codegen: no guardfiles to plan")
	}
	var sp SetParams
	for i, gf := range gfs {
		p, err := Plan(gf, names[i])
		if err != nil {
			return SetParams{}, err
		}
		switch {
		case sp.Binary == "":
			sp.Binary = p.Binary
		case sp.Binary != p.Binary:
			return SetParams{}, fmt.Errorf("codegen: guardfiles disagree on binary name (%q vs %q); a merged build needs one wrap binary", sp.Binary, p.Binary)
		}
		if p.Transport == TransportExec {
			sp.HasExec = true
		} else {
			sp.HasSpec = true
		}
		sp.Mounts = append(sp.Mounts, p)
	}
	return sp, nil
}

// Render emits a gofmt'd `package main` wiring specverb.Mount for gf, the
// single-guardfile convenience over RenderSet.
func Render(gf *guardfile.Guardfile, guardfileName string) ([]byte, error) {
	return RenderSet([]*guardfile.Guardfile{gf}, []string{guardfileName})
}

// RenderSet emits a gofmt'd `package main` mounting every spec guardfile onto
// one binary. names are the embed filenames, parallel to gfs. Spec-only.
func RenderSet(gfs []*guardfile.Guardfile, names []string) ([]byte, error) {
	sp, err := PlanSet(gfs, names)
	if err != nil {
		return nil, err
	}
	return RenderParams(sp)
}

// RenderParams emits a gofmt'd `package main` from pre-planned SetParams,
// mixing spec and exec members onto the one shared binary. The driver's entry.
func RenderParams(sp SetParams) ([]byte, error) {
	if len(sp.Mounts) == 0 {
		return nil, fmt.Errorf("codegen: no mounts to render")
	}
	if err := validateReplacement(sp); err != nil {
		return nil, err
	}
	// Derive which consumer-side resolvers to wire from the members' providers, so
	// a hand-assembled SetParams (the driver) and a planned one agree.
	seenProv := map[string]bool{}
	for _, m := range sp.Mounts {
		if len(m.EmbeddedFiles) > 0 {
			sp.HasEmbeds = true
		}
		for _, prov := range m.Providers {
			decl, ok := declByName(sp.Mounts, prov)
			if !ok || seenProv[prov] {
				continue
			}
			seenProv[prov] = true
			sp.ExecProviders = append(sp.ExecProviders, decl)
		}
	}
	var buf bytes.Buffer
	if err := mainTemplate.Execute(&buf, sp); err != nil {
		return nil, fmt.Errorf("codegen: execute template: %w", err)
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("codegen: gofmt generated source: %w", err)
	}
	return out, nil
}

// validateReplacement holds the one-tool rule: a merged member would mount
// verbs as if the occluded tool offered them.
func validateReplacement(sp SetParams) error {
	if !sp.Replace {
		for _, m := range sp.Mounts {
			if m.Replace {
				return fmt.Errorf("codegen: member %q declares `replace` but the binary was not planned as a replacement", m.GuardfileName)
			}
		}
		return nil
	}
	if len(sp.Mounts) != 1 {
		return fmt.Errorf("codegen: a replacement binary takes exactly one member, got %d: %q stands in for a tool, and a merged member would mount verbs that tool does not have", len(sp.Mounts), sp.Occluded)
	}
	if !sp.Mounts[0].Replace {
		return fmt.Errorf("codegen: binary %q is planned as a replacement but its member declares no `replace`", sp.Occluded)
	}
	if sp.Occluded == "" {
		return fmt.Errorf("codegen: a replacement needs the name it is installed under (fail-closed)")
	}
	return nil
}

// deriveLockName maps the Guardfile spec filename to the committed gzip lock
// name. The encoded payload expands to pruned JSON. See umbra-cli.md.
func deriveLockName(spec string) string {
	logicalSpec := strings.TrimSuffix(spec, ".gz")
	if strings.HasSuffix(logicalSpec, ".v1.json") {
		return strings.TrimSuffix(logicalSpec, ".v1.json") + ".lock.json.gz"
	}
	for _, sfx := range []string{".openapi.json", ".openapi.yaml", ".openapi.yml"} {
		if strings.HasSuffix(logicalSpec, sfx) {
			return strings.TrimSuffix(logicalSpec, sfx) + ".openapi.lock.json.gz"
		}
	}
	return logicalSpec + ".lock.gz"
}

// deriveSpecURL turns the Guardfile base-url into the Swagger fetch URL: the
// host root's /swagger.v1.json (Forgejo's convention), scheme defaulted to https.
func deriveSpecURL(baseURL string) (string, error) {
	if baseURL == "" {
		return "", fmt.Errorf("codegen: Guardfile has no base-url to derive the spec URL from")
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("codegen: parse base-url %q: %w", baseURL, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("codegen: base-url %q has no host", baseURL)
	}
	return u.Scheme + "://" + u.Host + "/swagger.v1.json", nil
}

// mainTemplate is the consumer main.go, materialized out-of-band into the cache.
// It branches per transport; the spec-only and execverb imports are gated.
var mainTemplate = template.Must(template.New("main").Parse(`// Code generated by umbra; DO NOT EDIT.
// Merged from each consumer guardfile. Regenerate with 'umbra gen' (or 'run').
package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
{{if or (not .Replace) .ExecProviders}}	"strings"
{{end}}{{if .ExecProviders}}	"os/exec"
{{end}}{{if .HasSpec}}	"io"
	"net/http"
	"time"
{{end}}
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/audit"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/config"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/valuesource"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/cli/verb"
{{if .HasSpec}}	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/guardfile"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/specverb"
{{end}}{{if .HasExec}}	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/cli/execverb"
{{end}}{{if .HasMCP}}	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/mcpverb"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/mcpclient"
	"encoding/json"
{{end}}{{if .HasEmbeds}}	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/umbra/embedfile"
{{end}}	"github.com/urfave/cli/v3"
)

// Each member embeds its policy; a spec member also embeds its committed spec
// lock. Refresh both with 'umbra lock'.
{{range $i, $m := .Mounts}}
//go:embed {{$m.GuardfileName}}
var embeddedGuardfile{{$i}} []byte
{{if ne $m.Transport "exec"}}
//go:embed {{$m.SpecLockName}}
var embeddedSpec{{$i}} []byte
{{end}}{{range $j, $e := $m.EmbeddedFiles}}
//go:embed {{$e.Name}}
var embeddedFile{{$i}}_{{$j}} []byte
{{end}}{{end}}

// Version is stamped at build via -ldflags "-X main.Version="; "dev" when
// unstamped. Non-empty makes urfave/cli auto-register --version. See driver doc.
var Version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "{{.Binary}}:", err)
		// The code the error declares, so an orchestrator can separate a policy
		// refusal from a tool failure without parsing prose.
		os.Exit(exitcode.Of(err))
	}
}

{{if not .Replace}}// unknownVerb keeps an unrecognised subcommand out of urfave's own exit 3,
// which collides with UpstreamFailed and would report a typo as an upstream
// fault. A denied verb and a misspelt one land here alike: deny is absence,
// and this deliberately does not say which.
func unknownVerb(_ context.Context, cmd *cli.Command, name string) {
	fmt.Fprintf(os.Stderr, "{{.Binary}}: unknown verb %q under %q; run --help for the verbs this binary grants\n", name, cmd.Name)
	os.Exit(exitcode.UserError)
}

// hintPlacement sets the handler on every mounted command. urfave raises the
// parse error on the command that owns the flag, so the root alone never sees it.
func hintPlacement(cmds []*cli.Command) {
	for _, c := range cmds {
		if c.OnUsageError == nil {
			c.OnUsageError = placementHint
		}
		hintPlacement(c.Commands)
	}
}

// hintUnknownVerb sets the handler on every mounted group. urfave resolves a
// subcommand against the group that owns it, so the root alone never sees it.
func hintUnknownVerb(cmds []*cli.Command) {
	for _, c := range cmds {
		if c.CommandNotFound == nil {
			c.CommandNotFound = unknownVerb
		}
		hintUnknownVerb(c.Commands)
	}
}

// placementHint keeps an unknown-flag parse error from reading as a refusal.
// A wrapped tool's flag is rejected before the verb and passes after it.
func placementHint(_ context.Context, _ *cli.Command, err error, _ bool) error {
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		return err
	}
	return fmt.Errorf("%w. This is argument placement, not a denied capability: a flag for the wrapped tool goes after the verb, not before it", err)
}
{{end}}
{{if .Replace}}// run drives an occluded replacement: this binary is installed as {{.Occluded}}
// itself, so the guardfile's grants are top-level verbs and there is no driver
// name to type. See docs/execverb-replacement.md.
func run() error {
	w := auditWriter()
{{if .HasEmbeds}}	embeddedFiles, cleanup, err := embedfile.Materialize("{{.Occluded}}", embeddedSources())
	if err != nil {
		return err
	}
	defer func() { _ = cleanup() }()
{{end}}	gf, err := execverb.Parse(embeddedGuardfile0)
	if err != nil {
		return fmt.Errorf("parse exec guardfile: %w", err)
	}
	// The only surface that says umbra is here. A replacement cannot take a
	// flag of its own, because every flag belongs to the tool it occludes.
	if os.Getenv(execverb.IdentifyEnv) != "" {
		execverb.Identify(os.Stdout, gf, Version)
		return nil
	}
	cfg := execverb.Config{Guardfile: gf, Wrap: wrapWith(w), Providers: providerRegistry(){{if .HasEmbeds}}, EmbeddedFiles: embeddedFiles[0]{{end}}}
	app, err := execverb.BuildReplacement(cfg)
	if err != nil {
		return err
	}
	// nil unless the guardfile declares default-allow, and nil is the refusal.
	fb, err := execverb.NewFallback(cfg)
	if err != nil {
		return err
	}
	app.OnUsageError = func(ctx context.Context, _ *cli.Command, err error, _ bool) error {
		return execverb.RootFlagFallback(ctx, gf, fb, err)
	}
	execverb.InstallFallback(app, gf, fb)
	return app.Run(context.Background(), os.Args)
}
{{else}}func run() error {
	app := &cli.Command{Name: "{{.Binary}}", Usage: "guarded verbs generated by umbra", Version: Version, OnUsageError: placementHint, CommandNotFound: unknownVerb}
	w := auditWriter()
{{if .HasEmbeds}}	embeddedFiles, cleanup, err := embedfile.Materialize("{{.Binary}}", embeddedSources())
	if err != nil {
		return err
	}
	defer func() { _ = cleanup() }()
{{end}}	if err := mountOps(app, w{{if .HasEmbeds}}, embeddedFiles{{end}}); err != nil {
		return err
	}
	hintPlacement(app.Commands)
	hintUnknownVerb(app.Commands)
	return app.Run(context.Background(), os.Args)
}
{{end}}

{{if not .Replace}}// mountOps mounts every merged guardfile onto app under its shared command
// path, dispatching on transport. The audit writer is built once and reused.
func mountOps(app *cli.Command, w *audit.Writer{{if .HasEmbeds}}, embeddedFiles map[int]map[string]string{{end}}) error {
	wrap := wrapWith(w)
	provs := providerRegistry()
{{range $i, $m := .Mounts}}{{if eq $m.Transport "spec"}}	if err := mountSpec(app, wrap, provs, embeddedGuardfile{{$i}}, embeddedSpec{{$i}}, "{{$m.SpecURL}}", "{{$m.SpecEnvVar}}"); err != nil {
		return err
	}
{{else if eq $m.Transport "mcp"}}	if err := mountMCP(app, wrap, provs, embeddedGuardfile{{$i}}, embeddedSpec{{$i}}); err != nil {
		return err
	}
{{else}}	if err := mountExec(app, wrap, provs, embeddedGuardfile{{$i}}{{if $.HasEmbeds}}, embeddedFiles[{{$i}}]{{end}}); err != nil {
		return err
	}
{{end}}{{end}}	return nil
}
{{end}}{{if .HasEmbeds}}
func embeddedSources() map[int]map[string]embedfile.Source {
	return map[int]map[string]embedfile.Source{
{{range $i, $m := .Mounts}}{{if $m.EmbeddedFiles}}		{{$i}}: {
{{range $j, $e := $m.EmbeddedFiles}}			"{{$e.Source}}": {Path: "{{$e.Name}}", Data: embeddedFile{{$i}}_{{$j}}},
{{end}}		},
{{end}}{{end}}	}
}
{{end}}

// providerRegistry wires the store-backed resolvers in use; umbra merges its
// no-SDK built-ins (env, file, literal) underneath. Shared by both transports.
func providerRegistry() map[string]valuesource.Provider {
	return map[string]valuesource.Provider{
{{range .ExecProviders}}		"{{.Name}}": execProvider("{{.Name}}", []string{ {{range .Exec}}"{{.}}", {{end}}}),
{{end}}	}
}
{{if .ExecProviders}}
// execProvider runs a consumer-declared resolver, appending the address as the
// final argument. Only stdout is read, so the value never reaches argv or logs.
func execProvider(name string, argv []string) valuesource.Provider {
	return func(ctx context.Context, address string) (string, error) {
		args := append(append([]string{}, argv[1:]...), address)
		out, err := exec.CommandContext(ctx, argv[0], args...).Output()
		if err != nil {
			return "", fmt.Errorf("provider %s: %s: %w", name, argv[0], err)
		}
		v := strings.TrimSpace(string(out))
		if v == "" {
			return "", fmt.Errorf("provider %s returned no value for %q", name, address)
		}
		return v, nil
	}
}
{{end}}
{{if .HasSpec}}
// mountSpec parses one spec member's policy, resolves its spec, and mounts the
// specverb tree onto app. The value providers resolve lazily at request time.
func mountSpec(app *cli.Command, wrap func(verb.Spec) cli.ActionFunc, provs map[string]valuesource.Provider, gfBytes, specLock []byte, specURL, specEnv string) error {
	gf, err := guardfile.Parse(gfBytes)
	if err != nil {
		return fmt.Errorf("parse guardfile: %w", err)
	}
	spec, err := resolveSpec(specLock, specURL, specEnv)
	if err != nil {
		return fmt.Errorf("resolve spec: %w", err)
	}
	return specverb.Mount(app, specverb.Config{
		Guardfile: gf,
		Spec:      spec,
		Wrap:      wrap,
		Providers: provs,
	})
}

// resolveSpec prefers the override, then the embedded lock, then a live-fetch
// bootstrap used only before the first 'umbra lock'.
func resolveSpec(specLock []byte, specURL, specEnv string) ([]byte, error) {
	if path := os.Getenv(specEnv); path != "" {
		return os.ReadFile(path) //nolint:gosec // operator-supplied dev/skew override
	}
	if len(specLock) > 0 {
		return specLock, nil
	}
	fmt.Fprintf(os.Stderr, "{{.Binary}}: no embedded spec lock; fetching %s (run 'umbra lock')\n", specURL)
	return fetchSpec(specURL)
}

func fetchSpec(u string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s -> %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
{{end}}{{if .HasMCP}}
// mountMCP parses one mcp member's policy plus its committed tool lock and
// mounts the mcpverb tree onto app. Mounting is offline; the first call is not.
func mountMCP(app *cli.Command, wrap func(verb.Spec) cli.ActionFunc, provs map[string]valuesource.Provider, gfBytes, toolLock []byte) error {
	gf, err := mcpverb.Parse(gfBytes)
	if err != nil {
		return fmt.Errorf("parse mcp guardfile: %w", err)
	}
	var tools []mcpclient.Tool
	if err := json.Unmarshal(toolLock, &tools); err != nil {
		return fmt.Errorf("decode tool lock: %w", err)
	}
	return mcpverb.Mount(app, mcpverb.Config{Guardfile: gf, Tools: tools, Wrap: wrap, Providers: provs})
}
{{end}}{{if and .HasExec (not .Replace)}}
// mountExec parses one exec member's policy and mounts the execverb tree onto
// app; its env injections resolve through the shared provider registry.
func mountExec(app *cli.Command, wrap func(verb.Spec) cli.ActionFunc, provs map[string]valuesource.Provider, gfBytes []byte{{if .HasEmbeds}}, embeddedFiles map[string]string{{end}}) error {
	gf, err := execverb.Parse(gfBytes)
	if err != nil {
		return fmt.Errorf("parse exec guardfile: %w", err)
	}
	return execverb.Mount(app, execverb.Config{Guardfile: gf, Wrap: wrap, Providers: provs{{if .HasEmbeds}}, EmbeddedFiles: embeddedFiles{{end}}})
}
{{end}}
func auditWriter() *audit.Writer {
	path, err := config.DefaultAuditPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "{{.Binary}}: fatal: resolve audit path: %v\n", err)
		os.Exit(2)
	}
	w := audit.NewWriter(path)
	if err := w.Preflight(); err != nil {
		fmt.Fprintf(os.Stderr, "{{.Binary}}: fatal: %v\n", err)
		os.Exit(2)
	}
	return w
}

func wrapWith(w *audit.Writer) func(verb.Spec) cli.ActionFunc {
	return func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) }
}
`))

// declByName finds a declared provider across every member, so one member may
// declare a resolver another member names. Builtins are absent by design.
func declByName(mounts []Params, name string) (guardfile.ProviderDecl, bool) {
	for _, m := range mounts {
		for _, d := range m.ProviderDecls {
			if d.Name == name {
				return d, true
			}
		}
	}
	return guardfile.ProviderDecl{}, false
}
