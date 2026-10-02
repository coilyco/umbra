// Package mcpapps is the MCP Apps host bridge: the frame-level half of a
// terminal or embedded host that renders an MCP App and answers the calls that
// App makes back through its postMessage channel.
//
// It lives in pkg/ rather than a guarded surface because it expresses no
// permission. A view-initiated tools/call passes a Policy this package only
// holds an interface for; http/mcpverb compiles a guardfile into one. A Host
// with no Policy forwards nothing, so an undeclared widget reaches no upstream.
// See docs/mcpapps.md.
package mcpapps

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProtocolVersion is the MCP Apps version this host speaks, read from
// `LATEST_PROTOCOL_VERSION` in @modelcontextprotocol/ext-apps spec.types.d.ts.
const ProtocolVersion = "2026-01-26"

// The frame methods this host answers. A view sends the ui/* ones plus ordinary
// MCP; every other method gets a MethodNotFound reply rather than silence.
const (
	MethodInitialize    = "ui/initialize"
	MethodInitialized   = "ui/notifications/initialized"
	MethodToolResult    = "ui/notifications/tool-result"
	MethodSizeChanged   = "ui/notifications/size-changed"
	MethodOpenLink      = "ui/open-link"
	MethodDownloadFile  = "ui/download-file"
	MethodToolsCall     = "tools/call"
	MethodToolsList     = "tools/list"
	MethodResourcesRead = "resources/read"
	MethodResourcesList = "resources/list"
	MethodProgress      = "notifications/progress"
)

// JSON-RPC codes this host returns. PolicyDenied is in the implementation-
// defined range and mirrors umbra's exitcode taxonomy.
const (
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodePolicyDenied   = -32001
	CodeUpstreamFailed = -32002
)

// Session is the upstream this host proxies to: the subset of
// *mcpclient.Session a view can reach, which that type already satisfies.
type Session interface {
	Call(ctx context.Context, c mcpclient.Call) (mcpclient.Result, error)
	ReadResource(ctx context.Context, uri string) (*mcp.ReadResourceResult, error)
	ListResources(ctx context.Context) ([]mcpclient.Resource, error)
}

// Policy decides what a view may reach. This package holds the interface and
// no rules: the guardfile is the policy, compiled above this by http/mcpverb.
type Policy interface {
	// Tools is the surface the view may see and call: deny by absence, the
	// same rule the CLI leaves follow. See docs/mcpapps.md.
	Tools() []mcpclient.Tool

	// CheckToolCall reports why this view-initiated call is refused, or nil. It
	// runs after Tools membership, so it guards arguments rather than names.
	CheckToolCall(tool string, args map[string]any) error

	// CheckResourceRead reports why this view-initiated read is refused, or nil.
	CheckResourceRead(uri string) error

	// CheckOpenLink reports why opening this URL for the view is refused, or
	// nil. Opening a URL an untrusted view chose is its own grant.
	CheckOpenLink(url string) error

	// CheckSaveFile reports why handing this resource to the viewer to save is
	// refused, or nil. It gates the URI of a link and of an inline resource.
	CheckSaveFile(uri string) error
}

// Implementation names a party to the handshake, matching MCP's own shape.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// HostContext is the environment the view renders against. A free map, because
// the spec's McpUiHostContext carries an index signature.
type HostContext map[string]any

// Host answers one view's frames. It holds no transport: a caller reads a
// postMessage frame from wherever it arrives and hands the bytes to Handle.
type Host struct {
	// Info identifies this host to the view during ui/initialize.
	Info Implementation

	// Context is sent as `hostContext`. Omitting it entirely leaves the view's
	// SDK never-ready, so Handle substitutes a minimal one.
	Context HostContext

	// Instantiating is the tools/call result that created this view, pushed as
	// ui/notifications/tool-result when the view says it is initialized.
	Instantiating any

	// Session is the upstream a permitted call reaches. A nil Session refuses
	// every proxied frame rather than dropping it.
	Session Session

	// Policy gates every view-initiated call. A nil Policy permits none, which
	// is what makes an undeclared widget inert.
	Policy Policy

	// Emit posts an unsolicited frame to the view. The SDK calls it from its own
	// goroutine, so it must be safe concurrently. nil drops them.
	Emit func(Reply)

	// OpenLink opens a URL the policy permitted. nil leaves `openLinks`
	// undeclared, so a view learns rather than asking for what never happens.
	OpenLink func(ctx context.Context, url string) error

	// SaveFile hands permitted contents to the viewer. nil leaves
	// `downloadFile` undeclared, for the same reason.
	SaveFile func(ctx context.Context, items []DownloadItem) error

	// mu guards tokens, which correlates one upstream progress token back to
	// the view's own. See docs/mcpapps.md.
	mu     sync.Mutex
	tokens map[string]any
	nextID uint64
}

