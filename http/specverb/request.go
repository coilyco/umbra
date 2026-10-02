// The static request machinery: one generic action assembles, previews
// (--dry-run), fires, and renders every mounted verb. See docs/specverb.md.

package specverb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/http/respfmt"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/stepflow"
	"github.com/urfave/cli/v3"
)

// redacted is what a secret header value renders as in a --dry-run preview, so
// the resolved request can be inspected without leaking the token.
const redacted = "<redacted>"

// runtime is the CLI projection of the engine: it embeds the urfave/cli-free
// opcore.Runtime and layers on the CLI-only wrap pipeline and step transport.
type runtime struct {
	*opcore.Runtime

	// wrap adapts a verb.Spec into a guarded cli.ActionFunc (the audit + argv
	// pipeline). Identity mounts the bare action, for doc rendering only.
	wrap func(verb.Spec) cli.ActionFunc

	// stepRun fires an action's steps; the runtime is the default HTTP
	// implementation; a test may inject a fake.
	stepRun stepflow.Runner
}

// universal flag names every mounted leaf carries.
const (
	flagDryRun   = "dry-run"
	flagQuery    = "query"
	flagOutput   = "output"
	flagBodyFile = "body-file"
	flagNoCache  = "no-cache"
	flagRefresh  = "refresh"
)

// buildLeaf turns one descriptor into a guarded leaf: query + body flags plus
// the universal dry-run/query/output flags, action wrapped in the verb pipeline.
func (rt *runtime) buildLeaf(desc opDescriptor) *cli.Command {
	flags := []cli.Flag{
		&cli.BoolFlag{Name: flagDryRun, Usage: "print the resolved request without firing it"},
		&cli.StringFlag{Name: flagQuery, Usage: "JMESPath projection applied to the response"},
		&cli.StringFlag{Name: flagOutput, Usage: "output format: yaml | yaml-stream | json | text | table"},
	}
	if len(desc.BodyFlags) > 0 {
		flags = append(flags, &cli.StringFlag{Name: flagBodyFile, Usage: "path to a JSON file supplying the full request body (exclusive with the body flags)"})
	}
	flags = append(flags, fieldFlagsToCLI(desc.QueryFlags)...)
	flags = append(flags, fieldFlagsToCLI(desc.BodyFlags)...)
	flags = append(flags, fieldFlagsToCLI(desc.FormFlags)...)

	usage := fmt.Sprintf("%s %s", desc.Method, desc.Path)
	if desc.Destructive {
		usage += " (destructive)"
	}
	return &cli.Command{
		Name:        desc.Leaf,
		Usage:       usage,
		Description: leafDescription(desc),
		ArgsUsage:   argsUsage(desc.PathParams),
		Flags:       flags,
		Action: rt.wrap(verb.Spec{
			Name:     desc.VerbName,
			ArgsFunc: rt.argsFuncFor(desc),
			Action:   rt.actionFor(desc),
		}),
	}
}

// fieldFlagsToCLI maps each promoted spec input to its typed cli.Flag; nothing
// is CLI-required since assembly enforces it (--body-file is a legal source).
func fieldFlagsToCLI(ff []fieldFlag) []cli.Flag {
	var flags []cli.Flag
	for _, f := range ff {
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
			case "boolean":
				// urfave/cli has no BoolSliceFlag. The action parses and validates
				// each StringSlice value before the shared opcore query validator.
				flags = append(flags, &cli.StringSliceFlag{Name: f.Name, Usage: usage})
			case "integer":
				flags = append(flags, &cli.IntSliceFlag{Name: f.Name, Usage: usage})
			case "number":
				flags = append(flags, &cli.FloatSliceFlag{Name: f.Name, Usage: usage})
			default: // string
				flags = append(flags, &cli.StringSliceFlag{Name: f.Name, Usage: usage})
			}
		default: // string
			flags = append(flags, &cli.StringFlag{Name: f.Name, Usage: usage})
		}
	}
	return flags
}

// argsUsage renders the positional path params as `<owner> <repo>`.
func argsUsage(params []string) string {
	if len(params) == 0 {
		return ""
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = "<" + p + ">"
	}
	return strings.Join(parts, " ")
}

