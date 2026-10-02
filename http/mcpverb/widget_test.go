package mcpverb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coilyco/umbra/pkg/mcpapps"
	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// widgetTools stands in for the committed lock: a tool carrying an MCP Apps
// view, the tool that view polls, and one it must not reach.
func widgetTools() []mcpclient.Tool {
	return []mcpclient.Tool{
		{
			Name: "get_system_info",
			Meta: map[string]any{"ui": map[string]any{"resourceUri": "ui://monitor/view.html"}},
		},
		{
			Name: "poll_system_stats",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"scope": map[string]any{"type": "string"}},
			},
		},
		{Name: "poll_system_stats_slow", InputSchema: map[string]any{"type": "object"}},
		{Name: "delete_everything", InputSchema: map[string]any{"type": "object"}},
	}
}

// gate parses one policy and resolves the view of get_system_info.
func gate(t *testing.T, src string) *WidgetGate {
	t.Helper()
	gf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pol, err := WidgetPolicy(Config{Guardfile: gf, Tools: widgetTools()}, "get_system_info")
	if err != nil {
		t.Fatalf("WidgetPolicy: %v", err)
	}
	return pol
}

const widgetPolicy = `wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget {
            can call poll_system_stats {
                deny scope matches "^secret"
            }
            never call delete_everything
            can read "^ui://"
            can open "^https://"
            never open "^https://evil\\."
            can save "^ui://"
        }
    }
    can call poll_system_stats
    can call delete_everything
}`

func TestWidgetPolicy_AbsentBlockGatesNothingIn(t *testing.T) {
	// A grant with no widget block still yields a gate, and that gate permits
	// nothing: the view renders and reaches no upstream.
	pol := gate(t, `wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info
}`)
	if got := pol.Tools(); len(got) != 0 {
		t.Errorf("surface = %v, want empty for an undeclared widget", toolNames(got))
	}
	err := pol.CheckToolCall("poll_system_stats", map[string]any{})
	if err == nil {
		t.Fatal("an undeclared widget permitted a call")
	}
	if !strings.Contains(err.Error(), "declares no `widget` block") {
		t.Errorf("error = %q, want the missing block named", err)
	}
	if err := pol.CheckResourceRead("ui://monitor/view.html"); err == nil {
		t.Fatal("an undeclared widget permitted a read")
	}
}

func TestWidgetPolicy_SurfaceIsTheDeclaredCallsOnly(t *testing.T) {
	pol := gate(t, widgetPolicy)
	tools := pol.Tools()
	if len(tools) != 1 || tools[0].Name != "poll_system_stats" {
		t.Fatalf("surface = %v, want only poll_system_stats", toolNames(tools))
	}
}

func TestWidgetPolicy_UngrantedToolIsRefusedEvenThoughTheCLIGrantsIt(t *testing.T) {
	// delete_everything is a CLI leaf in this guardfile. The view still cannot
	// call it: the widget block is its own surface, not an inherited one.
	pol := gate(t, widgetPolicy)
	if err := pol.CheckToolCall("delete_everything", map[string]any{}); err == nil {
		t.Fatal("CheckToolCall permitted a tool the widget block never granted")
	}
}

func TestWidgetPolicy_GuardRefusesTheArgumentItNames(t *testing.T) {
	pol := gate(t, widgetPolicy)
	if err := pol.CheckToolCall("poll_system_stats", map[string]any{"scope": "cpu"}); err != nil {
		t.Fatalf("permitted argument refused: %v", err)
	}
	err := pol.CheckToolCall("poll_system_stats", map[string]any{"scope": "secret-thing"})
	if err == nil {
		t.Fatal("CheckToolCall permitted an argument the deny guard names")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("error = %q, want the guarded field named", err)
	}
}

func TestWidgetPolicy_ReadsAreDenyByAbsence(t *testing.T) {
	pol := gate(t, widgetPolicy)
	if err := pol.CheckResourceRead("ui://monitor/view.html"); err != nil {
		t.Fatalf("declared read refused: %v", err)
	}
	err := pol.CheckResourceRead("file:///etc/passwd")
	if err == nil {
		t.Fatal("CheckResourceRead permitted a URI no `can read` matches")
	}
	if !strings.Contains(err.Error(), "can read") {
		t.Errorf("error = %q, want the missing sentence named", err)
	}
}