// DownloadItem is one entry of a ui/download-file request, a link the host
// fetches or an inline resource. URI addresses both, and is what is gated.
type DownloadItem struct {
	URI      string
	MIMEType string

	// Raw is the item as the view sent it, so a handler saving inline bytes
	// reads them without this package modelling every content shape.
	Raw json.RawMessage
}

// Frame is one inbound JSON-RPC message from the view. ID is raw because a
// JSON-RPC id is a string or a number and this host only echoes it back.
type Frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsRequest reports whether the frame expects exactly one reply. A frame with
// no id is a notification, and answering one is a protocol error.
func (f Frame) IsRequest() bool { return len(f.ID) > 0 && string(f.ID) != "null" }

// RPCError is the error member of a reply.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Reply is one outbound frame, carrying a result or an error and never both.
// Unexported members, because postMessage keeps an empty one. docs/mcpapps.md.
type Reply struct {
	id     json.RawMessage
	method string
	params any
	result any
	err    *RPCError
}

// Result answers a request with a value.
func Result(id json.RawMessage, v any) Reply { return Reply{id: id, result: v} }

// Failure answers a request with an error, which is what a refused call gets:
// the view learns rather than waiting on a reply that never arrives.
func Failure(id json.RawMessage, code int, msg string, data any) Reply {
	return Reply{id: id, err: &RPCError{Code: code, Message: msg, Data: data}}
}

// Notify is an unsolicited Host to View frame, carrying no id.
func Notify(method string, params any) Reply { return Reply{method: method, params: params} }

// IsError reports whether this reply carries an error member.
func (r Reply) IsError() bool { return r.err != nil }

// Method names the notification this reply is, or "" for a request answer.
func (r Reply) Method() string { return r.method }

// MarshalJSON emits exactly one of result, error, or method+params.
func (r Reply) MarshalJSON() ([]byte, error) {
	out := map[string]any{"jsonrpc": "2.0"}
	switch {
	case r.method != "":
		out["method"] = r.method
		if r.params != nil {
			out["params"] = r.params
		}
	case r.err != nil:
		out["id"] = r.id
		out["error"] = r.err
	default:
		out["id"] = r.id
		out["result"] = r.result
	}
	return json.Marshal(out)
}

// Handle answers one frame from the view, never returning a Go error. Every
// request yields exactly one reply, a notification only what it triggers.
func (h *Host) Handle(ctx context.Context, raw []byte) []Reply {
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil || f.JSONRPC != "2.0" {
		return nil
	}
	return h.HandleFrame(ctx, f)
}

// HandleFrame is Handle over an already-decoded frame.
func (h *Host) HandleFrame(ctx context.Context, f Frame) []Reply {
	switch f.Method {
	case MethodInitialize:
		return []Reply{Result(f.ID, h.initializeResult())}
	case MethodInitialized:
		// View to Host, not the other way round, and the correct trigger for
		// the instantiating result. Pushing it earlier is dropped silently.
		if h.Instantiating == nil {
			return nil
		}
		return []Reply{Notify(MethodToolResult, h.Instantiating)}
	case MethodToolsList:
		return h.replyOrNothing(f, h.toolsList())
	case MethodToolsCall:
		return h.replyOrNothing(f, h.toolsCall(ctx, f.Params))
	case MethodResourcesRead:
		return h.replyOrNothing(f, h.resourcesRead(ctx, f.Params))
	case MethodResourcesList:
		return h.replyOrNothing(f, h.resourcesList(ctx))
	case MethodOpenLink:
		return h.replyOrNothing(f, h.openLink(ctx, f.Params))
	case MethodDownloadFile:
		return h.replyOrNothing(f, h.downloadFile(ctx, f.Params))
	default:
		if !f.IsRequest() {
			// An unsolicited notification (size-changed, cancelled) needs no
			// answer, and inventing one would be a protocol error.
			return nil
		}
		return []Reply{Failure(f.ID, CodeMethodNotFound, "not implemented: "+f.Method, nil)}
	}
}

// replyOrNothing runs a proxied handler for a request and drops it for a
// notification, so a request always gets exactly one reply.
func (h *Host) replyOrNothing(f Frame, run func() (any, *RPCError)) []Reply {
	if !f.IsRequest() {
		return nil
	}
	v, rpcErr := run()
	if rpcErr != nil {
		return []Reply{Failure(f.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)}
	}
	return []Reply{Result(f.ID, v)}
}