// argsFuncFor hands the gate only the positionals: they alone reach the URL
// unescaped. opcore re-gates them in resolve; docs/specverb-request.md.
func (rt *runtime) argsFuncFor(desc opDescriptor) func(*cli.Command) (map[string]string, []string) {
	return func(c *cli.Command) (map[string]string, []string) {
		positional := c.Args().Slice()
		gated := make([]string, 0, len(positional))
		for i, v := range positional {
			if i < len(desc.PathParams) && rt.MetaAllowed(desc.PathParams[i]) {
				continue
			}
			gated = append(gated, v)
		}
		return map[string]string{}, gated
	}
}

// actionFor is the generic action bound to one descriptor.
func (rt *runtime) actionFor(desc opDescriptor) cli.ActionFunc {
	return func(ctx context.Context, c *cli.Command) error {
		positional := c.Args().Slice()
		if len(positional) != len(desc.PathParams) {
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("%s takes %d positional arg(s) %v, got %d", desc.Leaf, len(desc.PathParams), desc.PathParams, len(positional)),
				"supply exactly the path parameters this verb names")
		}
		url, err := rt.resolveCLIURL(ctx, c, desc, positional)
		if err != nil {
			return err
		}
		var body []byte
		contentType := contentTypeJSON
		var preview any
		switch {
		case len(desc.FormFlags) > 0:
			body, contentType, preview, err = assembleMultipart(c, desc.FormFlags, c.Bool(flagDryRun))
			if err != nil {
				return exitcode.New(exitcode.UserError, "user_error", err, "check the form flag values")
			}
		case len(desc.BodyMappings) > 0:
			// A mapped source mounts no CLI flag, so this surface cannot fill
			// one. Refusing beats sending a body missing its mapped keys.
			return exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("%s takes a mapped body, which this command line cannot supply", desc.Leaf),
				"call this grant through the MCP surface, which carries mapped inputs")
		case len(desc.FixedBody) > 0:
			body, err = json.Marshal(desc.FixedBody)
			if err != nil {
				return exitcode.New(exitcode.Internal, "internal", err, "")
			}
		default:
			body, err = assembleBody(c, desc.BodyFlags)
			if err != nil {
				return exitcode.New(exitcode.UserError, "user_error", err, "check the body flag values")
			}
		}

		if c.Bool(flagDryRun) {
			return rt.renderDryRun(desc.Method, url, body, contentType, preview, c.String(flagOutput))
		}
		return rt.fire(ctx, desc, desc.Method, url, body, contentType, c.String(flagQuery), c.String(flagOutput))
	}
}

func (rt *runtime) resolveCLIURL(ctx context.Context, c *cli.Command, desc opDescriptor, positional []string) (string, error) {
	query, err := resolveCLIQuery(c, desc)
	if err != nil {
		return "", err
	}
	if err := rt.CheckRestrictions(desc.PathParams, positional); err != nil {
		return "", err
	}
	base, err := rt.BaseForRequest(ctx, c.Bool(flagDryRun))
	if err != nil {
		return "", err
	}
	return base + opcore.FillPath(desc.Path, positional) + query, nil
}

// contentTypeJSON is the body content type for every non-multipart verb.
const contentTypeJSON = "application/json"

