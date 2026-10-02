package specverb

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/audit"
	"github.com/urfave/cli/v3"
)

// loadFixtures reads the proving-slice Guardfile and spec from testdata.
func loadFixtures(t *testing.T) (*guardfile.Guardfile, []byte) {
	t.Helper()
	kdl, err := os.ReadFile(filepath.Join("testdata", "forgejo.kdl"))
	if err != nil {
		t.Fatalf("read guardfile: %v", err)
	}
	gf, err := guardfile.Parse(kdl)
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	spec, err := os.ReadFile(filepath.Join("testdata", "forgejo.swagger.v1.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	return gf, spec
}

// runTree mounts the built command under `ward ops` and runs argv, capturing
// stdout. The leading "ward","ops" mirror how the real consumer mounts it.
func runTree(t *testing.T, cfg Config, argv ...string) (string, error) {
	t.Helper()
	root, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	app := &cli.Command{
		Name:     "ward",
		Commands: []*cli.Command{{Name: "ops", Commands: []*cli.Command{root}}},
	}

	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	runErr := app.Run(context.Background(), append([]string{"ward", "ops"}, argv...))
	_ = w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

// failingTransport fails any live HTTP call, so a dry-run test proves the wire
// is never touched.
type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Fatalf("dry-run must not fire an HTTP request")
	return nil, http.ErrUseLastResponse // unreachable: t.Fatalf stops the goroutine
}

func TestBuildMountsProvingSlice(t *testing.T) {
	gf, spec := loadFixtures(t)
	root, err := Build(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if root.Name != "forgejo" {
		t.Errorf("root name = %q, want forgejo", root.Name)
	}
	repo := childNamed(root, "repo")
	if repo == nil {
		t.Fatalf("want a `repo` group, got %v", names(root.Commands))
	}
	// describe is mounted as a sibling verb on the group.
	if childNamed(root, "describe") == nil {
		t.Fatalf("want a `describe` verb on the group, got %v", names(root.Commands))
	}
	leaves := names(repo.Commands)
	want := map[string]bool{"get": true, "create": true, "delete": true}
	if len(leaves) != 3 {
		t.Fatalf("want 3 leaves, got %v", leaves)
	}
	for _, l := range leaves {
		if !want[l] {
			t.Errorf("unexpected leaf %q", l)
		}
	}
}

func names(cmds []*cli.Command) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = c.Name
	}
	return out
}

func TestDenyByDefault(t *testing.T) {
	_, spec := loadFixtures(t)
	// The op the grant names is absent from the spec: the build must fail closed
	// rather than silently drop the verb.
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		auth header-token { header Authorization; value ssm "/forgejo/api-token" }
		can read webhooks { op "repoListWebhooks" }
	}`))
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	if _, err := Build(Config{Guardfile: gf, Spec: spec}); err == nil {
		t.Fatal("expected a fail-closed error for an op absent from the spec, got nil")
	}
}

func TestDryRunCreate(t *testing.T) {
	gf, spec := loadFixtures(t)
	cfg := Config{
		Guardfile:  gf,
		Spec:       spec,
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) {
			t.Fatal("dry-run must not resolve the auth secret")
			return "", nil
		}},
	}
	out, err := runTree(t, cfg, "forgejo", "repo", "create", "--name", "demo", "--private", "--dry-run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"POST", "/user/repos", "demo", "private", "redacted"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "token actual") {
		t.Errorf("dry-run leaked a secret:\n%s", out)
	}
}

func TestLiveCreate(t *testing.T) {
	gf, spec := loadFixtures(t)
	var gotAuth, gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"full_name":"kai/demo"}`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: gf,
		Spec:      spec,
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "sekret", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "repo", "create", "--name", "demo", "--output", "json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/user/repos" {
		t.Errorf("server saw %s %s, want POST /user/repos", gotMethod, gotPath)
	}
	if gotAuth != "token sekret" {
		t.Errorf("auth header = %q, want %q", gotAuth, "token sekret")
	}
	if !strings.Contains(gotBody, `"name":"demo"`) {
		t.Errorf("body = %q, want name=demo", gotBody)
	}
	// an unset optional must not be sent
	if strings.Contains(gotBody, "private") {
		t.Errorf("unset optional leaked into body: %q", gotBody)
	}
	if !strings.Contains(out, "kai/demo") {
		t.Errorf("rendered response missing full_name:\n%s", out)
	}
}

