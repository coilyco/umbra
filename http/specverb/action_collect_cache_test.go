package specverb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/config"
	"github.com/urfave/cli/v3"
)

// cachedCollectGuardfile builds the list-all collect action with a `cache`
// modifier set to ttl. An empty ttl omits the modifier entirely.
func cachedCollectGuardfile(t *testing.T, ttl string) *guardfile.Guardfile {
	t.Helper()
	cacheLine := ""
	if ttl != "" {
		cacheLine = "cache \"" + ttl + "\""
	}
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.example/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can list issue { op "issueListIssues" }
		action list-all issue {
			input owner { positional; required; help "repo owner" }
			input repo { positional; required; help "repo name" }
			input state { flag; help "issue state" }
			collect list issue {
				args {
					owner $owner
					repo $repo
					state $state
				}
				page-param page
				limit-param limit
				default-limit "2"
				as issues
				` + cacheLine + `
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse cached collect guardfile: %v", err)
	}
	return gf
}

// cacheTestServer returns a single-short-page issue server and a hit counter.
func cacheTestServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`[{"number":1}]`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func cacheWriter(t *testing.T) *audit.Writer {
	t.Helper()
	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// TestCollectCacheHitServesWithoutRefetch proves the second run is served from
// the on-disk cache (no wire call) and stamps `cache: "hit"` on the audit row.
func TestCollectCacheHitServesWithoutRefetch(t *testing.T) {
	t.Setenv(config.CacheDirEnv(), t.TempDir())
	srv, calls := cacheTestServer(t)
	w := cacheWriter(t)

	cfg := Config{Guardfile: cachedCollectGuardfile(t, "10m"), Spec: actionSpec(t), BaseURL: srv.URL,
		Wrap:      func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) },
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}

	first, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--output", "json")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("first run wire calls = %d, want 1", got)
	}

	second, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--output", "json")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("second run wire calls = %d, want 1 (cache hit should not refetch)", got)
	}
	if strings.TrimSpace(first) != strings.TrimSpace(second) {
		t.Fatalf("cache hit output differs from miss:\n%q\n%q", first, second)
	}

	rows := auditRows(t, w.Path)
	envelopes := 0
	for _, r := range rows {
		if r.Verb != "ward.ops.forgejo.issue.list-all" {
			continue
		}
		envelopes++
		if envelopes == 2 && r.Cache != "hit" {
			t.Errorf("second envelope cache = %q, want \"hit\"; rows:\n%+v", r.Cache, rows)
		}
		if envelopes == 1 && r.Cache != "" {
			t.Errorf("first (miss) envelope cache = %q, want empty", r.Cache)
		}
	}
	if envelopes != 2 {
		t.Fatalf("envelope rows = %d, want 2", envelopes)
	}
}

// TestCollectCacheNoCacheBypasses proves --no-cache neither reads nor writes.
func TestCollectCacheNoCacheBypasses(t *testing.T) {
	t.Setenv(config.CacheDirEnv(), t.TempDir())
	srv, calls := cacheTestServer(t)

	cfg := Config{Guardfile: cachedCollectGuardfile(t, "10m"), Spec: actionSpec(t), BaseURL: srv.URL,
		Wrap:      func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, cacheWriter(t)) },
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}

	for i := 0; i < 2; i++ {
		if _, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--no-cache", "--output", "json"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("wire calls = %d, want 2 (--no-cache must always refetch)", got)
	}
}

// TestCollectCacheRefreshInvalidates proves --refresh refetches even with a
// warm entry, then leaves a fresh entry a plain run can serve.
func TestCollectCacheRefreshInvalidates(t *testing.T) {
	t.Setenv(config.CacheDirEnv(), t.TempDir())
	srv, calls := cacheTestServer(t)

	cfg := Config{Guardfile: cachedCollectGuardfile(t, "10m"), Spec: actionSpec(t), BaseURL: srv.URL,
		Wrap:      func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, cacheWriter(t)) },
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}

	if _, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--output", "json"); err != nil {
		t.Fatalf("warm run: %v", err)
	}
	if _, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--refresh", "--output", "json"); err != nil {
		t.Fatalf("refresh run: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("wire calls = %d, want 2 (--refresh must refetch)", got)
	}
	// The refresh rewrote the entry, so a plain run is a hit (no third call).
	if _, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--output", "json"); err != nil {
		t.Fatalf("post-refresh run: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("wire calls = %d, want 2 (post-refresh run should hit cache)", got)
	}
}

// TestCollectCacheDryRunPrintsPlan proves --dry-run surfaces the key, TTL, and
// directory without firing or resolving a secret.
func TestCollectCacheDryRunPrintsPlan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.CacheDirEnv(), dir)

	cfg := Config{Guardfile: cachedCollectGuardfile(t, "10m"), Spec: actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) {
			t.Fatal("dry-run must not resolve a secret")
			return "", nil
		}}}
	out, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{`"cache"`, `"ttl": "10m0s"`, `"enabled": true`, dir} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run plan missing %q:\n%s", want, out)
		}
	}
}

// TestCollectCacheRejectsBadTTL proves an unparseable cache duration fails the
// build closed rather than silently disabling the cache.
func TestCollectCacheRejectsBadTTL(t *testing.T) {
	_, err := Build(Config{Guardfile: cachedCollectGuardfile(t, "soon"), Spec: actionSpec(t)})
	if err == nil || !strings.Contains(err.Error(), "not a valid duration") {
		t.Fatalf("want a bad-duration build error, got %v", err)
	}
}

// TestCollectCacheRejectedOnPoll proves `cache` fails closed outside collect:
// poll needs live data, so the parser rejects the modifier.
func TestCollectCacheRejectedOnPoll(t *testing.T) {
	_, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		auth header-token { header Authorization; value ssm "/forgejo/api-token" }
		can list issue { op "issueListIssues" }
		action watch issue {
			poll list issue {
				until "true"
				every "1s"
				timeout "5s"
				as issues
				cache "10m"
			}
		}
	}`))
	if err == nil || !strings.Contains(err.Error(), "cache") {
		t.Fatalf("want poll to reject `cache`, got %v", err)
	}
}

// auditRows reads the JSONL audit file into records, skipping blank lines.
func auditRows(t *testing.T, path string) []audit.Record {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var rows []audit.Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decode audit row %q: %v", line, err)
		}
		rows = append(rows, r)
	}
	return rows
}