// assembleMultipart builds a multipart/form-data body from the set form flags,
// streaming "file" params from paths; a dry run returns a part-name preview.
func assembleMultipart(c *cli.Command, flags []fieldFlag, dryRun bool) (body []byte, contentType string, preview any, err error) {
	if dryRun {
		return nil, "multipart/form-data", multipartPreview(c, flags), nil
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range flags {
		if !c.IsSet(f.Name) {
			continue
		}
		if f.Type != "file" {
			if err := w.WriteField(f.Name, c.String(f.Name)); err != nil {
				return nil, "", nil, err
			}
			continue
		}
		if err := writeFilePart(w, f.Name, c.String(f.Name)); err != nil {
			return nil, "", nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", nil, err
	}
	return buf.Bytes(), w.FormDataContentType(), nil, nil
}

// multipartPreview names each set form part for a dry run, file paths @-marked.
func multipartPreview(c *cli.Command, flags []fieldFlag) map[string]string {
	parts := map[string]string{}
	for _, f := range flags {
		if !c.IsSet(f.Name) {
			continue
		}
		v := c.String(f.Name)
		if f.Type == "file" {
			v = "@" + v
		}
		parts[f.Name] = v
	}
	return parts
}

// writeFilePart streams one file param from its path into the multipart body.
func writeFilePart(w *multipart.Writer, field, path string) error {
	part, err := w.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open --%s: %w", field, err)
	}
	_, err = io.Copy(part, src)
	_ = src.Close()
	if err != nil {
		return fmt.Errorf("read --%s: %w", field, err)
	}
	return nil
}

// resolveCLIQuery adapts set CLI flags to typed opcore inputs, then delegates
// validation, policy gating, aliasing, and repeated-key assembly to opcore.
func resolveCLIQuery(c *cli.Command, desc opDescriptor) (string, error) {
	values, err := queryValuesFromCLI(c, desc.QueryFlags)
	if err != nil {
		return "", err
	}
	query, err := (opcore.Operation{Desc: desc}).ResolveQuery(opcore.Args{QueryValues: values})
	if err != nil {
		return "", err
	}
	if len(query) == 0 {
		return "", nil
	}
	return "?" + query.Encode(), nil
}

func queryValuesFromCLI(c *cli.Command, flags []fieldFlag) (map[string]any, error) {
	values := map[string]any{}
	for _, f := range flags {
		if !c.IsSet(f.Name) {
			continue
		}
		value, err := queryFlagValue(c, f)
		if err != nil {
			return nil, err
		}
		values[f.Name] = value
	}
	return values, nil
}

func queryFlagValue(c *cli.Command, f fieldFlag) (any, error) {
	if f.Type != "array" || f.Items != "boolean" {
		return flagValue(c, f), nil
	}
	raw := c.StringSlice(f.Name)
	values := make([]bool, len(raw))
	for i, value := range raw {
		parsed, err := strconv.ParseBool(value)
		if err != nil || (value != "true" && value != "false") {
			return nil, exitcode.New(exitcode.UserError, "user_error",
				fmt.Errorf("query field %q item %d must be true or false", f.Name, i),
				"supply each boolean array value as true or false")
		}
		values[i] = parsed
	}
	return values, nil
}

// assembleBody builds the body JSON from --body-file or the body flags; unset
// optionals are omitted and required fields enforced over whichever source.
func assembleBody(c *cli.Command, flags []fieldFlag) ([]byte, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	obj, err := bodyObject(c, flags)
	if err != nil {
		return nil, err
	}
	for _, f := range flags {
		if f.Required {
			if _, present := obj[f.Name]; !present {
				return nil, fmt.Errorf("required body field %q is missing (set --%s or supply it via --%s)", f.Name, f.Name, flagBodyFile)
			}
		}
	}
	if len(obj) == 0 {
		return nil, nil
	}
	return json.Marshal(obj)
}

// bodyObject collects the body fields from --body-file or the set body flags,
// the two mutually exclusive sources.
func bodyObject(c *cli.Command, flags []fieldFlag) (map[string]any, error) {
	obj := map[string]any{}
	if !c.IsSet(flagBodyFile) {
		for _, f := range flags {
			if c.IsSet(f.Name) {
				obj[f.Name] = flagValue(c, f)
			}
		}
		return obj, nil
	}
	for _, f := range flags {
		if c.IsSet(f.Name) {
			return nil, fmt.Errorf("--%s and --%s are mutually exclusive", flagBodyFile, f.Name)
		}
	}
	raw, err := os.ReadFile(c.String(flagBodyFile))
	if err != nil {
		return nil, fmt.Errorf("read --%s: %w", flagBodyFile, err)
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("--%s must hold a JSON object: %w", flagBodyFile, err)
	}
	return obj, nil
}

// itemsAny marks an array whose swagger `items` schema is empty: the spec says
// the list carries more than one type. Forgejo labels. agentic-os#1047
const itemsAny = opcore.ItemsAny

// anyValue lowers one token: all digits becomes a JSON number, anything else
// stays a string, which is the only encoding a both-types spec allows.
func anyValue(token string) any { return opcore.AnyItem(token) }

func anyValues(tokens []string) []any {
	out := make([]any, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, anyValue(token))
	}
	return out
}