func TestWidgetPolicy_OpenAndSaveAreTheirOwnGrants(t *testing.T) {
	// Being able to read a URI does not make it openable, and neither makes it
	// savable. Three verbs, three sentences.
	pol := gate(t, widgetPolicy)
	if err := pol.CheckOpenLink("https://example.com"); err != nil {
		t.Errorf("declared open refused: %v", err)
	}
	if err := pol.CheckOpenLink("https://evil.example.com"); err == nil {
		t.Error("a `never open` match was permitted")
	}
	if err := pol.CheckOpenLink("http://example.com"); err == nil {
		t.Error("plain http was permitted by a `can open \"^https://\"` sentence")
	}
	if err := pol.CheckSaveFile("ui://report.csv"); err != nil {
		t.Errorf("declared save refused: %v", err)
	}
	err := pol.CheckSaveFile("file:///etc/passwd")
	if err == nil {
		t.Fatal("a URI no `can save` matches was permitted")
	}
	if !strings.Contains(err.Error(), "can save") {
		t.Errorf("error = %q, want the missing sentence named", err)
	}
}

func TestWidgetPolicy_NoOpenOrSaveSentenceGrantsNeither(t *testing.T) {
	pol := gate(t, `wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget { can read "^ui://" }
    }
}`)
	if err := pol.CheckOpenLink("https://example.com"); err == nil {
		t.Error("a widget with no `can open` still opened a link")
	}
	if err := pol.CheckSaveFile("ui://report.csv"); err == nil {
		t.Error("a widget with no `can save` still saved a file")
	}
}

func TestParse_WidgetRefusesAnUnknownVerb(t *testing.T) {
	_, err := Parse([]byte(`wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget { can delete "^ui://" }
    }
}`))
	if err == nil {
		t.Fatal("Parse accepted an unknown widget sentence")
	}
	if !strings.Contains(err.Error(), "call | read | open | save") {
		t.Errorf("error = %q, want the grammar listed", err)
	}
}

func TestWidgetPolicy_NoReadSentenceReadsNothing(t *testing.T) {
	pol := gate(t, `wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget { can call poll_system_stats }
    }
}`)
	if err := pol.CheckResourceRead("ui://monitor/view.html"); err == nil {
		t.Fatal("a widget with no `can read` still read a resource")
	}
}

func TestParse_WidgetRefusesTheWildcard(t *testing.T) {
	// `can call *` inside a widget is the unconditional forwarding this block
	// replaces, so it is a parse error rather than a very wide grant.
	_, err := Parse([]byte(`wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget { can call * }
    }
}`))
	if err == nil {
		t.Fatal("Parse accepted `can call *` inside a widget block")
	}
	if !strings.Contains(err.Error(), "widget") {
		t.Errorf("error = %q, want the widget block named", err)
	}
}

func TestWidgetPolicy_UnlockedViewCallFailsTheBuild(t *testing.T) {
	gf, err := Parse([]byte(`wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget { can call not_in_the_lock }
    }
}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := WidgetPolicy(Config{Guardfile: gf, Tools: widgetTools()}, "get_system_info"); err == nil {
		t.Fatal("WidgetPolicy accepted a view call absent from the lock")
	}
}

func TestWidgetPolicy_GuardOnAnAbsentArgumentFailsTheBuild(t *testing.T) {
	// A selector the called tool does not take matches nothing at run time and
	// reads like a guard that passed, so it is caught here.
	gf, err := Parse([]byte(`wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget {
            can call poll_system_stats {
                deny nosuchfield matches "^x"
            }
        }
    }
}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = WidgetPolicy(Config{Guardfile: gf, Tools: widgetTools()}, "get_system_info")
	if err == nil {
		t.Fatal("WidgetPolicy accepted a guard naming an argument the tool does not take")
	}
	if !strings.Contains(err.Error(), "nosuchfield") {
		t.Errorf("error = %q, want the bad selector named", err)
	}
}

