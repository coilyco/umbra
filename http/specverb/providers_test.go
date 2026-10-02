package specverb

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// TestBuiltinEnvProviderResolves proves umbra's built-in `env` provider
// resolves an auth secret end to end with zero consumer wiring (no Config.Providers).
func TestBuiltinEnvProviderResolves(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		auth bearer { value env "TS_API_KEY" }
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	t.Setenv("TS_API_KEY", "tskey-from-env")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"devices":[]}`))
	}))
	defer srv.Close()

	// No Providers: the built-in env resolver must satisfy the value source.
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL}
	if _, err := runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--output", "json"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotAuth != "Bearer tskey-from-env" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Bearer tskey-from-env")
	}
}

// TestUnregisteredProviderFailsClosed proves a value source naming a provider no
// one registered is a coded request-time error, never a silent empty secret.
func TestUnregisteredProviderFailsClosed(t *testing.T) {
	spec := readSpec(t, "tailscale.openapi.yaml")
	gf, err := guardfile.Parse([]byte(`wrap ward ops tailscale {
		spec tailscale.openapi.yaml
		auth bearer { value vault "secret/ts" }
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Build/mount must succeed offline; only firing resolves the value.
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: "https://example.test"}
	_, err = runTree(t, cfg, "tailscale", "devices", "list", "my-tailnet", "--output", "json")
	if err == nil {
		t.Fatal("expected a fail-closed error for the unregistered provider")
	}
	if !strings.Contains(err.Error(), "vault") {
		t.Errorf("error = %v, want it to name the missing provider %q", err, "vault")
	}
}

// TestBuiltinFileAndLiteralProviders proves the file and literal built-ins resolve
// through the merged registry; a consumer provider of the same name would win.
func TestBuiltinFileAndLiteralProviders(t *testing.T) {
	reg := mergeProviders(nil)
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("  secret-on-disk\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	got, err := reg["file"](t.Context(), tokenFile)
	if err != nil {
		t.Fatalf("file provider: %v", err)
	}
	if got != "secret-on-disk" {
		t.Errorf("file provider = %q, want the trimmed file contents", got)
	}
	lit, err := reg["literal"](t.Context(), "inline-value")
	if err != nil {
		t.Fatalf("literal provider: %v", err)
	}
	if lit != "inline-value" {
		t.Errorf("literal provider = %q, want %q", lit, "inline-value")
	}
}
