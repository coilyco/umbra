// Package mcpclient is the Model Context Protocol client umbra's mcp dialect
// speaks: one declared upstream server, a session over it, and the three calls
// the dialect needs (tools/list, tools/call, resources/read).
//
// It lives in pkg/ rather than a guarded surface because it expresses no
// permission. Policy is the guardfile's, enforced above this by http/mcpverb.
// See docs/mcpverb.md.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coilyco/umbra/pkg/policy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// clientName identifies umbra to the upstream during initialize. Servers log it
// and some gate behaviour on it, so it stays stable across versions.
const clientName = "umbra"

// DefaultTimeout bounds a single session's whole lifetime when the caller
// supplies no deadline, so a silent stdio server cannot hang the CLI.
const DefaultTimeout = 60 * time.Second

// terminateGrace bounds Close's wait for a stdio child to exit before SIGTERM.
// The SDK's 5s default costs that per call. See docs/mcpverb.md.
const terminateGrace = 100 * time.Millisecond

// Server is one fully declared upstream. Exactly one transport is set, which
// Validate enforces: an under-declared server is a policy hole, not a default.
type Server struct {
	// Name is the guardfile's name for this upstream, used only in errors.
	Name string

	Stdio *Stdio
	HTTP  *HTTPEndpoint
}

// Stdio is the subprocess transport: a command umbra starts and speaks
// newline-delimited JSON to over its stdin and stdout.
type Stdio struct {
	Command string
	Argv    []string
	// Env are `NAME=VALUE` overrides the caller already resolved. A secret reaches
	// the child here rather than through argv, which any local process can read.
	Env []string
}

// HTTPEndpoint is the Streamable HTTP transport.
type HTTPEndpoint struct {
	URL     string
	Headers map[string]string
	Client  *http.Client
}

// Validate refuses a server that is under- or over-declared, and puts a stdio
// command and argv through pkg/policy: a spawn gets no weaker gate here.
func (s Server) Validate() error {
	switch {
	case s.Stdio == nil && s.HTTP == nil:
		return fmt.Errorf("mcpclient: server %q declares no transport; add `mcp stdio { ... }` or `mcp http { ... }`", s.Name)
	case s.Stdio != nil && s.HTTP != nil:
		return fmt.Errorf("mcpclient: server %q declares both stdio and http; a server speaks one transport", s.Name)
	}
	if s.HTTP != nil {
		if s.HTTP.URL == "" {
			return fmt.Errorf("mcpclient: server %q has an http transport with no url", s.Name)
		}
		return nil
	}
	if s.Stdio.Command == "" {
		return fmt.Errorf("mcpclient: server %q has a stdio transport with no command", s.Name)
	}
	if err := policy.ValidateArg("command", s.Stdio.Command); err != nil {
		return fmt.Errorf("mcpclient: server %q: %w", s.Name, err)
	}
	if err := policy.ValidateArgSlice("argv", s.Stdio.Argv); err != nil {
		return fmt.Errorf("mcpclient: server %q: %w", s.Name, err)
	}
	return nil
}

// Session is a live client session over one upstream. Close it.
type Session struct {
	cs *mcp.ClientSession
}

// Progress is one upstream notifications/progress, correlated by the token the
// call carried. Total is zero when the upstream does not know it.
type Progress struct {
	Token    any
	Progress float64
	Total    float64
	Message  string
}

// Options tune one session beyond its declaration. The zero value is what
// Connect uses, and is what a CLI leaf wants.
type Options struct {
	// OnProgress receives the upstream's progress for a call that carried a
	// token. nil drops them, since a one-shot leaf has nowhere to show them.
	OnProgress func(Progress)
}

// Connect validates the declaration, starts or dials the transport, and
// completes the initialize handshake.
func Connect(ctx context.Context, s Server) (*Session, error) {
	return ConnectWith(ctx, s, Options{})
}

// ConnectWith is Connect with session options, which a long-lived consumer
// such as an MCP Apps host needs and a one-shot call does not.
func ConnectWith(ctx context.Context, s Server, opts Options) (*Session, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	transport, probe := s.transportWithProbe(ctx)
	sess, err := connect(ctx, transport, opts)
	if err != nil {
		if probe != nil {
			if hint := probe.refusal(len(s.HTTP.Headers) > 0); hint != "" {
				return nil, fmt.Errorf("mcpclient: connect %s: %s: %w", s.label(), hint, err)
			}
		}
		return nil, fmt.Errorf("mcpclient: connect %s: %w", s.label(), err)
	}
	return sess, nil
}

