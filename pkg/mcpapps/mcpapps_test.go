package mcpapps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakePolicy is a hand-built Policy, so a bridge test does not need a guardfile.
type fakePolicy struct {
	tools    []mcpclient.Tool
	callErr  error
	readErr  error
	openErr  error
	saveErr  error
	readOnly string // when set, only this URI passes CheckResourceRead
	callSeen string
}

func (p *fakePolicy) Tools() []mcpclient.Tool { return p.tools }

func (p *fakePolicy) CheckToolCall(tool string, _ map[string]any) error {
	p.callSeen = tool
	return p.callErr
}

func (p *fakePolicy) CheckResourceRead(uri string) error {
	if p.readOnly != "" && uri != p.readOnly {
		return errString("not readable: " + uri)
	}
	return p.readErr
}

func (p *fakePolicy) CheckOpenLink(string) error { return p.openErr }

func (p *fakePolicy) CheckSaveFile(string) error { return p.saveErr }

// fakeSession records what reached the upstream, which is how a test tells a
// refusal that stopped a call from one that merely returned an error.
type fakeSession struct {
	calls     []string
	reads     []string
	resources []mcpclient.Resource
	result    mcpclient.Result
	err       error

	// duringCall runs inside Call, so a test drives a progress notification
	// arriving while the request it belongs to is still in flight.
	duringCall func(c mcpclient.Call)
}

func (s *fakeSession) Call(_ context.Context, c mcpclient.Call) (mcpclient.Result, error) {
	s.calls = append(s.calls, c.Name)
	if s.duringCall != nil {
		s.duringCall(c)
	}
	return s.result, s.err
}

func (s *fakeSession) ReadResource(_ context.Context, uri string) (*mcp.ReadResourceResult, error) {
	s.reads = append(s.reads, uri)
	return &mcp.ReadResourceResult{}, s.err
}

func (s *fakeSession) ListResources(context.Context) ([]mcpclient.Resource, error) {
	return s.resources, s.err
}

// frame builds one inbound view frame.
func frame(t *testing.T, id any, method string, params any) []byte {
	t.Helper()
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		m["id"] = id
	}
	if params != nil {
		m["params"] = params
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return b
}

// decode renders one reply as the map a view would receive.
func decode(t *testing.T, r Reply) map[string]any {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	return m
}

func TestInitialize_CarriesTheFieldsThatFailSilently(t *testing.T) {
	// Omitting hostCapabilities, hostContext, or serverTools each fails
	// silently in its own way, so assert all three. See docs/mcpapps.md.
	h := &Host{Info: Implementation{Name: "umbra-host", Version: "v0"}}

	replies := h.Handle(context.Background(), frame(t, 0, MethodInitialize, map[string]any{}))
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	got := decode(t, replies[0])
	res, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want an object", got["result"])
	}
	if res["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], ProtocolVersion)
	}
	caps, ok := res["hostCapabilities"].(map[string]any)
	if !ok {
		t.Fatalf("hostCapabilities = %#v, want an object", res["hostCapabilities"])
	}
	if _, ok := caps["serverTools"]; !ok {
		t.Error("hostCapabilities has no serverTools; a view without it never sends tools/call")
	}
	if _, ok := res["hostContext"]; !ok {
		t.Error("result has no hostContext; the view's SDK never becomes ready without it")
	}
	for _, unwired := range []string{"openLinks", "downloadFile"} {
		if _, ok := caps[unwired]; ok {
			t.Errorf("hostCapabilities declares %s with no handler wired", unwired)
		}
	}
}

func TestInitialize_DeclaresOnlyTheOutwardVerbsThatAreWired(t *testing.T) {
	h := &Host{
		OpenLink: func(context.Context, string) error { return nil },
		SaveFile: func(context.Context, []DownloadItem) error { return nil },
	}

	replies := h.Handle(context.Background(), frame(t, 0, MethodInitialize, map[string]any{}))
	caps := decode(t, replies[0])["result"].(map[string]any)["hostCapabilities"].(map[string]any)
	for _, wired := range []string{"openLinks", "downloadFile"} {
		if _, ok := caps[wired]; !ok {
			t.Errorf("hostCapabilities omits %s with a handler wired", wired)
		}
	}
}

