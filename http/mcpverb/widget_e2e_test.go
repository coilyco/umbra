package mcpverb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/mcpapps"
	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// e2ePolicy grants the view one call, one read pattern, one open and one save.
// wipe_disk is a CLI leaf the view still cannot reach.
const e2ePolicy = `wrap example ops monitor {
    mcp http { url "%s" }
    can call get_system_info {
        widget {
            can call poll_system_stats {
                deny scope matches "^secret"
            }
            can read "^ui://"
            can open "^https://"
            can save "^ui://"
        }
    }
    can call wipe_disk { destructive }
}`

// serveWidgetUpstream starts the real MCP server the session talks to: a tool
// carrying a view, one the view polls, and one it must not reach.
func serveWidgetUpstream(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "monitor", Version: "v0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "get_system_info",
		InputSchema: map[string]any{"type": "object"},
		Meta:        map[string]any{"ui": map[string]any{"resourceUri": "ui://monitor/view.html"}},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"host": "example.local"}}, nil
	})
	srv.AddTool(&mcp.Tool{
		Name:        "poll_system_stats",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"scope": map[string]any{"type": "string"}}},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"uptime": "13h"}}, nil
	})
	srv.AddTool(&mcp.Tool{Name: "wipe_disk", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Error("wipe_disk reached the upstream: the view's grant does not name it")
			return &mcp.CallToolResult{}, nil
		})
	for _, uri := range []string{"ui://monitor/view.html", "file:///etc/passwd"} {
		srv.AddResource(&mcp.Resource{URI: uri, Name: uri, MIMEType: "text/plain"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{}, nil
			})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// liveWidgetHost wires a Host onto a real session with the policy resolved from
// the upstream's own tool list rather than from a literal.
func liveWidgetHost(t *testing.T) (*mcpapps.Host, context.Context) {
	t.Helper()
	ctx := context.Background()
	url := serveWidgetUpstream(t)
	gf, err := Parse(fmt.Appendf(nil, e2ePolicy, url))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	host := &mcpapps.Host{Info: mcpapps.Implementation{Name: "test-host", Version: "v0"}}
	host.OpenLink = func(context.Context, string) error { return nil }
	host.SaveFile = func(context.Context, []mcpapps.DownloadItem) error { return nil }
	sess, err := mcpclient.ConnectWith(ctx,
		mcpclient.Server{Name: "monitor", HTTP: &mcpclient.HTTPEndpoint{URL: url}},
		mcpclient.Options{OnProgress: host.HandleProgress})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	tools, err := sess.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	pol, err := WidgetPolicy(Config{Guardfile: gf, Tools: tools}, "get_system_info")
	if err != nil {
		t.Fatalf("widget policy: %v", err)
	}
	host.Session, host.Policy = sess, pol
	return host, ctx
}

// send hands one frame to the host and returns the single reply, failing when a
// frame that owes a reply produced none.
func send(ctx context.Context, t *testing.T, host *mcpapps.Host, body map[string]any) mcpapps.Reply {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	replies := host.Handle(ctx, raw)
	if len(replies) == 0 {
		t.Fatalf("frame %v produced no reply", body["method"])
	}
	return replies[len(replies)-1]
}

// The per-frame behaviour is covered in pkg/mcpapps against a stub session.
// This is the sequence, in order, against an upstream that really answers.
func TestWidgetSession_ReplaysARealViewsFrameSequence(t *testing.T) {
	host, ctx := liveWidgetHost(t)

	if r := send(ctx, t, host, map[string]any{
		"jsonrpc": "2.0", "id": 0, "method": mcpapps.MethodInitialize,
		"params": map[string]any{"appInfo": map[string]any{"name": "Monitor", "version": "1.0.0"}},
	}); r.IsError() {
		t.Fatalf("initialize refused: %v", r)
	}

	// tools/list answers from the grant, not from the upstream: three tools are
	// served and the view may see one.
	listed := send(ctx, t, host, map[string]any{"jsonrpc": "2.0", "id": 1, "method": mcpapps.MethodToolsList})
	body, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("marshal tools/list: %v", err)
	}
	for _, absent := range []string{"wipe_disk", "get_system_info"} {
		if strings.Contains(string(body), absent) {
			t.Errorf("tools/list offered the view %q", absent)
		}
	}
	if !strings.Contains(string(body), "poll_system_stats") {
		t.Error("tools/list withheld the one tool the widget block grants")
	}

	for _, tc := range []struct {
		name    string
		frame   map[string]any
		refused bool
	}{
		{"granted call", map[string]any{"jsonrpc": "2.0", "id": 2, "method": mcpapps.MethodToolsCall,
			"params": map[string]any{"name": "poll_system_stats", "arguments": map[string]any{"scope": "cpu"}}}, false},
		{"guarded argument", map[string]any{"jsonrpc": "2.0", "id": 3, "method": mcpapps.MethodToolsCall,
			"params": map[string]any{"name": "poll_system_stats", "arguments": map[string]any{"scope": "secret-partition"}}}, true},
		{"tool the widget block does not grant", map[string]any{"jsonrpc": "2.0", "id": 4, "method": mcpapps.MethodToolsCall,
			"params": map[string]any{"name": "wipe_disk", "arguments": map[string]any{}}}, true},
		{"read outside the permitted pattern", map[string]any{"jsonrpc": "2.0", "id": 5, "method": mcpapps.MethodResourcesRead,
			"params": map[string]any{"uri": "file:///etc/passwd"}}, true},
		{"permitted open", map[string]any{"jsonrpc": "2.0", "id": 6, "method": mcpapps.MethodOpenLink,
			"params": map[string]any{"url": "https://example.com"}}, false},
		{"open outside the permitted scheme", map[string]any{"jsonrpc": "2.0", "id": 7, "method": mcpapps.MethodOpenLink,
			"params": map[string]any{"url": "http://example.com"}}, true},
		{"undeclared capability", map[string]any{"jsonrpc": "2.0", "id": 8, "method": "ui/request-display-mode",
			"params": map[string]any{"mode": "fullscreen"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := send(ctx, t, host, tc.frame)
			if got := r.IsError(); got != tc.refused {
				t.Errorf("refused = %v, want %v (reply: %v)", got, tc.refused, r)
			}
		})
	}
}
