package opcore

import (
	"bytes"
	"context"
	sqldb "database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/valuesource"
)

// Provider resolves the value at address for one named value source: the shared
// valuesource.Provider, so the consumer's one registry drives both engines.
type Provider = valuesource.Provider

// contentTypeJSON is the body content type for every non-multipart verb.
const contentTypeJSON = "application/json"

// defaultHTTPTimeout bounds a single live request made through the engine's
// default client (RuntimeConfig.Client nil).
const defaultHTTPTimeout = 30 * time.Second

// Runtime carries the per-tree request dependencies shared by every leaf,
// urfave/cli-free: specverb embeds it and a non-CLI consumer drives it directly.
type Runtime struct {
	BaseURL   string
	Auth      guardfile.Auth
	Providers map[string]Provider
	Client    *http.Client
	Restrict  []guardfile.Restriction

	// AllowMeta names the path params the wrap's `allow-metacharacters` exempts.
	// Query is exempt by construction, never by this list; specverb-request.md.
	AllowMeta []string

	// Headers are wrap-declared request headers, applied before the default
	// User-Agent. See docs/specverb-request.md.
	Headers map[string]string

	// BaseURLValue (zero = static BaseURL) resolves the host through the first
	// available source in its chain once, caching it. See specverb-policy.md.
	BaseURLValue guardfile.ValueChain
	baseURLOnce  sync.Once
	baseURLVal   string
	baseURLErr   error

	// Database backs `sql` grants. The pool opens once on first use, so a
	// consumer that serves no SQL grant never opens a connection.
	Database Database
	dbOnce   sync.Once
	db       *sqldb.DB
	dbErr    error

	// MCP is the wrap-level upstream backing `mcp` dialect leaves. Its value
	// chains stay symbolic here and resolve per call, never at parse.
	MCP MCPUpstream
}

// RuntimeConfig is the input to NewRuntime: BaseURL is raw (scheme + trim
// applied), a nil Client defaults to the redirect-guarding client.
type RuntimeConfig struct {
	BaseURL      string
	Auth         guardfile.Auth
	Providers    map[string]Provider
	Client       *http.Client
	Restrict     []guardfile.Restriction
	AllowMeta    []string
	Headers      map[string]string
	BaseURLValue guardfile.ValueChain
	Database     Database
	MCP          MCPUpstream
}

// NewRuntime assembles a Runtime, defaulting the base-url scheme and the HTTP
// client; the wrap pipeline and step runner are CLI-layer concerns layered on top.
func NewRuntime(c RuntimeConfig) *Runtime {
	client := c.Client
	if client == nil {
		client = defaultHTTPClient()
	}
	return &Runtime{
		BaseURL:      DefaultScheme(strings.TrimRight(c.BaseURL, "/")),
		Auth:         c.Auth,
		Providers:    c.Providers,
		Client:       client,
		Restrict:     c.Restrict,
		AllowMeta:    c.AllowMeta,
		Headers:      c.Headers,
		BaseURLValue: c.BaseURLValue,
		Database:     c.Database,
		MCP:          c.MCP,
	}
}

// DefaultScheme prepends https:// to a base-url that carries no scheme, so a
// Guardfile may write `base-url "host/api/v1"` and rely on TLS by default.
func DefaultScheme(baseURL string) string {
	if baseURL == "" || strings.Contains(baseURL, "://") {
		return baseURL
	}
	return "https://" + baseURL
}

// defaultHTTPClient (used when RuntimeConfig.Client is nil) follows GET/HEAD
// redirects but refuses them for mutating methods. See docs/specverb.md.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultHTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			switch via[0].Method {
			case http.MethodGet, http.MethodHead:
				return nil
			}
			return fmt.Errorf("opcore: refusing to follow a %s redirect to %s; a mutating verb must not be silently downgraded", via[0].Method, req.URL)
		},
	}
}

// BaseForRequest returns the request base-url: the static base, or (for a
// `base-url { value }` host) a once-resolved provider value. Dry-run stays offline.
func (rt *Runtime) BaseForRequest(ctx context.Context, dry bool) (string, error) {
	if rt.BaseURLValue.IsZero() {
		return rt.BaseURL, nil
	}
	if dry {
		return "{base-url:" + rt.BaseURLValue.String() + "}", nil
	}
	rt.baseURLOnce.Do(func() {
		v, err := rt.resolveChain(ctx, rt.BaseURLValue)
		if err != nil {
			rt.baseURLErr = err
			return
		}
		rt.baseURLVal = DefaultScheme(strings.TrimRight(v, "/"))
	})
	if rt.baseURLErr != nil {
		return "", rt.baseURLErr
	}
	return rt.baseURLVal, nil
}

