package specverb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// TestAuthChainFallsBackLive proves a fallback-list auth value resolves the first
// available source (env unset -> ssm), each provider consulted once in order.
func TestAuthChainFallsBackLive(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		auth bearer {
			value {
				env TAILSCALE_API_KEY
				ssm "/tailscale/api-key"
			}
		}
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

	var envHit, ssmHit int
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{
			"env": func(_ context.Context, addr string) (string, error) {
				envHit++
				return "", fmt.Errorf("env %s not set", addr)
			},
			"ssm": func(context.Context, string) (string, error) { ssmHit++; return "tskey-backup", nil },
		}}
	if _, err := runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--output", "json"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotAuth != "Bearer tskey-backup" {
		t.Errorf("auth header = %q, want the ssm fallback", gotAuth)
	}
	if envHit != 1 || ssmHit != 1 {
		t.Errorf("provider hits env=%d ssm=%d, want 1 each (env tried first, ssm backs it)", envHit, ssmHit)
	}
}

// TestAuthChainAllFailNoLeak proves an all-failed auth chain fails closed before
// the request fires, the error never carrying a value a provider leaked.
func TestAuthChainAllFailNoLeak(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		auth bearer {
			value {
				env TAILSCALE_API_KEY
				ssm "/tailscale/api-key"
			}
		}
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{
			// A misbehaving provider returns a value AND an error; the value must never
			// surface in the combined failure.
			"env": func(context.Context, string) (string, error) { return "LEAKY-ENV", fmt.Errorf("env unset") },
			"ssm": func(context.Context, string) (string, error) { return "", fmt.Errorf("ssm denied") },
		}}
	_, runErr := runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--output", "json")
	if runErr == nil {
		t.Fatal("expected an error when every auth source fails")
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0 (a failed auth chain must not fire the request)", hits)
	}
	msg := runErr.Error()
	if strings.Contains(msg, "LEAKY-ENV") {
		t.Errorf("all-fail error leaked a resolved value:\n%s", msg)
	}
	for _, want := range []string{"env", "ssm"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should name the tried provider %q", msg, want)
		}
	}
}

// TestBaseURLChainFallsBackLive proves the base-url fallback list resolves the
// host through its first available source (env unset, ssm supplies the host).
func TestBaseURLChainFallsBackLive(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"devices":[]}`))
	}))
	defer srv.Close()

	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		base-url {
			value {
				env TAILSCALE_BASE_URL
				ssm "/tailscale/base-url"
			}
		}
		auth bearer { value ssm "/tailscale/api-key" }
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := Config{Guardfile: gf, Spec: spec,
		Providers: map[string]Provider{
			"env": func(context.Context, string) (string, error) { return "", fmt.Errorf("env unset") },
			"ssm": func(_ context.Context, addr string) (string, error) {
				if addr == "/tailscale/base-url" {
					return srv.URL, nil
				}
				return "tskey-abc", nil
			},
		}}
	if _, err := runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--output", "json"); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestAuthChainDescribeNamesSources proves the describe surface names every
// fallback source, joined symbolically, and never a resolved secret.
func TestAuthChainDescribeNamesSources(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		auth bearer {
			value {
				env TAILSCALE_API_KEY
				ssm "/tailscale/api-key"
			}
		}
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if surface.Auth.Source != "env TAILSCALE_API_KEY | ssm /tailscale/api-key" {
		t.Errorf("auth source = %q, want both fallback sources named", surface.Auth.Source)
	}
}

// TestAuthChainDryRunOffline proves a --dry-run over a chained auth value redacts
// the secret and never resolves any source in the chain.
func TestAuthChainDryRunOffline(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		auth bearer {
			value {
				env TAILSCALE_API_KEY
				ssm "/tailscale/api-key"
			}
		}
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: "https://api.tailscale.com",
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{
			"env": func(context.Context, string) (string, error) {
				t.Fatal("dry-run must not resolve a chain source")
				return "", nil
			},
			"ssm": func(context.Context, string) (string, error) {
				t.Fatal("dry-run must not resolve a chain source")
				return "", nil
			},
		}}
	out, err := runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "redacted") {
		t.Errorf("dry-run should redact the secret:\n%s", out)
	}
}
