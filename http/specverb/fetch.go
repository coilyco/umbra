package specverb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/valuesource"
	"github.com/urfave/cli/v3"
)

const fetchGroup = "fetch"

type fetchDescriptor struct {
	Name       string
	Leaf       string
	VerbName   string
	Describe   string
	Method     string
	Path       string
	PathParams []string
	Output     string
	Env        []guardfile.FetchEnv
	Headers    []guardfile.FetchHeader
	Whens      []guardfile.FetchWhen
}

func resolveFetchDescriptors(gf *guardfile.Guardfile) ([]fetchDescriptor, error) {
	var out []fetchDescriptor
	seen := map[string]bool{}
	for _, f := range gf.Fetches {
		d := fetchDescriptor{
			Name:       f.Name,
			Leaf:       f.Leaf,
			VerbName:   strings.Join(gf.Group, ".") + "." + fetchGroup + "." + f.Leaf,
			Describe:   f.Describe,
			Method:     f.Method,
			Path:       f.Path,
			PathParams: opcore.PathParamsInOrder(f.Path),
			Output:     f.Output,
			Env:        f.Env,
			Headers:    f.Headers,
			Whens:      f.Whens,
		}
		if seen[d.Leaf] {
			return nil, fmt.Errorf("specverb: fetch %q: duplicate leaf %q after normalization (fail-closed)", f.Name, d.Leaf)
		}
		seen[d.Leaf] = true
		out = append(out, d)
	}
	return out, nil
}

func (rt *runtime) buildFetchGroup(descs []fetchDescriptor) *cli.Command {
	if len(descs) == 0 {
		return nil
	}
	grp := &cli.Command{Name: fetchGroup, Usage: "HTTP fetch overlays for non-Swagger endpoints"}
	for _, d := range descs {
		grp.Commands = append(grp.Commands, rt.buildFetchLeaf(d))
	}
	return grp
}

func (rt *runtime) buildFetchLeaf(desc fetchDescriptor) *cli.Command {
	flags := []cli.Flag{
		&cli.BoolFlag{Name: flagDryRun, Usage: "print the resolved fetch without firing it"},
	}
	return &cli.Command{
		Name:        desc.Leaf,
		Usage:       fetchUsage(desc),
		Description: fetchDescription(desc),
		ArgsUsage:   argsUsage(desc.PathParams),
		Flags:       flags,
		Action: rt.wrap(verb.Spec{
			Name:     desc.VerbName,
			ArgsFunc: fetchArgsFunc(desc),
			Action:   rt.runFetch(desc),
		}),
	}
}

func fetchArgsFunc(_ fetchDescriptor) func(*cli.Command) (map[string]string, []string) {
	return func(c *cli.Command) (map[string]string, []string) {
		return nil, c.Args().Slice()
	}
}

func (rt *runtime) runFetch(desc fetchDescriptor) cli.ActionFunc {
	return func(ctx context.Context, c *cli.Command) error {
		positional := c.Args().Slice()
		if len(positional) != len(desc.PathParams) {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("%s takes %d positional arg(s) %v, got %d", desc.Leaf, len(desc.PathParams), desc.PathParams, len(positional)),
				"supply exactly the path parameters this fetch names")
		}
		if err := rt.CheckRestrictions(desc.PathParams, positional); err != nil {
			return err
		}
		url, err := fetchURL(ctx, rt.Runtime, desc.Path, positional, c.Bool(flagDryRun))
		if err != nil {
			return err
		}
		headers, err := rt.resolveFetchHeaders(ctx, desc, c.Bool(flagDryRun))
		if err != nil {
			return err
		}
		if err := rt.applyFetchWhens(desc, positional); err != nil {
			return err
		}
		if c.Bool(flagDryRun) {
			return renderFetchPlan(desc, url, headers)
		}
		return rt.fireFetch(ctx, desc, url, headers)
	}
}

func fetchURL(ctx context.Context, rt *opcore.Runtime, path string, positional []string, dry bool) (string, error) {
	base, err := rt.BaseForRequest(ctx, dry)
	if err != nil {
		return "", err
	}
	filled := opcore.FillPath(path, positional)
	switch {
	case strings.HasPrefix(filled, "http://") || strings.HasPrefix(filled, "https://"):
		return filled, nil
	case base == "":
		return "", fmt.Errorf("specverb: fetch path %q needs a base-url or an absolute URL", path)
	default:
		return base + filled, nil
	}
}

func (rt *runtime) resolveFetchHeaders(ctx context.Context, desc fetchDescriptor, dry bool) (http.Header, error) {
	envVals := map[string]string{}
	if !dry {
		for _, e := range desc.Env {
			v, err := valuesource.ResolveFirst(ctx, rt.Providers, opcore.ChainSources(e.Value))
			if err != nil {
				return nil, exitcode.New(exitcode.Internal, "internal", err, "check the fetch env value source or register the provider via specverb.Config.Providers")
			}
			envVals[e.Name] = v
		}
	}
	hdrs := make(http.Header, len(desc.Headers))
	for _, h := range desc.Headers {
		v, err := expandFetchHeader(h.Value, envVals, dry)
		if err != nil {
			return nil, err
		}
		hdrs.Add(h.Name, v)
	}
	return hdrs, nil
}