// initializeResult builds the handshake answer. hostCapabilities and
// hostContext are both required, and omitting either fails silently.
func (h *Host) initializeResult() map[string]any {
	hostCtx := h.Context
	if hostCtx == nil {
		hostCtx = HostContext{}
	}
	info := h.Info
	if info.Name == "" {
		info = Implementation{Name: "umbra", Version: "v0"}
	}
	// Load-bearing rather than informational: a view that does not see
	// serverTools declared will not send tools/call at all.
	caps := map[string]any{
		"serverTools":     map[string]any{"listChanged": false},
		"serverResources": map[string]any{"listChanged": false},
	}
	// Declared only when wired. A capability this host would answer -32601 to
	// is the same silent failure as omitting one it does answer.
	if h.OpenLink != nil {
		caps["openLinks"] = map[string]any{}
	}
	if h.SaveFile != nil {
		caps["downloadFile"] = map[string]any{}
	}
	return map[string]any{
		"protocolVersion":  ProtocolVersion,
		"hostInfo":         info,
		"hostCapabilities": caps,
		"hostContext":      hostCtx,
	}
}

// toolsList answers with the granted surface rather than the upstream's. A view
// told about a tool it may not call is invited to make a call that will refuse.
func (h *Host) toolsList() func() (any, *RPCError) {
	return func() (any, *RPCError) {
		tools := []mcpclient.Tool{}
		if h.Policy != nil {
			tools = append(tools, h.Policy.Tools()...)
		}
		out := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			entry := map[string]any{"name": t.Name}
			putIf(entry, "title", t.Title)
			putIf(entry, "description", t.Description)
			if t.InputSchema != nil {
				entry["inputSchema"] = t.InputSchema
			} else {
				entry["inputSchema"] = map[string]any{"type": "object"}
			}
			if t.OutputSchema != nil {
				entry["outputSchema"] = t.OutputSchema
			}
			if t.Meta != nil {
				entry["_meta"] = t.Meta
			}
			out = append(out, entry)
		}
		return map[string]any{"tools": out}, nil
	}
}

// putIf sets a key only when the value is non-empty, so an absent optional
// field stays absent rather than arriving as "".
func putIf(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}

// callParams is the tools/call params a view sends. _meta rides along and is
// read only for progressToken, which this host does not yet honour.
type callParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Meta      struct {
		ProgressToken any `json:"progressToken"`
	} `json:"_meta"`
}

// toolsCall gates one view-initiated call and forwards what policy permits.
func (h *Host) toolsCall(ctx context.Context, raw json.RawMessage) func() (any, *RPCError) {
	return func() (any, *RPCError) {
		var p callParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "tools/call params are not an object: " + err.Error()}
		}
		if p.Name == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "tools/call needs a tool name"}
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		if rpcErr := h.permitCall(p); rpcErr != nil {
			return nil, rpcErr
		}
		if h.Session == nil {
			return nil, &RPCError{Code: CodeUpstreamFailed, Message: "this host has no upstream session"}
		}
		upstream := h.mintToken(p.Meta.ProgressToken)
		defer h.releaseToken(upstream)
		res, err := h.Session.Call(ctx, mcpclient.Call{
			Name: p.Name, Arguments: p.Arguments, ProgressToken: upstream,
		})
		if err != nil {
			return nil, &RPCError{Code: CodeUpstreamFailed, Message: err.Error()}
		}
		return callResult(res), nil
	}
}

// mintToken records the view's progress token under one this host owns, so two
// views cannot collide on a token either one chose. See docs/mcpapps.md.
func (h *Host) mintToken(viewToken any) any {
	if viewToken == nil || h.Emit == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tokens == nil {
		h.tokens = map[string]any{}
	}
	h.nextID++
	mine := fmt.Sprintf("umbra-%d", h.nextID)
	h.tokens[mine] = viewToken
	return mine
}

// releaseToken drops the correlation once its call has returned.
func (h *Host) releaseToken(mine any) {
	key, ok := mine.(string)
	if !ok {
		return
	}
	h.mu.Lock()
	delete(h.tokens, key)
	h.mu.Unlock()
}