// authorize resolves the scheme's secret(s) and applies them to req: a header
// for header-token/bearer, or query parameters for query-param.
func (rt *Runtime) authorize(ctx context.Context, req *http.Request) error {
	// `none` is stated credential-free, empty is a spec that forgot; both send
	// nothing. See docs/specverb-policy.md.
	if rt.Auth.Scheme == "" || rt.Auth.Scheme == guardfile.AuthSchemeNone {
		return nil
	}
	if rt.Auth.Scheme == "query-param" {
		q := req.URL.Query()
		for _, p := range rt.Auth.Params {
			secret, err := rt.resolveChain(ctx, p.Value)
			if err != nil {
				return err
			}
			q.Set(p.Name, secret)
		}
		req.URL.RawQuery = q.Encode()
		return nil
	}
	secret, err := rt.resolveChain(ctx, rt.Auth.Value)
	if err != nil {
		return err
	}
	req.Header.Set(rt.Auth.Header, rt.Auth.Prefix+secret)
	return nil
}

// Authorize applies the runtime's configured auth to req. Blank auth is a no-op.
func (rt *Runtime) Authorize(ctx context.Context, req *http.Request) error {
	return rt.authorize(ctx, req)
}

// resolveChain resolves a value chain to its first available source, wrapping an
// all-failed chain as a coded Internal error - never a value, never a silent empty.
func (rt *Runtime) resolveChain(ctx context.Context, chain guardfile.ValueChain) (string, error) {
	v, err := valuesource.ResolveFirst(ctx, rt.Providers, chainSources(chain))
	if err != nil {
		return "", exitcode.New(exitcode.Internal, "internal", err,
			"check the value provider address and credentials, or register the provider via specverb.Config.Providers")
	}
	return v, nil
}

// ChainSources lowers a guardfile.ValueChain to the []valuesource.Source the
// shared resolver walks, keeping guardfile free of the resolution layer.
func ChainSources(chain guardfile.ValueChain) []valuesource.Source {
	out := make([]valuesource.Source, len(chain))
	for i, vs := range chain {
		out[i] = valuesource.Source{Provider: vs.Provider, Address: vs.Address}
	}
	return out
}

func chainSources(chain guardfile.ValueChain) []valuesource.Source { return ChainSources(chain) }

// FireCapture sends one request and returns the decoded JSON value plus raw
// body, rendering nothing: the "fire and capture" path complex actions feed on.
func (rt *Runtime) FireCapture(ctx context.Context, method, url string, body []byte, contentType string) (decoded any, raw []byte, status string, err error) {
	respBody, status, serr := rt.send(ctx, method, url, body, contentType)
	if serr != nil {
		return nil, nil, status, serr
	}
	if len(bytes.TrimSpace(respBody)) == 0 {
		return nil, respBody, status, nil
	}
	if jerr := json.Unmarshal(respBody, &decoded); jerr != nil {
		return nil, respBody, status, exitcode.New(exitcode.Internal, "internal", jerr, "the response was not valid JSON")
	}
	return decoded, respBody, status, nil
}

// FireCaptureRaw sends one request and returns the body undecoded, for an op
// declaring a non-JSON media type. See docs/specverb-request.md.
func (rt *Runtime) FireCaptureRaw(ctx context.Context, method, url string, body []byte, contentType string) (raw []byte, status string, err error) {
	return rt.send(ctx, method, url, body, contentType)
}

// DefaultUserAgent names this client when nothing else does. Go's default is
// refused outright by some APIs; see docs/specverb-request.md (umbra#303).
const DefaultUserAgent = "umbra/1.0 (+https://github.com/coilyco/umbra)"

// send performs the request and returns the success body. It never inspects the
// payload, so a plaintext log or a ZIP survives it intact.
func (rt *Runtime) send(ctx context.Context, method, url string, body []byte, contentType string) ([]byte, string, error) {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, rerr := http.NewRequestWithContext(ctx, method, url, reqBody)
	if rerr != nil {
		return nil, "", exitcode.New(exitcode.Internal, "internal", rerr, "")
	}
	if aerr := rt.Authorize(ctx, req); aerr != nil {
		return nil, "", aerr
	}
	// Wrap headers first, so a declared User-Agent wins the default below and a
	// caller can state a contact address where an API asks for one.
	for name, value := range rt.Headers {
		req.Header.Set(name, value)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, derr := rt.Client.Do(req)
	if derr != nil {
		return nil, "", exitcode.New(exitcode.UpstreamFailed, "upstream_failed", derr, "the API was unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, resp.Status, exitcode.New(exitcode.UpstreamFailed, "upstream_failed",
			fmt.Errorf("%s %s -> %s: %s", method, url, resp.Status, strings.TrimSpace(string(respBody))),
			"the API rejected the request")
	}
	return respBody, resp.Status, nil
}
