package opcore_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/valuesource"
)

const contactAgent = "coilyco-mcp/1.0 (by /u/coilysiren)"

func headerSrc(decl string) string {
	return `wrap ward mcp reddit {
    auth none
    ` + decl + `
    can list post {
        path "/r/{sub}/new.rss"
        raw-response
    }
}`
}

// fireWithConfig runs one real request against srv and returns the headers the
// upstream saw, which is the only proof that a declared header left the client.
func fireWithConfig(t *testing.T, src string) http.Header {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("<feed/>"))
	}))
	defer srv.Close()

	descs, cfg, err := opcore.ParseInline([]byte(src))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	cfg.BaseURL = srv.URL
	cfg.Providers = valuesource.Merge(nil)
	cfg.Client = srv.Client()

	op := opcore.Operation{RT: opcore.NewRuntime(cfg), Desc: descs[0]}
	if _, err := op.Execute(context.Background(), opcore.Args{Path: map[string]string{"sub": "golang"}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return got
}

// The acceptance for umbra#303: a guardfile-rendered request carries the
// descriptive agent an API asks for, contact address and all.
func TestWrapHeaderReachesTheWire(t *testing.T) {
	got := fireWithConfig(t, headerSrc(`header "User-Agent" "`+contactAgent+`"`))
	if ua := got.Get("User-Agent"); ua != contactAgent {
		t.Errorf("User-Agent = %q, want %q", ua, contactAgent)
	}
}

func TestDefaultUserAgentWhenNoHeaderDeclared(t *testing.T) {
	src := `wrap ward mcp reddit {
    auth none
    can list post {
        path "/r/{sub}/new.rss"
        raw-response
    }
}`
	got := fireWithConfig(t, src)
	if ua := got.Get("User-Agent"); ua != opcore.DefaultUserAgent {
		t.Errorf("User-Agent = %q, want the default %q", ua, opcore.DefaultUserAgent)
	}
}

func TestWrapHeaderCarriesArbitraryNames(t *testing.T) {
	got := fireWithConfig(t, headerSrc(`header "Accept" "application/atom+xml"`))
	if a := got.Get("Accept"); a != "application/atom+xml" {
		t.Errorf("Accept = %q, want the declared value", a)
	}
	// The default still fills the agent nobody declared.
	if ua := got.Get("User-Agent"); ua != opcore.DefaultUserAgent {
		t.Errorf("User-Agent = %q, want the default", ua)
	}
}

// Authorization stays off limits: `auth` owns it, and a second path to it would
// be an unreviewed credential surface.
func TestWrapHeaderRefusesReservedNames(t *testing.T) {
	cases := map[string]string{
		"authorization":        `header "Authorization" "Bearer nope"`,
		"authorization case":   `header "authorization" "Bearer nope"`,
		"content-type":         `header "Content-Type" "text/plain"`,
		"missing value":        `header "User-Agent"`,
		"empty value":          `header "User-Agent" ""`,
		"three args":           `header "User-Agent" "a" "b"`,
		"duplicate":            "header \"User-Agent\" \"a\"\n    header \"user-agent\" \"b\"",
		"duplicate exact case": "header \"Accept\" \"a\"\n    header \"Accept\" \"b\"",
	}
	for name, decl := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := opcore.ParseInline([]byte(headerSrc(decl)))
			if err == nil {
				t.Fatal("want a parse error, got nil")
			}
			if name == "authorization" && !strings.Contains(err.Error(), "auth") {
				t.Errorf("refusal should name auth as the owner: %v", err)
			}
		})
	}
}