// transportWithProbe builds the transport and, for http, the observer that
// turns a bare 401 into something an operator can act on.
func (s Server) transportWithProbe(ctx context.Context) (mcp.Transport, *headerTransport) {
	if s.HTTP == nil {
		return s.transport(ctx), nil
	}
	client, probe := s.httpClient()
	return &mcp.StreamableClientTransport{Endpoint: s.HTTP.URL, HTTPClient: client}, probe
}

// connect completes the handshake over an already-built transport. Separate
// from Connect so a test drives an in-memory transport without a subprocess.
func connect(ctx context.Context, t mcp.Transport, opts Options) (*Session, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "v0"}, clientOptions(opts))
	cs, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, err
	}
	return &Session{cs: cs}, nil
}

// clientOptions adapts umbra's session options onto the SDK's, or nil when
// nothing is set so the SDK keeps its own defaults.
func clientOptions(opts Options) *mcp.ClientOptions {
	if opts.OnProgress == nil {
		return nil
	}
	return &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			p := req.Params
			opts.OnProgress(Progress{
				Token: p.ProgressToken, Progress: p.Progress, Total: p.Total, Message: p.Message,
			})
		},
	}
}

// transport builds the SDK transport for the declared shape.
func (s Server) transport(ctx context.Context) mcp.Transport {
	if s.HTTP != nil {
		client, _ := s.httpClient()
		return &mcp.StreamableClientTransport{Endpoint: s.HTTP.URL, HTTPClient: client}
	}
	cmd := exec.CommandContext(ctx, s.Stdio.Command, s.Stdio.Argv...) //nolint:gosec // argv is guardfile-declared and policy-validated above
	if len(s.Stdio.Env) > 0 {
		cmd.Env = append(cmd.Environ(), s.Stdio.Env...)
	}
	return &mcp.CommandTransport{Command: cmd, TerminateDuration: terminateGrace}
}

// httpClient returns a client carrying the declared headers, wrapped so a
// refusal is observed rather than reduced to "Unauthorized".
func (s Server) httpClient() (*http.Client, *headerTransport) {
	base := s.HTTP.Client
	if base == nil {
		base = &http.Client{Timeout: DefaultTimeout}
	}
	ht := &headerTransport{base: base.Transport, headers: s.HTTP.Headers}
	clone := *base
	clone.Transport = ht
	return &clone, ht
}

// headerTransport adds the declared headers to every request and remembers a
// refusal. The SDK owns request construction, so both happen here.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string

	mu        sync.Mutex
	status    int
	challenge string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	// Cloning keeps the caller's request untouched, which RoundTripper requires.
	out := req.Clone(req.Context())
	for k, v := range t.headers {
		out.Header.Set(k, v)
	}
	resp, err := base.RoundTrip(out)
	if err == nil && resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		t.mu.Lock()
		t.status = resp.StatusCode
		t.challenge = resp.Header.Get("WWW-Authenticate")
		t.mu.Unlock()
	}
	return resp, err
}

// refusal returns guidance when the upstream rejected the credential, or "".
// The SDK reports a 401 as the bare word "Unauthorized". See docs/mcpverb.md.
func (t *headerTransport) refusal(authConfigured bool) string {
	t.mu.Lock()
	status, challenge := t.status, t.challenge
	t.mu.Unlock()
	if status == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the upstream refused the credential (HTTP %d)", status)
	switch {
	case !authConfigured:
		b.WriteString("; this `mcp http` block declares no `auth`")
	default:
		b.WriteString("; the declared `auth` value resolved but was rejected")
	}
	if wantsOAuth(challenge) {
		b.WriteString(". It advertises OAuth, which umbra does not acquire: supply an already-minted token through `auth bearer { value ... }`. See docs/mcpverb.md")
	}
	return b.String()
}

// wantsOAuth reports whether a WWW-Authenticate challenge asks for OAuth. The
// scheme is `Bearer`, and MCP servers add an OAuth resource-metadata pointer.
func wantsOAuth(challenge string) bool {
	c := strings.ToLower(challenge)
	return strings.Contains(c, "bearer") || strings.Contains(c, "oauth")
}

// label names the upstream in an error without leaking a resolved secret.
func (s Server) label() string {
	switch {
	case s.HTTP != nil:
		return fmt.Sprintf("%s (%s)", s.Name, s.HTTP.URL)
	case s.Stdio != nil:
		return fmt.Sprintf("%s (%s)", s.Name, s.Stdio.Command)
	default:
		return s.Name
	}
}

// Close ends the session, terminating a stdio child.
func (s *Session) Close() error {
	if s == nil || s.cs == nil {
		return nil
	}
	return s.cs.Close()
}