func TestReply_SendsResultOrError_NeverBoth(t *testing.T) {
	// A present `error` member beside a valid result reads to the view as a
	// failure, and structured clone keeps it. Assert the wire, not the type.
	res := decode(t, Result(json.RawMessage("1"), map[string]any{"ok": true}))
	if _, present := res["error"]; present {
		t.Errorf("success reply carries an error member: %#v", res)
	}
	fail := decode(t, Failure(json.RawMessage("1"), CodePolicyDenied, "refused", nil))
	if _, present := fail["result"]; present {
		t.Errorf("failure reply carries a result member: %#v", fail)
	}
}

func TestToolsCall_ReachesTheSessionWhenGranted(t *testing.T) {
	sess := &fakeSession{result: mcpclient.Result{
		Decoded: map[string]any{"uptime": "13h 38m"},
		Raw:     []byte(`{"uptime":"13h 38m"}`),
	}}
	h := &Host{
		Session: sess,
		Policy:  &fakePolicy{tools: []mcpclient.Tool{{Name: "poll-system-stats"}}},
	}

	replies := h.Handle(context.Background(), frame(t, 1, MethodToolsCall,
		map[string]any{"name": "poll-system-stats", "arguments": map[string]any{}}))
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	got := decode(t, replies[0])
	if _, failed := got["error"]; failed {
		t.Fatalf("granted call was refused: %#v", got)
	}
	if len(sess.calls) != 1 || sess.calls[0] != "poll-system-stats" {
		t.Fatalf("session calls = %v, want one poll-system-stats", sess.calls)
	}
	res := got["result"].(map[string]any)
	structured, ok := res["structuredContent"].(map[string]any)
	if !ok || structured["uptime"] != "13h 38m" {
		t.Errorf("structuredContent = %#v, want the upstream value", res["structuredContent"])
	}
}

func TestToolsCall_RefusedCallIsAnsweredRatherThanDropped(t *testing.T) {
	// The whole refusal contract: the view learns it was refused. A dropped
	// frame leaves it waiting on a reply that never arrives.
	sess := &fakeSession{}
	h := &Host{
		Session: sess,
		Policy:  &fakePolicy{tools: []mcpclient.Tool{{Name: "poll-system-stats"}}},
	}

	replies := h.Handle(context.Background(), frame(t, 7, MethodToolsCall,
		map[string]any{"name": "delete-everything", "arguments": map[string]any{}}))
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want exactly 1; a refused call must be answered", len(replies))
	}
	if !replies[0].IsError() {
		t.Fatalf("refusal was not an error reply: %#v", decode(t, replies[0]))
	}
	got := decode(t, replies[0])
	if got["id"] != float64(7) {
		t.Errorf("reply id = %v, want 7; a reply the view cannot correlate is a dropped frame", got["id"])
	}
	rpcErr := got["error"].(map[string]any)
	if rpcErr["code"] != float64(CodePolicyDenied) {
		t.Errorf("code = %v, want %d", rpcErr["code"], CodePolicyDenied)
	}
	if !strings.Contains(rpcErr["message"].(string), "delete-everything") {
		t.Errorf("message = %q, want the refused tool named", rpcErr["message"])
	}
	if len(sess.calls) != 0 {
		t.Errorf("session saw %v; a refused call must not reach the upstream", sess.calls)
	}
}

func TestToolsCall_NoPolicyForwardsNothing(t *testing.T) {
	// The spike this replaces forwarded every view-initiated call. A host with
	// no widget policy is the default, and it reaches no upstream.
	sess := &fakeSession{}
	h := &Host{Session: sess}

	replies := h.Handle(context.Background(), frame(t, 1, MethodToolsCall,
		map[string]any{"name": "poll-system-stats"}))
	if len(replies) != 1 || !replies[0].IsError() {
		t.Fatalf("replies = %#v, want one error", replies)
	}
	if len(sess.calls) != 0 {
		t.Errorf("session saw %v, want nothing forwarded without a policy", sess.calls)
	}
}

func TestToolsCall_GuardRefusalNamesTheRule(t *testing.T) {
	sess := &fakeSession{}
	pol := &fakePolicy{
		tools:   []mcpclient.Tool{{Name: "read-file"}},
		callErr: errString("argument path=\"/etc/shadow\" is outside the allowed scope"),
	}
	h := &Host{Session: sess, Policy: pol}

	replies := h.Handle(context.Background(), frame(t, 2, MethodToolsCall,
		map[string]any{"name": "read-file", "arguments": map[string]any{"path": "/etc/shadow"}}))
	if len(replies) != 1 || !replies[0].IsError() {
		t.Fatalf("replies = %#v, want one error", replies)
	}
	msg := decode(t, replies[0])["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "/etc/shadow") {
		t.Errorf("message = %q, want the guard's own reason", msg)
	}
	if len(sess.calls) != 0 {
		t.Errorf("session saw %v; a guarded refusal must not reach the upstream", sess.calls)
	}
}