func TestWidgetPolicy_ContradictionIsRefusedRatherThanResolved(t *testing.T) {
	gf, err := Parse([]byte(`wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget {
            can call poll_system_stats
            never call poll_system_stats
        }
    }
}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := WidgetPolicy(Config{Guardfile: gf, Tools: widgetTools()}, "get_system_info"); err == nil {
		t.Fatal("WidgetPolicy resolved a tool that is both granted and denied")
	}
}

// serveMonitor starts an upstream carrying the widget fixture's tools, so a
// test drives the real protocol rather than a fake session.
func serveMonitor(t *testing.T, held <-chan struct{}) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "monitor", Version: "v0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "poll_system_stats",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"scope": map[string]any{"type": "string"}}},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Scope string `json:"scope"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{"uptime": "13h 38m", "scope": args.Scope},
		}, nil
	})
	srv.AddTool(&mcp.Tool{Name: "poll_system_stats_slow", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if token := req.Params.GetProgressToken(); token != nil {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: token, Progress: 3, Total: 10, Message: "reading",
				})
			}
			// The SDK dispatches a notification on its own goroutine, so a tool
			// that returned at once would race the frame it just sent.
			if held != nil {
				select {
				case <-held:
				case <-ctx.Done():
				}
			}
			return &mcp.CallToolResult{StructuredContent: map[string]any{"uptime": "13h 38m"}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "delete_everything", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Error("delete_everything ran; the guardfile denies it to the view")
			return &mcp.CallToolResult{}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// TestHost_LiveSessionThroughMcpclient is the acceptance path: a granted view
// call reaches a live upstream, and an ungranted one is refused and never runs.
func TestHost_LiveSessionThroughMcpclient(t *testing.T) {
	ctx := context.Background()
	url := serveMonitor(t, nil)
	sess, err := mcpclient.Connect(ctx, mcpclient.Server{Name: "monitor", HTTP: &mcpclient.HTTPEndpoint{URL: url}})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = sess.Close() }()

	pol := gate(t, widgetPolicy)
	host := &mcpapps.Host{
		Info:    mcpapps.Implementation{Name: "umbra-test-host", Version: "v0"},
		Session: sess,
		Policy:  pol,
	}

	permitted := host.Handle(ctx, callFrame(t, 1, "poll_system_stats", map[string]any{"scope": "cpu"}))
	if len(permitted) != 1 {
		t.Fatalf("got %d replies, want 1", len(permitted))
	}
	if permitted[0].IsError() {
		t.Fatalf("permitted call refused: %s", mustJSON(t, permitted[0]))
	}
	var got struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mustJSON(t, permitted[0]), &got); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if got.Result.StructuredContent["uptime"] != "13h 38m" {
		t.Errorf("structuredContent = %#v, want the live upstream's value", got.Result.StructuredContent)
	}

	refused := host.Handle(ctx, callFrame(t, 2, "delete_everything", map[string]any{}))
	if len(refused) != 1 || !refused[0].IsError() {
		t.Fatalf("refused = %s, want one error reply", mustJSON(t, refused[0]))
	}
	if !strings.Contains(string(mustJSON(t, refused[0])), "delete_everything") {
		t.Errorf("refusal = %s, want the refused tool named", mustJSON(t, refused[0]))
	}
}

// TestHost_ProgressReachesTheViewFromALiveUpstream is the second acceptance
// path: a token the view chose comes back on a notification it can correlate.
func TestHost_ProgressReachesTheViewFromALiveUpstream(t *testing.T) {
	ctx := context.Background()
	// The tool holds its result until the notification has reached the view, so
	// progress is observed mid-call rather than racing the reply.
	seen := make(chan struct{})
	url := serveMonitor(t, seen)

	host := &mcpapps.Host{Info: mcpapps.Implementation{Name: "umbra-test-host", Version: "v0"}}
	var mu sync.Mutex
	var emitted []mcpapps.Reply
	host.Emit = func(r mcpapps.Reply) {
		mu.Lock()
		emitted = append(emitted, r)
		mu.Unlock()
		close(seen)
	}
	sess, err := mcpclient.ConnectWith(ctx,
		mcpclient.Server{Name: "monitor", HTTP: &mcpclient.HTTPEndpoint{URL: url}},
		mcpclient.Options{OnProgress: host.HandleProgress})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = sess.Close() }()
	host.Session = sess
	host.Policy = gate(t, `wrap example ops monitor {
    mcp stdio { command "monitor-server" }
    can call get_system_info {
        widget { can call poll_system_stats_slow }
    }
}`)

	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": mcpapps.MethodToolsCall,
		"params": map[string]any{
			"name": "poll_system_stats_slow", "arguments": map[string]any{},
			"_meta": map[string]any{"progressToken": "view-7"},
		},
	})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	replies := host.Handle(ctx, raw)
	if len(replies) != 1 || replies[0].IsError() {
		t.Fatalf("call failed: %s", mustJSON(t, replies[0]))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(emitted) != 1 {
		t.Fatalf("emitted %d frames, want 1 progress notification", len(emitted))
	}
	var got struct {
		Method string `json:"method"`
		Params struct {
			ProgressToken string  `json:"progressToken"`
			Progress      float64 `json:"progress"`
			Message       string  `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(mustJSON(t, emitted[0]), &got); err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if got.Method != mcpapps.MethodProgress {
		t.Errorf("method = %q, want %s", got.Method, mcpapps.MethodProgress)
	}
	if got.Params.ProgressToken != "view-7" {
		t.Errorf("progressToken = %q, want the view's own view-7", got.Params.ProgressToken)
	}
	if got.Params.Progress != 3 || got.Params.Message != "reading" {
		t.Errorf("params = %#v, want the upstream's own values", got.Params)
	}
}

// callFrame builds one view-initiated tools/call frame.
func callFrame(t *testing.T, id int, tool string, args map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": mcpapps.MethodToolsCall,
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return b
}

// mustJSON renders one reply as the bytes a view would receive.
func mustJSON(t *testing.T, r mcpapps.Reply) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	return b
}

// toolNames lists a surface for an error message.
func toolNames(tools []mcpclient.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