// Tool is one upstream tool as umbra records it. It is umbra's own type rather
// than the SDK's so the lock format does not move when the SDK does.
type Tool struct {
	Name         string         `json:"name"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"inputSchema,omitempty"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
	Meta         map[string]any `json:"_meta,omitempty"`
}

// ListTools walks every page and returns the full surface sorted by name,
// because tools/list order is unspecified and this result reaches a lock.
func (s *Session) ListTools(ctx context.Context) ([]Tool, error) {
	var tools []Tool
	for tool, err := range s.cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcpclient: list tools: %w", err)
		}
		tools = append(tools, convertTool(tool))
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

// convertTool re-marshals the SDK's tool into umbra's. InputSchema arrives as
// `any`, so it round-trips through JSON rather than a lossy type assertion.
func convertTool(t *mcp.Tool) Tool {
	out := Tool{
		Name:        t.Name,
		Title:       t.Title,
		Description: t.Description,
		Meta:        t.Meta,
	}
	out.InputSchema = asObject(t.InputSchema)
	out.OutputSchema = asObject(t.OutputSchema)
	out.Annotations = asObject(t.Annotations)
	return out
}

// asObject renders any JSON-marshalable value as a generic object, or nil when
// it is not one. A non-object schema binds no flags, which mcpverb reports.
func asObject(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// Result is one tools/call outcome for the guard floor above: Decoded feeds a
// fail-when postcondition, Raw the renderer, IsError a tool-reported failure.
type Result struct {
	Decoded any
	Raw     []byte
	IsError bool
}

// Call is one tools/call. ProgressToken, when non-nil, rides as
// `_meta.progressToken` so the upstream's progress notifications correlate.
type Call struct {
	Name          string
	Arguments     map[string]any
	ProgressToken any
}

// CallTool fires one tool with already-bound arguments and no progress token.
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any) (Result, error) {
	return s.Call(ctx, Call{Name: name, Arguments: args})
}

// Call fires one tool, carrying a progress token when the caller has one.
func (s *Session) Call(ctx context.Context, c Call) (Result, error) {
	name := c.Name
	params := &mcp.CallToolParams{Name: name, Arguments: c.Arguments}
	if c.ProgressToken != nil {
		params.SetProgressToken(c.ProgressToken)
	}
	res, err := s.cs.CallTool(ctx, params)
	if err != nil {
		return Result{}, fmt.Errorf("mcpclient: call %s: %w", name, err)
	}
	decoded := decodeResult(res)
	raw, err := json.Marshal(decoded)
	if err != nil {
		return Result{}, fmt.Errorf("mcpclient: encode %s result: %w", name, err)
	}
	return Result{Decoded: decoded, Raw: raw, IsError: res.IsError}, nil
}

// decodeResult picks the most structured form the result offers. A server that
// sets StructuredContent means it, so it wins over the same value as text.
func decodeResult(res *mcp.CallToolResult) any {
	if res.StructuredContent != nil {
		return res.StructuredContent
	}
	texts := make([]any, 0, len(res.Content))
	allText := true
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, decodeText(tc.Text))
			continue
		}
		allText = false
		break
	}
	switch {
	case allText && len(texts) == 1:
		return texts[0]
	case allText && len(texts) > 0:
		return texts
	}
	// A non-text block (image, audio, embedded resource) has no useful scalar
	// form, so the content list goes out as the protocol shaped it.
	return res.Content
}

// decodeText re-decodes a text block that is itself JSON, which a server with
// no output schema commonly returns. Left as a string, every guard is substring.
func decodeText(text string) any {
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return text
	}
	switch v.(type) {
	case map[string]any, []any:
		return v
	}
	// A bare number or bool that happens to parse stays the text it was.
	return text
}

// Resource is one upstream resource as umbra records it, the fields a listing
// shows. It is umbra's own type so the SDK's shape does not reach consumers.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// ListResources walks every page and returns the full listing sorted by URI,
// because resources/list order is unspecified.
func (s *Session) ListResources(ctx context.Context) ([]Resource, error) {
	var out []Resource
	for r, err := range s.cs.Resources(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcpclient: list resources: %w", err)
		}
		out = append(out, Resource{
			URI: r.URI, Name: r.Name, Title: r.Title,
			Description: r.Description, MIMEType: r.MIMEType,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

// ReadResource reads one resource URI. This serves a `ui://` MCP Apps resource,
// which is an ordinary resources/read against the same session.
func (s *Session) ReadResource(ctx context.Context, uri string) (*mcp.ReadResourceResult, error) {
	res, err := s.cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, fmt.Errorf("mcpclient: read resource %s: %w", uri, err)
	}
	return res, nil
}