func TestToolsList_AnswersFromTheGrantedSurface(t *testing.T) {
	// A view told about a tool it may not call is invited to make a call that
	// will refuse, so the list is the grant rather than the upstream.
	h := &Host{
		Session: &fakeSession{},
		Policy:  &fakePolicy{tools: []mcpclient.Tool{{Name: "poll-system-stats", Description: "stats"}}},
	}

	replies := h.Handle(context.Background(), frame(t, 3, MethodToolsList, map[string]any{}))
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	tools := decode(t, replies[0])["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	entry := tools[0].(map[string]any)
	if entry["name"] != "poll-system-stats" {
		t.Errorf("name = %v, want poll-system-stats", entry["name"])
	}
	if _, ok := entry["inputSchema"]; !ok {
		t.Error("entry has no inputSchema; a view cannot bind arguments without one")
	}
}

func TestInitialized_PushesTheInstantiatingResultAsAHostNotification(t *testing.T) {
	// ui/notifications/initialized is View to Host. The host answers it with a
	// notification carrying the instantiating result, and never replies to it.
	h := &Host{Instantiating: map[string]any{"content": []any{}}}

	replies := h.Handle(context.Background(), frame(t, nil, MethodInitialized, map[string]any{}))
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	if replies[0].Method() != MethodToolResult {
		t.Errorf("method = %q, want %s", replies[0].Method(), MethodToolResult)
	}
	got := decode(t, replies[0])
	if _, present := got["id"]; present {
		t.Errorf("notification carries an id: %#v", got)
	}
}

func TestNotification_GetsNoReply(t *testing.T) {
	// size-changed arrives unsolicited and answering it is a protocol error.
	h := &Host{Session: &fakeSession{}, Policy: &fakePolicy{}}
	if replies := h.Handle(context.Background(), frame(t, nil, MethodSizeChanged,
		map[string]any{"width": 737, "height": 558})); len(replies) != 0 {
		t.Errorf("got %d replies to a notification, want 0", len(replies))
	}
}

func TestUnknownRequest_IsAnsweredWithMethodNotFound(t *testing.T) {
	// ui/open-link is unimplemented and undeclared. A view asking for it learns
	// so, rather than waiting: the same reason a refusal is a reply.
	h := &Host{}
	replies := h.Handle(context.Background(), frame(t, 9, "ui/open-link", map[string]any{"url": "https://example.com"}))
	if len(replies) != 1 || !replies[0].IsError() {
		t.Fatalf("replies = %#v, want one error", replies)
	}
	code := decode(t, replies[0])["error"].(map[string]any)["code"]
	if code != float64(CodeMethodNotFound) {
		t.Errorf("code = %v, want %d", code, CodeMethodNotFound)
	}
}

func TestResourcesRead_RefusalStopsTheRead(t *testing.T) {
	sess := &fakeSession{}
	h := &Host{Session: sess, Policy: &fakePolicy{readErr: errString("resource is not readable by this view")}}

	replies := h.Handle(context.Background(), frame(t, 4, MethodResourcesRead, map[string]any{"uri": "file:///etc/passwd"}))
	if len(replies) != 1 || !replies[0].IsError() {
		t.Fatalf("replies = %#v, want one error", replies)
	}
	if len(sess.reads) != 0 {
		t.Errorf("session read %v; a refused read must not reach the upstream", sess.reads)
	}
}

func TestProgress_ReachesTheViewUnderItsOwnToken(t *testing.T) {
	// The view chose token 42. Upstream never sees it: the host mints its own
	// and maps back, so two views cannot collide on a token either one picked.
	var emitted []Reply
	h := &Host{Policy: &fakePolicy{tools: []mcpclient.Tool{{Name: "slow"}}}}
	h.Emit = func(r Reply) { emitted = append(emitted, r) }
	sess := &fakeSession{}
	sess.duringCall = func(c mcpclient.Call) {
		if c.ProgressToken == nil {
			t.Error("the upstream call carried no progress token")
			return
		}
		if c.ProgressToken == float64(42) || c.ProgressToken == 42 {
			t.Errorf("upstream token = %v, want one the host minted rather than the view's", c.ProgressToken)
		}
		h.HandleProgress(mcpclient.Progress{Token: c.ProgressToken, Progress: 3, Total: 10, Message: "reading"})
	}
	h.Session = sess

	h.Handle(context.Background(), frame(t, 1, MethodToolsCall, map[string]any{
		"name": "slow", "arguments": map[string]any{}, "_meta": map[string]any{"progressToken": 42},
	}))

	if len(emitted) != 1 {
		t.Fatalf("emitted %d frames, want 1 progress notification", len(emitted))
	}
	got := decode(t, emitted[0])
	if got["method"] != MethodProgress {
		t.Errorf("method = %v, want %s", got["method"], MethodProgress)
	}
	params := got["params"].(map[string]any)
	if params["progressToken"] != float64(42) {
		t.Errorf("progressToken = %v, want the view's own 42", params["progressToken"])
	}
	if params["progress"] != float64(3) || params["total"] != float64(10) {
		t.Errorf("params = %#v, want progress 3 of 10", params)
	}
}

func TestProgress_UnknownTokenIsNotForwarded(t *testing.T) {
	// A token this host did not mint belongs to no view in flight, and guessing
	// a correlation would report progress against the wrong call.
	var emitted []Reply
	h := &Host{Emit: func(r Reply) { emitted = append(emitted, r) }}
	h.HandleProgress(mcpclient.Progress{Token: "umbra-999", Progress: 1})
	if len(emitted) != 0 {
		t.Errorf("emitted %d frames for an unminted token, want 0", len(emitted))
	}
}

func TestProgress_NoTokenMeansNoUpstreamToken(t *testing.T) {
	// A view that asked for no progress must not have one invented for it.
	sess := &fakeSession{duringCall: func(c mcpclient.Call) {
		if c.ProgressToken != nil {
			t.Errorf("ProgressToken = %v, want nil when the view sent none", c.ProgressToken)
		}
	}}
	h := &Host{Session: sess, Emit: func(Reply) {}, Policy: &fakePolicy{tools: []mcpclient.Tool{{Name: "quick"}}}}
	h.Handle(context.Background(), frame(t, 1, MethodToolsCall, map[string]any{"name": "quick"}))
}

func TestOpenLink_UnwiredStaysMethodNotFound(t *testing.T) {
	// Undeclared in hostCapabilities and unanswerable, so a view learns rather
	// than waiting on a capability this host never had.
	h := &Host{Policy: &fakePolicy{}}
	replies := h.Handle(context.Background(), frame(t, 1, MethodOpenLink, map[string]any{"url": "https://example.com"}))
	if len(replies) != 1 || !replies[0].IsError() {
		t.Fatalf("replies = %#v, want one error", replies)
	}
	if code := decode(t, replies[0])["error"].(map[string]any)["code"]; code != float64(CodeMethodNotFound) {
		t.Errorf("code = %v, want %d", code, CodeMethodNotFound)
	}
}

func TestOpenLink_RefusedUrlNeverReachesTheOpener(t *testing.T) {
	var opened []string
	h := &Host{
		Policy:   &fakePolicy{openErr: errString(`"http://evil" is not openable by the view`)},
		OpenLink: func(_ context.Context, url string) error { opened = append(opened, url); return nil },
	}

	replies := h.Handle(context.Background(), frame(t, 1, MethodOpenLink, map[string]any{"url": "http://evil"}))
	if len(replies) != 1 || !replies[0].IsError() {
		t.Fatalf("replies = %#v, want one error", replies)
	}
	if code := decode(t, replies[0])["error"].(map[string]any)["code"]; code != float64(CodePolicyDenied) {
		t.Errorf("code = %v, want %d", code, CodePolicyDenied)
	}
	if len(opened) != 0 {
		t.Errorf("opener saw %v; a refused url must not reach it", opened)
	}
}

func TestOpenLink_PermittedUrlOpens(t *testing.T) {
	var opened []string
	h := &Host{
		Policy:   &fakePolicy{},
		OpenLink: func(_ context.Context, url string) error { opened = append(opened, url); return nil },
	}

	replies := h.Handle(context.Background(), frame(t, 1, MethodOpenLink, map[string]any{"url": "https://example.com"}))
	if len(replies) != 1 || replies[0].IsError() {
		t.Fatalf("permitted url refused: %#v", decode(t, replies[0]))
	}
	if res := decode(t, replies[0])["result"].(map[string]any); res["isError"] != false {
		t.Errorf("isError = %v, want false", res["isError"])
	}
	if len(opened) != 1 || opened[0] != "https://example.com" {
		t.Errorf("opener saw %v, want the one url", opened)
	}
}

func TestDownloadFile_GatesBothContentShapes(t *testing.T) {
	// A resource link carries its uri directly, an embedded resource one level
	// in. Both are gated, and one permitted item cannot carry a refused one.
	var saved [][]DownloadItem
	h := &Host{
		Policy:   &fakePolicy{saveErr: errString("not savable")},
		SaveFile: func(_ context.Context, items []DownloadItem) error { saved = append(saved, items); return nil },
	}
	for _, content := range []map[string]any{
		{"type": "resource_link", "uri": "file:///etc/passwd"},
		{"type": "resource", "resource": map[string]any{"uri": "file:///etc/shadow"}},
	} {
		replies := h.Handle(context.Background(), frame(t, 1, MethodDownloadFile,
			map[string]any{"contents": []any{content}}))
		if len(replies) != 1 || !replies[0].IsError() {
			t.Fatalf("content %v was not refused: %#v", content, replies)
		}
	}
	if len(saved) != 0 {
		t.Errorf("saver saw %v; a refused item must not reach it", saved)
	}
}

func TestDownloadFile_PermittedItemsReachTheSaver(t *testing.T) {
	var saved []DownloadItem
	h := &Host{
		Policy:   &fakePolicy{},
		SaveFile: func(_ context.Context, items []DownloadItem) error { saved = items; return nil },
	}

	replies := h.Handle(context.Background(), frame(t, 1, MethodDownloadFile, map[string]any{
		"contents": []any{map[string]any{
			"type": "resource", "resource": map[string]any{"uri": "ui://report.csv", "mimeType": "text/csv"},
		}},
	}))
	if len(replies) != 1 || replies[0].IsError() {
		t.Fatalf("permitted download refused: %#v", decode(t, replies[0]))
	}
	if len(saved) != 1 || saved[0].URI != "ui://report.csv" || saved[0].MIMEType != "text/csv" {
		t.Fatalf("saver saw %#v, want the embedded resource's own uri and mime type", saved)
	}
	if len(saved[0].Raw) == 0 {
		t.Error("item carries no Raw; a handler saving inline bytes has nothing to read")
	}
}

func TestResourcesList_ShowsOnlyWhatTheViewMayRead(t *testing.T) {
	// An enumeration and a read must agree: listing a URI the read would refuse
	// invites exactly the call that refuses.
	h := &Host{
		Session: &fakeSession{resources: []mcpclient.Resource{
			{URI: "ui://monitor/view.html"}, {URI: "file:///etc/passwd"},
		}},
		Policy: &fakePolicy{readOnly: "ui://monitor/view.html"},
	}

	replies := h.Handle(context.Background(), frame(t, 1, MethodResourcesList, map[string]any{}))
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	list := decode(t, replies[0])["result"].(map[string]any)["resources"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d resources, want 1", len(list))
	}
	if uri := list[0].(map[string]any)["uri"]; uri != "ui://monitor/view.html" {
		t.Errorf("uri = %v, want the readable one", uri)
	}
}

func TestWidgetURI_ReadsBothMetaSpellings(t *testing.T) {
	nested := mcpclient.Tool{Meta: map[string]any{"ui": map[string]any{"resourceUri": "ui://widget/a.html"}}}
	if got := WidgetURI(nested); got != "ui://widget/a.html" {
		t.Errorf("nested = %q, want ui://widget/a.html", got)
	}
	flat := mcpclient.Tool{Meta: map[string]any{"ui/resourceUri": "ui://widget/b.html"}}
	if got := WidgetURI(flat); got != "ui://widget/b.html" {
		t.Errorf("flat = %q, want ui://widget/b.html", got)
	}
	if got := WidgetURI(mcpclient.Tool{}); got != "" {
		t.Errorf("no meta = %q, want empty", got)
	}
}

// errString is a minimal error so a fake policy states its own refusal.
type errString string

func (e errString) Error() string { return string(e) }
