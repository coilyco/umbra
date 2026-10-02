package specverb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// baseURLValueFixture parses a bearer guardfile whose host comes from a value
// provider (no committed base-url), over the tailscale 3.1 spec.
func baseURLValueFixture(t *testing.T) (*guardfile.Guardfile, []byte) {
	t.Helper()
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		base-url { value ssm "/coilysiren/open-webui/url" }
		auth bearer { value ssm "/tailscale/api-key" }
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wantChain := guardfile.ValueChain{{Provider: "ssm", Address: "/coilysiren/open-webui/url"}}
	if gf.BaseURL != "" || !reflect.DeepEqual(gf.BaseURLValue, wantChain) {
		t.Fatalf("base-url parse: BaseURL=%q BaseURLValue=%+v", gf.BaseURL, gf.BaseURLValue)
	}
	return gf, spec
}

// TestBaseURLFromValueLive proves a real request resolves the host through the
// value provider and that the host resolver is consulted exactly once (cached).
func TestBaseURLFromValueLive(t *testing.T) {
	gf, spec := baseURLValueFixture(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"devices":[]}`))
	}))
	defer srv.Close()

	var resolvedHost int
	cfg := Config{Guardfile: gf, Spec: spec,
		Providers: map[string]Provider{"ssm": func(_ context.Context, addr string) (string, error) {
			if addr == "/coilysiren/open-webui/url" {
				resolvedHost++
				return srv.URL, nil
			}
			return "tskey-abc", nil
		}},
	}
	if _, err := runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--output", "json"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (base-url did not resolve to the live host)", hits)
	}
	if resolvedHost != 1 {
		t.Errorf("host resolver calls = %d, want 1 (lazy, cached once)", resolvedHost)
	}
}

// TestBaseURLFromValueDryRunOffline proves --dry-run never resolves the host
// (offline) and shows the value source symbolically.
func TestBaseURLFromValueDryRunOffline(t *testing.T) {
	gf, spec := baseURLValueFixture(t)
	cfg := Config{Guardfile: gf, Spec: spec,
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) {
			t.Fatal("dry-run must not resolve a value")
			return "", nil
		}},
	}
	out, err := runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "base-url:ssm /coilysiren/open-webui/url") {
		t.Errorf("dry-run preview missing the symbolic base marker:\n%s", out)
	}
}

// TestBaseURLValueDescribeNamesSource proves the describe surface names the value
// source for the host rather than resolving it.
func TestBaseURLValueDescribeNamesSource(t *testing.T) {
	gf, spec := baseURLValueFixture(t)
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !strings.Contains(surface.BaseURL, "/coilysiren/open-webui/url") {
		t.Errorf("describe base-url = %q, want the value address named", surface.BaseURL)
	}
}
