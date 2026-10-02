package specverb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// TestBearerAuthLive proves the bearer scheme sends `Authorization: Bearer <secret>`
// against an OpenAPI 3.1 spec end to end.
func TestBearerAuthLive(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		auth bearer { value ssm "/tailscale/api-key" }
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"devices":[]}`))
	}))
	defer srv.Close()

	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "tskey-abc", nil }}}
	if _, err := runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--output", "json"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotAuth != "Bearer tskey-abc" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Bearer tskey-abc")
	}
}

// trelloFixture parses a query-param-auth Guardfile over the Trello 3.0 spec.
func trelloFixture(t *testing.T) (*guardfile.Guardfile, []byte) {
	t.Helper()
	spec := readSpec(t, "trello.openapi.json")
	gf, err := guardfile.Parse([]byte(`wrap ward ops trello {
		spec trello.openapi.json
		auth query-param {
			param key { value ssm "/trello/api-key" }
			param token { value ssm "/trello/api-token" }
		}
		can create cards { op "post-cards" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return gf, spec
}

// TestQueryParamAuthLive proves the dual-secret query-param scheme injects both
// secrets as query parameters (Trello's ?key=&token=).
func TestQueryParamAuthLive(t *testing.T) {
	gf, spec := trelloFixture(t)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	secrets := map[string]string{"/trello/api-key": "KEYVAL", "/trello/api-token": "TOKENVAL"}
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(_ context.Context, p string) (string, error) { return secrets[p], nil }}}
	if _, err := runTree(t, cfg, "trello", "cards", "create", "--name", "demo", "--idList", "abc", "--output", "json"); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"key=KEYVAL", "token=TOKENVAL", "name=demo", "idList=abc"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

// TestQueryParamAuthDryRunRedacts proves a dry-run shows the auth params with
// redacted values on the URL and never resolves a secret.
func TestQueryParamAuthDryRunRedacts(t *testing.T) {
	gf, spec := trelloFixture(t)
	cfg := Config{Guardfile: gf, Spec: spec, HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) {
			t.Fatal("dry-run must not resolve an auth secret")
			return "", nil
		}}}
	out, err := runTree(t, cfg, "trello", "cards", "create", "--name", "demo", "--idList", "abc", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{"key=", "token=", "redacted", "name=demo", "idList=abc"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "KEYVAL") || strings.Contains(out, "TOKENVAL") {
		t.Errorf("dry-run leaked a secret:\n%s", out)
	}
}

// TestBearerDescribeNamesScheme proves the describe surface names the bearer
// scheme and its SSM path without showing the token.
func TestBearerDescribeNamesScheme(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		auth bearer { value ssm "/tailscale/api-key" }
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if surface.Auth.Scheme != "bearer" || surface.Auth.Source != "ssm /tailscale/api-key" {
		t.Errorf("auth info = %+v", surface.Auth)
	}
}