// flagValue reads one set flag as the JSON value its swagger type implies.
func flagValue(c *cli.Command, f fieldFlag) any {
	switch f.Type {
	case "boolean":
		return c.Bool(f.Name)
	case "integer":
		return c.Int(f.Name)
	case "number":
		return c.Float(f.Name)
	case "array":
		switch f.Items {
		case "integer":
			return c.IntSlice(f.Name)
		case "number":
			return c.FloatSlice(f.Name)
		case itemsAny:
			return anyValues(c.StringSlice(f.Name))
		default:
			return c.StringSlice(f.Name)
		}
	default:
		return c.String(f.Name)
	}
}

// renderDryRun prints the resolved request without firing it, auth value
// redacted, honoring --output so a dry-run reads the same as a live response.
func (rt *runtime) renderDryRun(method, url string, body []byte, contentType string, bodyPreview any, output string) error {
	preview := map[string]any{
		"method":  method,
		"url":     rt.previewURL(url),
		"headers": rt.previewHeaders(body != nil || bodyPreview != nil, contentType),
	}
	switch {
	case bodyPreview != nil: // multipart: name the parts, never dump encodings
		preview["body"] = bodyPreview
	case body != nil:
		var parsed any
		if err := json.Unmarshal(body, &parsed); err == nil {
			preview["body"] = parsed
		} else {
			preview["body"] = string(body)
		}
	}
	raw, err := json.Marshal(preview)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	rendered, err := respfmt.Render(raw, "", output)
	if err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	fmt.Print(string(rendered))
	return nil
}

// previewHeaders builds the header map a dry-run shows, redacting the secret. The
// query-param scheme carries no auth header (it rides the URL); see previewURL.
func (rt *runtime) previewHeaders(hasBody bool, contentType string) map[string]string {
	h := map[string]string{}
	if rt.Auth.Header != "" {
		h[rt.Auth.Header] = rt.Auth.Prefix + redacted
	}
	if hasBody {
		h["Content-Type"] = contentType
	}
	return h
}

// previewURL returns the URL a dry-run shows: for the query-param scheme it
// appends each auth parameter with a redacted value; other schemes pass through.
func (rt *runtime) previewURL(url string) string {
	if rt.Auth.Scheme != "query-param" || len(rt.Auth.Params) == 0 {
		return url
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	parts := make([]string, len(rt.Auth.Params))
	for i, p := range rt.Auth.Params {
		parts[i] = p.Name + "=" + redacted
	}
	return url + sep + strings.Join(parts, "&")
}

// fire captures the response through the engine core and renders it.
// Non-2xx becomes an UpstreamFailed coded error carrying the response body.
func (rt *runtime) fire(ctx context.Context, desc opDescriptor, method, url string, body []byte, contentType, query, output string) error {
	// Chosen before the call, not after. Decoding first fails on a declared
	// plaintext or ZIP body, which left this branch unreachable. See #289.
	if desc.RawResponse {
		respBody, status, rerr := rt.FireCaptureRaw(ctx, method, url, body, contentType)
		if rerr != nil {
			return rerr
		}
		return writeRawResponse(respBody, query, method, url, status)
	}
	_, respBody, status, err := rt.FireCapture(ctx, method, url, body, contentType)
	if err != nil {
		return err
	}
	rendered, rerr := respfmt.Render(respBody, query, output)
	if rerr != nil {
		return exitcode.New(exitcode.Internal, "internal", rerr, "the response was not valid JSON")
	}
	if len(rendered) == 0 {
		// empty 2xx (204): confirm so the operator sees the call landed.
		fmt.Printf("ok: %s %s -> %s\n", method, url, status)
		return nil
	}
	fmt.Print(string(rendered))
	return nil
}

// writeRawResponse emits a non-JSON success body byte for byte, refusing a
// projection rather than ignoring it. See docs/specverb-request.md.
func writeRawResponse(body []byte, query, method, url, status string) error {
	if strings.TrimSpace(query) != "" {
		return exitcode.New(exitcode.UserError, "user_error",
			fmt.Errorf("--%s cannot project a non-JSON response", flagQuery),
			"this verb returns raw bytes; drop the query and filter downstream")
	}
	if len(body) == 0 {
		// empty 2xx: confirm the call landed, matching the parsed path.
		fmt.Printf("ok: %s %s -> %s\n", method, url, status)
		return nil
	}
	if _, err := os.Stdout.Write(body); err != nil {
		return exitcode.New(exitcode.Internal, "internal", err, "")
	}
	return nil
}