func expandFetchHeader(tpl string, envVals map[string]string, dry bool) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tpl); {
		start := strings.Index(tpl[i:], "${")
		if start < 0 {
			b.WriteString(tpl[i:])
			break
		}
		start += i
		b.WriteString(tpl[i:start])
		end := strings.IndexByte(tpl[start+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("specverb: fetch header template %q has an unterminated ${...} placeholder", tpl)
		}
		end += start + 2
		name := tpl[start+2 : end]
		if name == "" {
			return "", fmt.Errorf("specverb: fetch header template %q has an empty ${...} placeholder", tpl)
		}
		if dry {
			b.WriteString(redacted)
		} else {
			v, ok := envVals[name]
			if !ok {
				return "", fmt.Errorf("specverb: fetch header template %q references undeclared env %q (fail-closed)", tpl, name)
			}
			b.WriteString(v)
		}
		i = end + 1
	}
	return b.String(), nil
}

func (rt *runtime) applyFetchWhens(desc fetchDescriptor, positional []string) error {
	for _, w := range desc.Whens {
		i, err := fetchSelectorIndex(w.Selector)
		if err != nil {
			return err
		}
		if i >= len(positional) {
			return exitcode.New(exitcode.PolicyDenied, "policy_denied",
				fmt.Errorf("fetch %q: selector %q is missing positional arg %d", desc.Name, w.Selector, i),
				"supply the positional argument the fetch guard names")
		}
		if !matchesAnyFetchGlob(positional[i], w.Globs) {
			return exitcode.New(exitcode.PolicyDenied, "policy_denied",
				fmt.Errorf("fetch %q: %q did not match %s", desc.Name, w.Selector, strings.Join(w.Globs, " or ")),
				"adjust the guard glob or the supplied argument")
		}
	}
	return nil
}

func fetchSelectorIndex(selector string) (int, error) {
	if selector == "arg0" {
		return 0, nil
	}
	if !strings.HasPrefix(selector, "arg") {
		return 0, fmt.Errorf("specverb: fetch selector %q unsupported (want argN or first input; fail-closed)", selector)
	}
	n, err := parseFetchIndex(selector[3:])
	if err != nil {
		return 0, fmt.Errorf("specverb: fetch selector %q unsupported (want argN or first input; fail-closed)", selector)
	}
	return n, nil
}

func parseFetchIndex(raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func matchesAnyFetchGlob(val string, globs []string) bool {
	val = strings.ToLower(val)
	for _, g := range globs {
		if match, _ := filepath.Match(strings.ToLower(g), val); match {
			return true
		}
	}
	return false
}

func renderFetchPlan(desc fetchDescriptor, url string, headers http.Header) error {
	var b strings.Builder
	fmt.Fprintf(&b, "fetch %s\n", desc.Name)
	fmt.Fprintf(&b, "  method: %s\n", desc.Method)
	fmt.Fprintf(&b, "  url: %s\n", url)
	if len(headers) > 0 {
		b.WriteString("  headers:\n")
		keys := make([]string, 0, len(headers))
		for k := range headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "    %s: %s\n", k, strings.Join(headers[k], ", "))
		}
	}
	if len(desc.Whens) > 0 {
		b.WriteString("  when:\n")
		for _, w := range desc.Whens {
			fmt.Fprintf(&b, "    %s matches %s\n", w.Selector, strings.Join(w.Globs, " or "))
		}
	}
	fmt.Print(b.String())
	return nil
}

func (rt *runtime) fireFetch(ctx context.Context, desc fetchDescriptor, url string, headers http.Header) error {
	req, err := http.NewRequestWithContext(ctx, desc.Method, url, nil)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	if err := rt.Authorize(ctx, req); err != nil {
		return err
	}
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	resp, err := rt.Client.Do(req)
	if err != nil {
		return exitcode.New(exitcode.UpstreamFailed, "upstream_failed", err, "the API was unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return exitcode.New(exitcode.UpstreamFailed, "upstream_failed",
			fmt.Errorf("fetch %q (%s): %s %s -> %s: %s", desc.Name, desc.Leaf, desc.Method, url, resp.Status, strings.TrimSpace(string(body))),
			"the API rejected the request")
	}
	if len(body) > 0 {
		fmt.Print(string(body))
	}
	return nil
}

func fetchUsage(desc fetchDescriptor) string {
	if desc.Describe != "" {
		return desc.Describe
	}
	if desc.Name != desc.Leaf {
		return fmt.Sprintf("HTTP fetch: %s", desc.Name)
	}
	return fmt.Sprintf("HTTP fetch: %s %s", desc.Method, desc.Path)
}

func fetchDescription(desc fetchDescriptor) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", desc.Method, desc.Path)
	fmt.Fprintf(&b, "Output: raw stdout.\n")
	if desc.Name != desc.Leaf {
		fmt.Fprintf(&b, "Label: %s.\n", desc.Name)
	}
	if desc.Describe != "" {
		fmt.Fprintf(&b, "%s\n", desc.Describe)
	}
	if len(desc.PathParams) > 0 {
		b.WriteString("\nPositional arguments:\n")
		for _, p := range desc.PathParams {
			fmt.Fprintf(&b, "  <%s>\n", p)
		}
	}
	if len(desc.Env) > 0 {
		b.WriteString("\nEnv values:\n")
		for _, e := range desc.Env {
			fmt.Fprintf(&b, "  %s <- %s\n", e.Name, e.Value.String())
		}
	}
	if len(desc.Headers) > 0 {
		b.WriteString("\nHeaders:\n")
		for _, h := range desc.Headers {
			fmt.Fprintf(&b, "  %s: %s\n", h.Name, h.Value)
		}
	}
	if len(desc.Whens) > 0 {
		b.WriteString("\nGuards:\n")
		for _, w := range desc.Whens {
			fmt.Fprintf(&b, "  %s matches %s\n", w.Selector, strings.Join(w.Globs, " or "))
		}
	}
	b.WriteString("\nUse --dry-run to print the resolved fetch without firing it.")
	return b.String()
}
