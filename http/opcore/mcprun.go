package opcore

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/mcpclient"
)

// MCPCall marks a leaf of the mcp dialect: the exact upstream tool it fires and
// the guards over that call. A leaf carrying it assembles no request.
type MCPCall struct {
	Tool string

	// Allow and Deny guard the outgoing arguments, PostCall the returned value.
	// All three match a regex against one named string field.
	Allow    []ProxyRule
	Deny     []ProxyRule
	PostCall []ProxyRule
}

// MCPUpstream is the wrap-level `mcp <transport> { ... }` declaration, with its
// credential material still symbolic: chains resolve per call, not at parse.
type MCPUpstream struct {
	Kind string // "stdio" | "http"

	Command string
	Argv    []string
	Env     []MCPEnv

	URL      string
	URLValue guardfile.ValueChain
	Auth     guardfile.Auth
}

// MCPEnv is one environment injection on a stdio upstream.
type MCPEnv struct {
	Name  string
	Value guardfile.ValueChain
}

// IsZero reports whether no upstream was declared.
func (u MCPUpstream) IsZero() bool { return u.Kind == "" }

// executeMCP connects, guards the arguments, fires one tools/call, and closes.
func (o Operation) executeMCP(ctx context.Context, a Args) (Response, error) {
	call := o.Desc.MCP
	args := mcpArguments(a)
	if err := checkProxyRules(call, args); err != nil {
		return Response{}, err
	}
	server, err := o.RT.mcpServer(ctx)
	if err != nil {
		return Response{}, err
	}
	sess, err := mcpclient.Connect(ctx, server)
	if err != nil {
		return Response{}, exitcode.New(exitcode.UpstreamFailed, "upstream_failed", err,
			"check the declared mcp transport, and that the upstream server starts")
	}
	defer func() { _ = sess.Close() }()

	res, err := sess.CallTool(ctx, call.Tool, args)
	if err != nil {
		return Response{}, exitcode.New(exitcode.UpstreamFailed, "upstream_failed", err,
			"check the tool name against the committed lock, and rerun `umbra skew`")
	}
	if res.IsError {
		return Response{}, exitcode.New(exitcode.UpstreamFailed, "upstream_failed",
			fmt.Errorf("tool %s reported an error: %s", call.Tool, summarize(res.Decoded)),
			"the upstream ran the tool and it failed; the message above is the tool's own")
	}
	if err := checkPostCall(call, res.Decoded); err != nil {
		return Response{}, err
	}
	return Response{Decoded: res.Decoded, Raw: res.Raw, Status: "200 OK"}, nil
}

// mcpArguments flattens the bound inputs into the single argument object a
// tools/call takes. One input object, so every slot is just a key in it.
func mcpArguments(a Args) map[string]any {
	out := map[string]any{}
	for k, v := range a.Path {
		out[k] = v
	}
	for k, v := range a.Query {
		out[k] = v
	}
	for k, v := range a.QueryValues {
		out[k] = v
	}
	for k, v := range a.Body {
		out[k] = v
	}
	return out
}

// mcpServer resolves the declared upstream's value chains into a connectable
// server. Resolution happens per call so a rotated credential is picked up.
func (rt *Runtime) mcpServer(ctx context.Context) (mcpclient.Server, error) {
	u := rt.MCP
	if u.IsZero() {
		return mcpclient.Server{}, exitcode.New(exitcode.Internal, "internal",
			fmt.Errorf("no `mcp` upstream declared for an mcp grant"),
			"add `mcp stdio { ... }` or `mcp http { ... }` to the wrap block")
	}
	if u.Kind == "stdio" {
		env := make([]string, 0, len(u.Env))
		for _, e := range u.Env {
			v, err := rt.resolveChain(ctx, e.Value)
			if err != nil {
				return mcpclient.Server{}, err
			}
			env = append(env, e.Name+"="+v)
		}
		return mcpclient.Server{
			Name:  u.Command,
			Stdio: &mcpclient.Stdio{Command: u.Command, Argv: u.Argv, Env: env},
		}, nil
	}
	url := u.URL
	if url == "" {
		v, err := rt.resolveChain(ctx, u.URLValue)
		if err != nil {
			return mcpclient.Server{}, err
		}
		url = v
	}
	if !strings.Contains(url, "://") {
		url = DefaultScheme(url) + "://" + url
	}
	headers, err := rt.mcpHeaders(ctx)
	if err != nil {
		return mcpclient.Server{}, err
	}
	return mcpclient.Server{
		Name: url,
		HTTP: &mcpclient.HTTPEndpoint{URL: url, Headers: headers, Client: rt.Client},
	}, nil
}

