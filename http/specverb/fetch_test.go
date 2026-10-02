package specverb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

func fetchFixture(t *testing.T, method string) *guardfile.Guardfile {
	t.Helper()
	src := `wrap ward ops forgejo {
    fetch "actions logs" {
        method "` + method + `"
        path "/repos/{owner}/{repo}/actions/runs/{run}/jobs/{job}/attempt/{attempt}/logs"
        output "raw"
        env FORGEJO_TOKEN {
            value ssm "/forgejo/token"
        }
        header "Authorization" "token ${FORGEJO_TOKEN}"
        header "Accept" "text/plain"
        when first input matches coily*
    }
}`
	gf, err := guardfile.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse fetch guardfile: %v", err)
	}
	return gf
}

func TestFetchOverlayLiveAndDryRun(t *testing.T) {
	gf := fetchFixture(t, http.MethodGet)
	var gotMethod, gotPath, gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("raw logs\n"))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile:  gf,
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{
			"ssm": func(context.Context, string) (string, error) { return "sekret", nil },
		},
	}
	out, err := runTree(t, cfg, "forgejo", "fetch", "actions-logs", "coilyco", "demo", "1", "2", "3", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{srv.URL, "GET", "/repos/coilyco/demo/actions/runs/1/jobs/2/attempt/3/logs", "<redacted>", "coily*"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}

	cfg.HTTPClient = nil
	out, err = runTree(t, cfg, "forgejo", "fetch", "actions-logs", "coilyco", "demo", "1", "2", "3")
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/repos/coilyco/demo/actions/runs/1/jobs/2/attempt/3/logs" {
		t.Fatalf("server saw %s %s", gotMethod, gotPath)
	}
	if gotAuth != "token sekret" || gotAccept != "text/plain" {
		t.Errorf("headers = Authorization %q Accept %q", gotAuth, gotAccept)
	}
	if !strings.Contains(out, "raw logs") {
		t.Errorf("live fetch output missing raw body:\n%s", out)
	}
}

func TestFetchOverlayDescribeRendersHTTPFetch(t *testing.T) {
	gf := fetchFixture(t, http.MethodGet)
	out, err := runTree(t, Config{Guardfile: gf, BaseURL: "https://forgejo.example/api/v1"}, "forgejo", "describe")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	for _, want := range []string{"## ward ops forgejo fetch actions-logs", "`GET /repos/{owner}/{repo}/actions/runs/{run}/jobs/{job}/attempt/{attempt}/logs`", "Fetch overlay. Output is raw stdout.", "Authorization", "FORGEJO_TOKEN"} {
		if !strings.Contains(out, want) {
			t.Errorf("describe output missing %q:\n%s", want, out)
		}
	}
}

func TestFetchOverlayFailureIncludesStatusAndBody(t *testing.T) {
	gf := fetchFixture(t, http.MethodGet)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing logs", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: gf,
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "sekret", nil }},
	}
	_, err := runTree(t, cfg, "forgejo", "fetch", "actions-logs", "coilyco", "demo", "1", "2", "3")
	if err == nil {
		t.Fatal("expected a non-2xx fetch error")
	}
	for _, want := range []string{"404", "missing logs", "actions-logs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestFetchOverlayRefusesMutatingRedirects(t *testing.T) {
	gf := fetchFixture(t, http.MethodPost)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/coilyco/demo/actions/runs/1/jobs/2/attempt/3/logs" {
			t.Fatalf("unexpected redirected request: %s %s", r.Method, r.URL.Path)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: gf,
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "sekret", nil }},
	}
	_, err := runTree(t, cfg, "forgejo", "fetch", "actions-logs", "coilyco", "demo", "1", "2", "3")
	if err == nil || !strings.Contains(err.Error(), "refusing to follow a POST redirect") {
		t.Fatalf("mutating redirect should be refused, got %v", err)
	}
}