func TestLiveDeleteFillsPathParams(t *testing.T) {
	gf, spec := loadFixtures(t)
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: gf,
		Spec:      spec,
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "sekret", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "repo", "delete", "kai", "demo")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/repos/kai/demo" {
		t.Errorf("server saw %s %s, want DELETE /repos/kai/demo", gotMethod, gotPath)
	}
	if !strings.Contains(out, "ok:") {
		t.Errorf("empty 2xx should print a confirmation, got:\n%s", out)
	}
}

func TestPositionalArgCountValidated(t *testing.T) {
	gf, spec := loadFixtures(t)
	cfg := Config{
		Guardfile: gf,
		Spec:      spec,
		BaseURL:   "http://127.0.0.1:0",
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	// delete wants <owner> <repo>; one positional is a user error before any wire call.
	if _, err := runTree(t, cfg, "forgejo", "repo", "delete", "kai"); err == nil {
		t.Fatal("expected a positional-arg-count error, got nil")
	}
}

// TestComposesWithVerbWrap proves the engine mounts under the real verb
// pipeline (audit + argv gate), not just the identity wrap.
func TestComposesWithVerbWrap(t *testing.T) {
	gf, spec := loadFixtures(t)
	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(func() { _ = w.Close() })

	cfg := Config{
		Guardfile:  gf,
		Spec:       spec,
		Wrap:       func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) },
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	if _, err := runTree(t, cfg, "forgejo", "repo", "create", "--name", "demo", "--dry-run"); err != nil {
		t.Fatalf("run through verb.Wrap: %v", err)
	}
	// the wrapped action wrote an audit row
	if data, _ := os.ReadFile(w.Path); !strings.Contains(string(data), "ward.ops.forgejo.repo.create") {
		t.Errorf("audit row missing the verb name; got:\n%s", string(data))
	}
}

// withGate wires the real verb pipeline (audit writer + shell-metachar gate) so
// a leaf's argsFuncFor is actually enforced, not bypassed by the identity wrap.
func withGate(t *testing.T, gf *guardfile.Guardfile, spec []byte) Config {
	t.Helper()
	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(func() { _ = w.Close() })
	return Config{
		Guardfile:  gf,
		Spec:       spec,
		Wrap:       func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) },
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
}

// TestBodyParamExemptFromMetacharGate proves the gate is location-aware: a `(` in
// a body field is JSON-encoded, never shell/URL-bound, so it must pass.
func TestBodyParamExemptFromMetacharGate(t *testing.T) {
	gf, spec := loadFixtures(t)
	cfg := withGate(t, gf, spec)
	out, err := runTree(t, cfg, "forgejo", "repo", "create",
		"--name", "demo", "--description", "Game projects (eco + factorio)", "--dry-run")
	if err != nil {
		t.Fatalf("body param with `(` must not trip the metachar gate: %v", err)
	}
	if !strings.Contains(out, "eco + factorio") {
		t.Errorf("description did not survive into the request body:\n%s", out)
	}
}

// TestPathParamStillGated proves location-awareness does not disarm the gate on
// the URL injection surface: a metacharacter in a path positional is rejected.
func TestPathParamStillGated(t *testing.T) {
	gf, spec := loadFixtures(t)
	cfg := withGate(t, gf, spec)
	_, err := runTree(t, cfg, "forgejo", "repo", "delete", "kai$(whoami)", "demo")
	if err == nil {
		t.Fatal("a metacharacter in a path param must still be rejected")
	}
	if !strings.Contains(err.Error(), "shell metacharacter") {
		t.Errorf("want a shell-metacharacter rejection, got: %v", err)
	}
}