// mcpHeaders resolves the declared auth into request headers. The SDK owns the
// request, so the credential attaches as a header rather than per call.
func (rt *Runtime) mcpHeaders(ctx context.Context) (map[string]string, error) {
	headers := map[string]string{}
	for k, v := range rt.Headers {
		headers[k] = v
	}
	a := rt.MCP.Auth
	switch a.Scheme {
	case "", guardfile.AuthSchemeNone:
		return headers, nil
	case "header-token", "bearer":
		token, err := rt.resolveChain(ctx, a.Value)
		if err != nil {
			return nil, err
		}
		name, prefix := a.Header, a.Prefix
		if a.Scheme == "bearer" {
			name, prefix = "Authorization", "Bearer "
		}
		if name == "" {
			name = "Authorization"
		}
		headers[name] = prefix + token
		return headers, nil
	default:
		// query-param auth has no meaning for a single MCP endpoint, and
		// silently ignoring a declared credential would read as authenticated.
		return nil, exitcode.New(exitcode.Internal, "internal",
			fmt.Errorf("auth scheme %q is not supported on an mcp http upstream", a.Scheme),
			"use `auth header-token { ... }`, `auth bearer { ... }`, or `auth none`")
	}
}

// checkProxyRules enforces the request-side guards over the bound arguments.
func checkProxyRules(call *MCPCall, args map[string]any) error {
	return CheckProxyRules(call.Allow, call.Deny, args)
}

// CheckProxyRules enforces request-side allow and deny guards over one argument
// map. Exported so the MCP Apps host refuses on identical regex semantics.
func CheckProxyRules(allow, deny []ProxyRule, args map[string]any) error {
	for _, r := range deny {
		v, ok := stringField(args, r.Field)
		if !ok {
			continue
		}
		if matchesAny(v, r.Patterns) {
			return proxyDenied(r, "deny", v)
		}
	}
	for _, r := range allow {
		v, ok := stringField(args, r.Field)
		if !ok {
			continue
		}
		if !matchesAny(v, r.Patterns) {
			return proxyDenied(r, "allow", v)
		}
	}
	return nil
}

// checkPostCall enforces the response-side guards. A match rejects, the same
// direction as `deny` and a truthy `fail-when`: every guard reads one way.
func checkPostCall(call *MCPCall, decoded any) error {
	if len(call.PostCall) == 0 {
		return nil
	}
	fields := responseFields(decoded)
	for _, r := range call.PostCall {
		v, ok := fields[r.Field]
		if !ok {
			continue
		}
		if matchesAny(v, r.Patterns) {
			return exitcode.New(exitcode.PolicyDenied, "policy_denied",
				fmt.Errorf("response field %s=%q matched post-call %v", r.Field, v, r.Patterns),
				"the upstream answered, and the declared post-call guard rejected the response")
		}
	}
	return nil
}

// responseFields flattens a decoded result into the string fields a post-call
// rule can name. A scalar result is addressable as `text`.
func responseFields(decoded any) map[string]string {
	out := map[string]string{}
	switch v := decoded.(type) {
	case string:
		out["text"] = v
	case map[string]any:
		for k, raw := range v {
			if s, ok := raw.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// stringField reads one argument as a string. A non-string argument renders
// through fmt so a guard over a number or bool still sees a value to match.
func stringField(args map[string]any, name string) (string, bool) {
	raw, ok := args[name]
	if !ok || raw == nil {
		return "", false
	}
	if s, ok := raw.(string); ok {
		return s, true
	}
	return fmt.Sprint(raw), true
}

// matchesAny reports whether val matches at least one pattern. A malformed
// pattern matches nothing, so a typo never widens an allow or narrows a deny.
func matchesAny(val string, patterns []string) bool {
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		if re.MatchString(val) {
			return true
		}
	}
	return false
}

// proxyDenied builds the refusal for one guard, naming the rule that fired.
func proxyDenied(r ProxyRule, mode, val string) error {
	return exitcode.New(exitcode.PolicyDenied, "policy_denied",
		fmt.Errorf("argument %s=%q is outside the allowed scope (%s %s matches %v)", r.Field, val, mode, r.Field, r.Patterns),
		"supply a value the declared guard permits, or widen the guard")
}

// summarize renders a tool's own error content for the refusal message, bounded
// so a large payload does not become the whole error.
func summarize(decoded any) string {
	var s string
	switch v := decoded.(type) {
	case string:
		s = v
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v[k]))
		}
		s = strings.Join(parts, " ")
	default:
		s = fmt.Sprint(decoded)
	}
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