// HandleProgress forwards one upstream progress notification to the view under
// its own token. Wire it as mcpclient.Options.OnProgress.
func (h *Host) HandleProgress(p mcpclient.Progress) {
	key, ok := p.Token.(string)
	if !ok || h.Emit == nil {
		return
	}
	h.mu.Lock()
	viewToken, known := h.tokens[key]
	h.mu.Unlock()
	if !known {
		// A token this host did not mint belongs to no view in flight, so
		// correlating it would report progress on the wrong call.
		return
	}
	params := map[string]any{"progressToken": viewToken, "progress": p.Progress}
	if p.Total != 0 {
		params["total"] = p.Total
	}
	if p.Message != "" {
		params["message"] = p.Message
	}
	h.Emit(Notify(MethodProgress, params))
}

// permitCall runs the two policy gates in the order the refusal should read:
// an ungranted name is not a guarded argument.
func (h *Host) permitCall(p callParams) *RPCError {
	if h.Policy == nil {
		return &RPCError{
			Code:    CodePolicyDenied,
			Message: fmt.Sprintf("policy_denied: this view declares no `widget` block, so tool %q is not callable from it", p.Name),
			Data:    map[string]any{"tool": p.Name, "reason": "no_widget_policy"},
		}
	}
	if !granted(h.Policy.Tools(), p.Name) {
		// The policy's own sentence is the precise one, so it is asked rather
		// than told. Membership stays the floor if it answers nil anyway.
		reason := fmt.Sprintf("tool %q is not granted to this view", p.Name)
		if err := h.Policy.CheckToolCall(p.Name, p.Arguments); err != nil {
			reason = err.Error()
		}
		return &RPCError{
			Code:    CodePolicyDenied,
			Message: "policy_denied: " + reason,
			Data:    map[string]any{"tool": p.Name, "reason": "not_granted"},
		}
	}
	if err := h.Policy.CheckToolCall(p.Name, p.Arguments); err != nil {
		return &RPCError{
			Code:    CodePolicyDenied,
			Message: "policy_denied: " + err.Error(),
			Data:    map[string]any{"tool": p.Name, "reason": "guard"},
		}
	}
	return nil
}

// granted reports whether the policy's surface holds this tool name.
func granted(tools []mcpclient.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// callResult renders one upstream result as the CallToolResult a view expects.
// The view's SDK reads `content`, so a structured-only result still gets one.
func callResult(res mcpclient.Result) map[string]any {
	out := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(res.Raw)}},
		"isError": res.IsError,
	}
	if m, ok := res.Decoded.(map[string]any); ok {
		out["structuredContent"] = m
	}
	return out
}

// readParams is the resources/read params a view sends.
type readParams struct {
	URI string `json:"uri"`
}

// resourcesRead gates one view-initiated read and forwards what policy permits.
func (h *Host) resourcesRead(ctx context.Context, raw json.RawMessage) func() (any, *RPCError) {
	return func() (any, *RPCError) {
		var p readParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "resources/read params are not an object: " + err.Error()}
		}
		if p.URI == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "resources/read needs a uri"}
		}
		if h.Policy == nil {
			return nil, &RPCError{
				Code:    CodePolicyDenied,
				Message: fmt.Sprintf("policy_denied: this view declares no `widget` block, so resource %q is not readable from it", p.URI),
				Data:    map[string]any{"uri": p.URI, "reason": "no_widget_policy"},
			}
		}
		if err := h.Policy.CheckResourceRead(p.URI); err != nil {
			return nil, &RPCError{
				Code:    CodePolicyDenied,
				Message: "policy_denied: " + err.Error(),
				Data:    map[string]any{"uri": p.URI, "reason": "guard"},
			}
		}
		if h.Session == nil {
			return nil, &RPCError{Code: CodeUpstreamFailed, Message: "this host has no upstream session"}
		}
		res, err := h.Session.ReadResource(ctx, p.URI)
		if err != nil {
			return nil, &RPCError{Code: CodeUpstreamFailed, Message: err.Error()}
		}
		return res, nil
	}
}

// resourcesList answers with the upstream listing the view may actually read,
// so an enumeration and a read agree rather than advertising a refusal.
func (h *Host) resourcesList(ctx context.Context) func() (any, *RPCError) {
	return func() (any, *RPCError) {
		if h.Policy == nil {
			return map[string]any{"resources": []any{}}, nil
		}
		if h.Session == nil {
			return nil, &RPCError{Code: CodeUpstreamFailed, Message: "this host has no upstream session"}
		}
		all, err := h.Session.ListResources(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeUpstreamFailed, Message: err.Error()}
		}
		out := make([]mcpclient.Resource, 0, len(all))
		for _, r := range all {
			if h.Policy.CheckResourceRead(r.URI) == nil {
				out = append(out, r)
			}
		}
		return map[string]any{"resources": out}, nil
	}
}

