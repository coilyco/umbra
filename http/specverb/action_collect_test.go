package specverb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

func collectIssueGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.example/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can list issue { op "issueListIssues" }
		action list-all issue {
			describe "List every issue by auto-paginating issue list."
			input owner { positional; required; help "repo owner" }
			input repo { positional; required; help "repo name" }
			input state { flag; help "issue state" }
			input limit { flag; help "page size" }
			collect list issue {
				args {
					owner $owner
					repo $repo
					state $state
				}
				page-param page
				limit-param limit
				limit $limit
				default-limit "2"
				as issues
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse collect guardfile: %v", err)
	}
	return gf
}

func TestCollectActionAccumulatesUntilShortPage(t *testing.T) {
	var gotQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`[{"number":1},{"number":2}]`))
		case "2":
			_, _ = w.Write([]byte(`[{"number":3}]`))
		default:
			t.Errorf("unexpected page %s", r.URL.Query().Get("page"))
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := Config{Guardfile: collectIssueGuardfile(t), Spec: actionSpec(t), BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	out, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--state", "open", "--output", "json")
	if err != nil {
		t.Fatalf("run collect: %v", err)
	}
	var issues []map[string]any
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		t.Fatalf("output is not an issue array: %v\n%s", err, out)
	}
	if len(issues) != 3 {
		t.Fatalf("len(issues) = %d, want 3; output:\n%s", len(issues), out)
	}
	if len(gotQueries) != 2 {
		t.Fatalf("wire calls = %d, want 2 (%v)", len(gotQueries), gotQueries)
	}
	for _, q := range gotQueries {
		if !strings.Contains(q, "limit=2") || !strings.Contains(q, "state=open") {
			t.Errorf("query %q missing limit/state", q)
		}
	}
}

func TestCollectActionSuppliedLimitAndAbsentOptionalFilter(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	cfg := Config{Guardfile: collectIssueGuardfile(t), Spec: actionSpec(t), BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	if _, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--limit", "5", "--output", "json"); err != nil {
		t.Fatalf("run collect: %v", err)
	}
	if !strings.Contains(gotQuery, "limit=5") {
		t.Errorf("query %q missing supplied limit", gotQuery)
	}
	if strings.Contains(gotQuery, "state=") {
		t.Errorf("query %q should omit absent optional state", gotQuery)
	}
}

func TestCollectActionDryRunPlan(t *testing.T) {
	cfg := Config{Guardfile: collectIssueGuardfile(t), Spec: actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) {
			t.Fatal("dry-run must not fire or resolve a secret")
			return "", nil
		}}}
	out, err := runTree(t, cfg, "forgejo", "issue", "list-all", "kai", "demo", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{`"action": "list-all-issue"`, `"stop": "short_page"`, `page=1`, `limit=2`} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q:\n%s", want, out)
		}
	}
}

func TestCollectActionDescribed(t *testing.T) {
	surface, err := Describe(Config{Guardfile: collectIssueGuardfile(t), Spec: actionSpec(t)})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(surface.Actions) != 1 || surface.Actions[0].Collect == nil {
		t.Fatalf("collect action missing from surface: %+v", surface.Actions)
	}
	md := surface.Markdown()
	for _, want := range []string{"forgejo issue list-all", "Collects every page", "fewer than"} {
		if !strings.Contains(md, want) {
			t.Errorf("prose missing %q:\n%s", want, md)
		}
	}
}