// listIssuesGate builds the query-bearing proving slice: issueListIssues has
// ?state&labels&..., so a query metacharacter has somewhere to land.
func listIssuesGate(t *testing.T) Config {
	t.Helper()
	kdl, err := os.ReadFile(filepath.Join("testdata", "forgejo.kdl"))
	if err != nil {
		t.Fatalf("read guardfile: %v", err)
	}
	kdl = append(kdl[:len(kdl)-2], []byte("\n    can list issues\n}\n")...)
	gf, err := guardfile.Parse(kdl)
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	spec, err := os.ReadFile(filepath.Join("testdata", "forgejo.swagger.v1.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	return withGate(t, gf, spec)
}

// TestQueryParamNotGated replaces TestQueryParamStillGated: a query metachar
// now reaches the request, and must arrive percent-encoded.
func TestQueryParamNotGated(t *testing.T) {
	out, err := runTree(t, listIssuesGate(t), "forgejo", "issues", "list", "kai", "demo", "--labels", "a|b", "--dry-run")
	if err != nil {
		t.Fatalf("a metacharacter in a query flag must no longer be rejected: %v", err)
	}
	if strings.Contains(out, "a|b") {
		t.Errorf("query value reached the URL unencoded:\n%s", out)
	}
	if !strings.Contains(out, "a%7Cb") {
		t.Errorf("dry-run output missing the percent-encoded query value a%%7Cb:\n%s", out)
	}
}

// Proves the opt-out exists, is per-param, and reaches no further: `owner` is
// exempt and `repo` beside it is not.
func TestAllowMetacharactersExemptsNamedPathParam(t *testing.T) {
	kdl, err := os.ReadFile(filepath.Join("testdata", "forgejo.kdl"))
	if err != nil {
		t.Fatalf("read guardfile: %v", err)
	}
	kdl = append(kdl[:len(kdl)-2], []byte("\n    can list issues\n    allow-metacharacters \"owner\"\n}\n")...)
	gf, err := guardfile.Parse(kdl)
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	if len(gf.AllowMeta) != 1 || gf.AllowMeta[0] != "owner" {
		t.Fatalf("AllowMeta = %v, want [owner]", gf.AllowMeta)
	}
	spec, err := os.ReadFile(filepath.Join("testdata", "forgejo.swagger.v1.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	cfg := withGate(t, gf, spec)

	if _, err := runTree(t, cfg, "forgejo", "issues", "list", "kai{x}", "demo", "--dry-run"); err != nil {
		t.Fatalf("owner is named in allow-metacharacters and must pass the gate: %v", err)
	}
	_, err = runTree(t, cfg, "forgejo", "issues", "list", "kai", "demo{x}", "--dry-run")
	if err == nil {
		t.Fatal("repo is not named in allow-metacharacters and must still be refused")
	}
	if !strings.Contains(err.Error(), "shell metacharacter") {
		t.Errorf("want a shell-metacharacter rejection for repo, got: %v", err)
	}
}

// TestMountGeneratesIntermediatePath proves Mount grafts the built group onto a
// root, creating the `ops` path segment the Guardfile names but root lacks.
func TestMountGeneratesIntermediatePath(t *testing.T) {
	gf, spec := loadFixtures(t)
	root := &cli.Command{Name: "ward"}
	if err := Mount(root, Config{Guardfile: gf, Spec: spec}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	ops := childNamed(root, "ops")
	if ops == nil {
		t.Fatalf("root has no generated `ops` group; got %v", names(root.Commands))
	}
	if childNamed(ops, "forgejo") == nil {
		t.Fatalf("`ops` has no `forgejo` group; got %v", names(ops.Commands))
	}
}

// TestMountReusesExistingPath proves Mount attaches to an `ops` group that
// already exists rather than creating a duplicate.
func TestMountReusesExistingPath(t *testing.T) {
	gf, spec := loadFixtures(t)
	existing := &cli.Command{Name: "ops"}
	root := &cli.Command{Name: "ward", Commands: []*cli.Command{existing}}
	if err := Mount(root, Config{Guardfile: gf, Spec: spec}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if n := len(root.Commands); n != 1 {
		t.Fatalf("root should keep one `ops` group, got %d: %v", n, names(root.Commands))
	}
	if childNamed(existing, "forgejo") == nil {
		t.Errorf("existing `ops` did not gain `forgejo`; got %v", names(existing.Commands))
	}
}

// TestDescribeModel proves the surface model mirrors the mounted verbs: one
// VerbInfo per grant, with auth scope as the token path (never the secret).
func TestDescribeModel(t *testing.T) {
	gf, spec := loadFixtures(t)
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got, want := surface.Auth.Source, "ssm /forgejo/api-token"; got != want {
		t.Errorf("auth source = %q, want %q", got, want)
	}
	if surface.Auth.Header != "Authorization" {
		t.Errorf("auth header = %q, want Authorization", surface.Auth.Header)
	}
	byLeaf := map[string]VerbInfo{}
	for _, v := range surface.Verbs {
		byLeaf[v.Leaf] = v
	}
	if len(byLeaf) != 3 {
		t.Fatalf("want 3 verbs in the model, got %d: %+v", len(byLeaf), surface.Verbs)
	}
	create := byLeaf["create"]
	if create.Method != "POST" || create.Path != "/user/repos" {
		t.Errorf("create = %s %s, want POST /user/repos", create.Method, create.Path)
	}
	if create.Grant != "can create repo" {
		t.Errorf("create grant = %q, want %q", create.Grant, "can create repo")
	}
	if create.Name != "ward.ops.forgejo.repo.create" {
		t.Errorf("create dotted name = %q", create.Name)
	}
	del := byLeaf["delete"]
	if !del.Destructive {
		t.Errorf("delete should be flagged destructive")
	}
	if del.Describe == "" {
		t.Errorf("delete should carry the guardfile describe note")
	}
	// path params are modeled as required, kind=path, in invocation order.
	if len(del.Params) != 2 || del.Params[0].Name != "owner" || del.Params[0].Kind != "path" {
		t.Errorf("delete params = %+v, want owner/repo path params", del.Params)
	}
}

// TestDescribeVerbRenders proves `describe` is a real, runnable verb on the
// group whose default output is the readable prose reference.
func TestDescribeVerbRenders(t *testing.T) {
	gf, spec := loadFixtures(t)
	out, err := runTree(t, Config{Guardfile: gf, Spec: spec}, "forgejo", "describe")
	if err != nil {
		t.Fatalf("run describe: %v", err)
	}
	for _, want := range []string{
		"## ward ops forgejo repo create", // heading carries the full command path
		"/user/repos",
		"Authorized by grant: can create repo",
		"/forgejo/api-token",
		"Destructive - mutates irreversibly.",
		"deletes the repo",
		"Positional arguments (", // path params, their own list
		"Options (",              // body flags, kept separate
	} {
		if !strings.Contains(out, want) {
			t.Errorf("describe output missing %q:\n%s", want, out)
		}
	}
}

// TestLeafDescriptionIsRich proves a mounted leaf carries structural help -
// method/path, the grant, kind-tagged params, dry-run hint - even with no spec desc.
func TestLeafDescriptionIsRich(t *testing.T) {
	gf, spec := loadFixtures(t)
	root, err := Build(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	del := childNamed(childNamed(root, "repo"), "delete")
	if del == nil {
		t.Fatal("no repo delete leaf")
	}
	for _, want := range []string{"DELETE", "/repos/{owner}/{repo}", "Authorized by: can delete repo", "<owner> (path", "--dry-run", "deletes the repo"} {
		if !strings.Contains(del.Description, want) {
			t.Errorf("leaf description missing %q:\n%s", want, del.Description)
		}
	}
}

func childNamed(parent *cli.Command, name string) *cli.Command {
	for _, c := range parent.Commands {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// issuesFixtures parses an issues-grants guardfile against the shared spec,
// the consumer for the query-param and array-body primitives.
func issuesFixtures(t *testing.T) (*guardfile.Guardfile, []byte) {
	t.Helper()
	_, spec := loadFixtures(t)
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		auth header-token { header Authorization; value ssm "/forgejo/api-token" }
		can list issue { op "issueListIssues" }
		can create issue { op "issueCreateIssue" }
	}`))
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	return gf, spec
}

// TestQueryParamsPromoted proves scalar query params mount as flags and a set
// flag lands in the URL's query string; unset ones are omitted entirely.
func TestQueryParamsPromoted(t *testing.T) {
	gf, spec := issuesFixtures(t)
	cfg := Config{Guardfile: gf, Spec: spec, HTTPClient: &http.Client{Transport: failingTransport{t}}}
	out, err := runTree(t, cfg, "forgejo", "issue", "list", "kai", "demo", "--state", "open", "--page", "2", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// JSON rendering escapes the ampersand, so assert the parts separately.
	for _, want := range []string{"/repos/kai/demo/issues?page=2", "state=open"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run URL missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "limit") {
		t.Errorf("unset query param leaked into the URL:\n%s", out)
	}
}

// TestArrayBodyFlags proves array-of-scalar body fields mount as repeatable
// flags and serialize as JSON arrays of the right element type.
func TestArrayBodyFlags(t *testing.T) {
	gf, spec := issuesFixtures(t)
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	_, err := runTree(t, cfg, "forgejo", "issue", "create", "kai", "demo",
		"--title", "t", "--assignees", "a", "--assignees", "b", "--labels", "7", "--labels", "9")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{`"assignees":["a","b"]`, `"labels":[7,9]`, `"title":"t"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body = %q, want it to contain %q", gotBody, want)
		}
	}
}

// TestBodyFileSuppliesBody proves --body-file replaces the body flags wholesale
// and satisfies required-field enforcement.
func TestBodyFileSuppliesBody(t *testing.T) {
	gf, spec := issuesFixtures(t)
	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, []byte(`{"title":"from-file","assignees":["c"]}`), 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}
	cfg := Config{Guardfile: gf, Spec: spec, HTTPClient: &http.Client{Transport: failingTransport{t}}}
	out, err := runTree(t, cfg, "forgejo", "issue", "create", "kai", "demo", "--body-file", path, "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"from-file", `"c"`} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run body missing %q:\n%s", want, out)
		}
	}
}

// TestBodyFileExclusiveWithFlags proves mixing --body-file with a body flag is
// a user error, not a silent merge.
func TestBodyFileExclusiveWithFlags(t *testing.T) {
	gf, spec := issuesFixtures(t)
	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, []byte(`{"title":"x"}`), 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: "http://127.0.0.1:0",
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	if _, err := runTree(t, cfg, "forgejo", "issue", "create", "kai", "demo", "--body-file", path, "--title", "y", "--dry-run"); err == nil {
		t.Fatal("expected a mutual-exclusion error, got nil")
	}
}

// TestRequiredBodyFieldEnforced proves a missing required field fails at
// request assembly with a pointer at both sources.
func TestRequiredBodyFieldEnforced(t *testing.T) {
	gf, spec := issuesFixtures(t)
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: "http://127.0.0.1:0",
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	_, err := runTree(t, cfg, "forgejo", "issue", "create", "kai", "demo", "--body", "no title", "--dry-run")
	if err == nil {
		t.Fatal("expected a required-field error, got nil")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

// TestFlagCollisionFailsClosed proves a spec input shadowing a reserved engine
// flag refuses to build rather than silently winning.
func TestFlagCollisionFailsClosed(t *testing.T) {
	desc := opDescriptor{
		VerbName:   "ward.ops.forgejo.issue.list",
		QueryFlags: []fieldFlag{{Name: "output", Type: "string"}},
	}
	if err := checkFlagCollisions(desc); err == nil {
		t.Fatal("expected a reserved-flag collision error, got nil")
	}
	desc = opDescriptor{
		VerbName:   "ward.ops.forgejo.issue.list",
		QueryFlags: []fieldFlag{{Name: "state", Type: "string"}},
		BodyFlags:  []fieldFlag{{Name: "state", Type: "string"}},
	}
	if err := checkFlagCollisions(desc); err == nil {
		t.Fatal("expected a query/body name collision error, got nil")
	}
}

// TestStateToggleSendsFixedBody proves a close grant PATCHes exactly
// {"state":"closed"}, mounts no body flags, and says so in describe.
func TestStateToggleSendsFixedBody(t *testing.T) {
	_, spec := loadFixtures(t)
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		auth header-token { header Authorization; value ssm "/forgejo/api-token" }
		can close issue { op "issueEditIssue"; body state="closed" }
	}`))
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	if _, err := runTree(t, cfg, "forgejo", "issue", "close", "kai", "demo", "7"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "PATCH" || gotPath != "/repos/kai/demo/issues/7" {
		t.Errorf("server saw %s %s, want PATCH /repos/kai/demo/issues/7", gotMethod, gotPath)
	}
	if gotBody != `{"state":"closed"}` {
		t.Errorf("body = %q, want exactly the fixed state body", gotBody)
	}
	// the toggle owns its body: the edit op's fields must not mount as flags
	root, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	closeLeaf := childNamed(childNamed(root, "issue"), "close")
	for _, f := range closeLeaf.Flags {
		for _, name := range f.Names() {
			if name == "title" || name == "state" || name == flagBodyFile {
				t.Errorf("state toggle mounted body flag %q", name)
			}
		}
	}
	surface, err := Describe(cfg)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := surface.Verbs[0].FixedBody["state"]; got != "closed" {
		t.Errorf("describe fixed body = %v", surface.Verbs[0].FixedBody)
	}
}

// TestUntypedArrayTakesNames proves an untyped-items array (IssueLabelsOption)
// mounts as a repeatable string flag carrying label names through verbatim.
func TestUntypedArrayTakesNames(t *testing.T) {
	_, spec := loadFixtures(t)
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		auth header-token { header Authorization; value ssm "/forgejo/api-token" }
		can add issue-label { op "issueAddLabel" }
	}`))
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	if _, err := runTree(t, cfg, "forgejo", "issue-label", "add", "kai", "demo", "7", "--labels", "bug", "--labels", "P3"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(gotBody, `"labels":["bug","P3"]`) {
		t.Errorf("body = %q, want the names as a string array", gotBody)
	}
}

// runIssueLabels returns the request body issueAddLabel sends, so the union
// cases below differ only by their arguments.
func runIssueLabels(t *testing.T, values ...string) string {
	t.Helper()
	_, spec := loadFixtures(t)
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		auth header-token { header Authorization; value ssm "/forgejo/api-token" }
		can add issue-label { op "issueAddLabel" }
	}`))
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	args := []string{"forgejo", "issue-label", "add", "kai", "demo", "7"}
	for _, value := range values {
		args = append(args, "--labels", value)
	}
	if _, err := runTree(t, cfg, args...); err != nil {
		t.Fatalf("run: %v", err)
	}
	return gotBody
}

// The other half of the union: 332 went as "332", matched no label name,
// applied nothing, and returned success. agentic-os#1047
func TestUntypedArrayTakesNumericIDs(t *testing.T) {
	if got := runIssueLabels(t, "332", "333"); !strings.Contains(got, `"labels":[332,333]`) {
		t.Errorf("body = %q, want the IDs as a number array", got)
	}
}

// The spec's own sentence: "integers representing label IDs or strings
// representing label names".
func TestUntypedArrayMixesNamesAndIDs(t *testing.T) {
	if got := runIssueLabels(t, "332", "bug"); !strings.Contains(got, `"labels":[332,"bug"]`) {
		t.Errorf("body = %q, want the ID as a number and the name as a string", got)
	}
}

// Nothing that merely looks numeric is coerced, since a label may be named
// with digits and a sign.
func TestUntypedArrayKeepsNonNumericTokensQuoted(t *testing.T) {
	for _, token := range []string{"-1", "1.5", "1a", "007x", " 12"} {
		got := runIssueLabels(t, token)
		if !strings.Contains(got, `"labels":["`+token+`"]`) {
			t.Errorf("token %q: body = %q, want it left as a string", token, got)
		}
	}
}

// TestMultipartUpload proves a formData op streams the file flag as a real
// multipart part and keeps the scalar form field beside it.
func TestMultipartUpload(t *testing.T) {
	_, spec := loadFixtures(t)
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		auth header-token { header Authorization; value ssm "/forgejo/api-token" }
		can upload-asset release { op "repoCreateReleaseAttachment" }
	}`))
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	asset := filepath.Join(t.TempDir(), "asset.bin")
	if err := os.WriteFile(asset, []byte("payload-bytes"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	var gotContentType, gotFile, gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		f, hdr, err := r.FormFile("attachment")
		if err != nil {
			t.Errorf("form file: %v", err)
		} else {
			b, _ := io.ReadAll(f)
			gotFile = string(b)
			gotFilename = hdr.Filename
			_ = f.Close()
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	if _, err := runTree(t, cfg, "forgejo", "release", "upload-asset", "kai", "demo", "5", "--attachment", asset, "--name", "asset.bin"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("content type = %q, want multipart/form-data", gotContentType)
	}
	if gotFile != "payload-bytes" || gotFilename != "asset.bin" {
		t.Errorf("file part = %q (%q), want payload-bytes (asset.bin)", gotFile, gotFilename)
	}
	// dry run must not read the file, only name the part
	cfg.HTTPClient = &http.Client{Transport: failingTransport{t}}
	out, err := runTree(t, cfg, "forgejo", "release", "upload-asset", "kai", "demo", "5", "--attachment", asset, "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "@"+asset) {
		t.Errorf("dry-run preview should name the file part:\n%s", out)
	}
}

// TestBoolFixedBodyToggle proves a non-string fixed body (archive ->
// {"archived":true}) serializes with its JSON type in the describe sentence.
func TestBoolFixedBodyToggle(t *testing.T) {
	fixed := map[string]any{"archived": true}
	if got := fixedBodySentence(fixed); !strings.Contains(got, `"archived": true`) {
		t.Errorf("fixed body sentence = %q, want a JSON-typed rendering", got)
	}
}

// TestDescribeShowsQueryAndArrayParams proves the describe model carries the
// new param kinds so the reference doc and help stay truthful.
func TestDescribeShowsQueryAndArrayParams(t *testing.T) {
	gf, spec := issuesFixtures(t)
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	byLeaf := map[string]VerbInfo{}
	for _, v := range surface.Verbs {
		byLeaf[v.Leaf] = v
	}
	kinds := map[string]string{}
	types := map[string]string{}
	for _, p := range byLeaf["list"].Params {
		kinds[p.Name] = p.Kind
	}
	for _, p := range byLeaf["create"].Params {
		types[p.Name] = p.Type
	}
	if kinds["state"] != "query" || kinds["owner"] != "path" {
		t.Errorf("list param kinds = %v, want state=query owner=path", kinds)
	}
	if types["assignees"] != "[]string" || types["labels"] != "[]integer" {
		t.Errorf("create param types = %v, want assignees=[]string labels=[]integer", types)
	}
}