// linkParams is the ui/open-link params a view sends.
type linkParams struct {
	URL string `json:"url"`
}

// openLink gates one URL and hands it to the wired opener.
func (h *Host) openLink(ctx context.Context, raw json.RawMessage) func() (any, *RPCError) {
	return func() (any, *RPCError) {
		if h.OpenLink == nil {
			return nil, &RPCError{Code: CodeMethodNotFound, Message: "not implemented: " + MethodOpenLink}
		}
		var p linkParams
		if err := json.Unmarshal(raw, &p); err != nil || p.URL == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: MethodOpenLink + " needs a url"}
		}
		if rpcErr := h.gate(func(pol Policy) error { return pol.CheckOpenLink(p.URL) },
			map[string]any{"url": p.URL}); rpcErr != nil {
			return nil, rpcErr
		}
		if err := h.OpenLink(ctx, p.URL); err != nil {
			// isError is the spec's own failure channel here, and the view
			// renders it rather than treating the frame as unanswered.
			return map[string]any{"isError": true}, nil
		}
		return map[string]any{"isError": false}, nil
	}
}

// downloadParams is the ui/download-file params a view sends.
type downloadParams struct {
	Contents []json.RawMessage `json:"contents"`
}

// downloadFile gates every item's URI and hands the set to the wired saver.
func (h *Host) downloadFile(ctx context.Context, raw json.RawMessage) func() (any, *RPCError) {
	return func() (any, *RPCError) {
		if h.SaveFile == nil {
			return nil, &RPCError{Code: CodeMethodNotFound, Message: "not implemented: " + MethodDownloadFile}
		}
		var p downloadParams
		if err := json.Unmarshal(raw, &p); err != nil || len(p.Contents) == 0 {
			return nil, &RPCError{Code: CodeInvalidParams, Message: MethodDownloadFile + " needs a non-empty contents list"}
		}
		items := make([]DownloadItem, 0, len(p.Contents))
		for _, c := range p.Contents {
			item := decodeDownloadItem(c)
			// Every item is gated, so one permitted entry does not carry an
			// unpermitted one alongside it.
			if rpcErr := h.gate(func(pol Policy) error { return pol.CheckSaveFile(item.URI) },
				map[string]any{"uri": item.URI}); rpcErr != nil {
				return nil, rpcErr
			}
			items = append(items, item)
		}
		if err := h.SaveFile(ctx, items); err != nil {
			return map[string]any{"isError": true}, nil
		}
		return map[string]any{"isError": false}, nil
	}
}

// decodeDownloadItem reads the URI and mime type off either content shape: a
// resource link carries them directly, an embedded resource one level in.
func decodeDownloadItem(raw json.RawMessage) DownloadItem {
	var shape struct {
		URI      string `json:"uri"`
		MIMEType string `json:"mimeType"`
		Resource struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
		} `json:"resource"`
	}
	_ = json.Unmarshal(raw, &shape)
	item := DownloadItem{URI: shape.URI, MIMEType: shape.MIMEType, Raw: raw}
	if item.URI == "" {
		item.URI, item.MIMEType = shape.Resource.URI, shape.Resource.MIMEType
	}
	return item
}

// gate runs one policy check and renders its refusal, so the outward verbs
// refuse in the same shape and with the same code a tool call does.
func (h *Host) gate(check func(Policy) error, data map[string]any) *RPCError {
	if h.Policy == nil {
		return denied("this host has no widget policy, so the view reaches nothing", data)
	}
	if err := check(h.Policy); err != nil {
		return denied(err.Error(), data)
	}
	return nil
}

// denied builds one policy refusal, tagging what the view's own data carries.
func denied(reason string, data map[string]any) *RPCError {
	data["reason"] = "guard"
	return &RPCError{Code: CodePolicyDenied, Message: "policy_denied: " + reason, Data: data}
}

// WidgetURI returns the `ui://` resource a tool's view lives at, or "". It
// reads both `_meta` spellings a server may emit. See docs/mcpverb.md.
func WidgetURI(t mcpclient.Tool) string {
	if t.Meta == nil {
		return ""
	}
	if ui, ok := t.Meta["ui"].(map[string]any); ok {
		if uri, ok := ui["resourceUri"].(string); ok {
			return uri
		}
	}
	if uri, ok := t.Meta["ui/resourceUri"].(string); ok {
		return uri
	}
	return ""
}
