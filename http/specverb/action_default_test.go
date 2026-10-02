package specverb

import (
	"context"
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
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/urfave/cli/v3"
)

// ciWatchDefaultGuardfile mirrors ciWatchGuardfile but gives `run` a `default`
// pre-flight JMESPath: no --run resolves the latest run.
func ciWatchDefaultGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	src := []byte("wrap ward ops forgejo {\n" +
		"    spec forgejo.swagger.v1.json\n" +
		"    base-url \"https://forgejo.example/api/v1\"\n" +
		"    auth header-token { header Authorization; prefix \"token \"; value ssm \"/forgejo/api-token\" }\n" +
		"    can list tasks { op \"ListActionTasks\" }\n" +
		"    action ci-watch {\n" +
		"        describe \"Watch a CI run to completion.\"\n" +
		"        input repo { positional; required; help \"owner/name\" }\n" +
		"        input run  { flag; default \"max([].run_number)\"; help \"run number (default: latest in the listing)\" }\n" +
		"        poll list tasks {\n" +
		"            args { owner-repo $repo }\n" +
		"            until \"\"\"\n" +
		"                length([?run_number==$run && status!='success'\n" +
		"                        && status!='failure' && status!='cancelled'\n" +
		"                        && status!='skipped']) == `0`\n" +
		"                \"\"\"\n" +
		"            every \"5ms\"\n" +
		"            timeout \"2s\"\n" +
		"            as run_tasks\n" +
		"        }\n" +
		"        fail-when \"length($run_tasks[?status=='failure']) > `0`\"\n" +
		"    }\n" +
		"}\n")
	gf, err := guardfile.Parse(src)
	if err != nil {
		t.Fatalf("parse ci-watch-default guardfile: %v", err)
	}
	return gf
}

// TestActionDefaultBindsLatestRun drives the pre-flight: no --run binds run to
// max([].run_number)=7, so settling at >=3 calls proves it bound 7, not 5.
func TestActionDefaultBindsLatestRun(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		// run 5 is always terminal; run 7 (the max) turns terminal only on the
		// third call, so a correct max-binding must keep polling past call 2.
		if n <= 2 {
			_, _ = w.Write([]byte(`[{"run_number":7,"status":"running","name":"build"},{"run_number":5,"status":"success","name":"old"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"run_number":7,"status":"success","name":"build"},{"run_number":5,"status":"success","name":"old"}]`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: ciWatchDefaultGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--output", "json")
	if err != nil {
		t.Fatalf("default poll run: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 3 {
		t.Errorf("expected the pre-flight plus polling of run 7 (>=3 calls), got %d", got)
	}
	if !strings.Contains(out, `"run_number": 7`) || !strings.Contains(out, `"status": "success"`) {
		t.Errorf("final listing not rendered:\n%s", out)
	}
}

// TestActionDefaultSuppliedSkipsPreflight asserts that supplying --run skips the
// pre-flight entirely: the explicit value wins and no extra wire call is made.
func TestActionDefaultSuppliedSkipsPreflight(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		// run 5 success, run 7 still running: with --run 5 the loop settles at once.
		_, _ = w.Write([]byte(`[{"run_number":7,"status":"running","name":"build"},{"run_number":5,"status":"success","name":"old"}]`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: ciWatchDefaultGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	if _, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--run", "5", "--output", "json"); err != nil {
		t.Fatalf("supplied run: %v", err)
	}
	// One call only: no pre-flight (run was supplied), loop settles on tick one.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected exactly 1 wire call (no pre-flight), got %d", got)
	}
}

// TestActionDefaultEmptyFailsClosed asserts an unresolvable default (empty
// listing => null) fails closed as a user error, not a loop on a silent null.
func TestActionDefaultEmptyFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`)) // empty listing => null default
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: ciWatchDefaultGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	_, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo")
	if err == nil {
		t.Fatal("expected a fail-closed error for an empty pre-flight listing, got nil")
	}
	if coded := exitcode.From(err); coded == nil || coded.Code() != exitcode.UserError {
		t.Errorf("error = %v, want a coded UserError", err)
	}
	if !strings.Contains(err.Error(), "resolved to null") {
		t.Errorf("error = %v, want a null-default message", err)
	}
}

// TestActionDefaultDryRunNamesPreflight asserts --dry-run with an absent
// defaulted input surfaces the pre-flight call and its bindings, firing nothing.
func TestActionDefaultDryRunNamesPreflight(t *testing.T) {
	cfg := Config{
		Guardfile:  ciWatchDefaultGuardfile(t),
		Spec:       actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}}, // any wire call fails the test
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "preflight") {
		t.Errorf("plan missing the pre-flight section:\n%s", out)
	}
	if !strings.Contains(out, "max([].run_number)") || !strings.Contains(out, `"input": "run"`) {
		t.Errorf("plan missing the default binding:\n%s", out)
	}
}

// TestActionDefaultDryRunSuppliedNoPreflight asserts the plan omits the
// pre-flight when the defaulted input is supplied (the actual invocation).
func TestActionDefaultDryRunSuppliedNoPreflight(t *testing.T) {
	cfg := Config{
		Guardfile:  ciWatchDefaultGuardfile(t),
		Spec:       actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--run", "5", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if strings.Contains(out, "preflight") {
		t.Errorf("plan should omit the pre-flight when --run is supplied:\n%s", out)
	}
}

// TestActionDefaultWritesPreflightAudit asserts the pre-flight writes its own
// leaf audit row, the same granted-only accounting a poll tick does.
func TestActionDefaultWritesPreflightAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"run_number":7,"status":"success","name":"build"}]`))
	}))
	defer srv.Close()

	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(func() { _ = w.Close() })
	cfg := Config{
		Guardfile: ciWatchDefaultGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Wrap:      func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) },
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	if _, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--output", "json"); err != nil {
		t.Fatalf("audited default run: %v", err)
	}
	data, _ := os.ReadFile(w.Path)
	rows := string(data)
	// the envelope row plus at least two leaf rows (one pre-flight, one poll tick)
	if strings.Count(rows, "ward.ops.forgejo.tasks.list") < 2 {
		t.Errorf("expected a pre-flight leaf row and a poll-tick leaf row:\n%s", rows)
	}
	if !strings.Contains(rows, "ward.ops.forgejo.action.ci-watch") {
		t.Errorf("missing the envelope audit row:\n%s", rows)
	}
}

// TestActionDefaultOnCallActionFailsClosed asserts a `default` on a call-action
// input is rejected at Build: defaulting is a poll-only pre-flight binding.
func TestActionDefaultOnCallActionFailsClosed(t *testing.T) {
	gf := callActionGuardfile(t)
	gf.Actions[0].Inputs = append(gf.Actions[0].Inputs, guardfile.Input{Name: "n", Default: "max([].number)"})
	_, err := Build(Config{Guardfile: gf, Spec: actionSpec(t)})
	if err == nil {
		t.Fatal("expected a build error for a default on a call action, got nil")
	}
	if !strings.Contains(err.Error(), "not supported on call actions") {
		t.Errorf("error = %v, want a call-action rejection", err)
	}
}

// TestActionDefaultArgConflictFailsClosed asserts an input that both defaults
// from the poll response and binds a poll arg is rejected at Build (circular).
func TestActionDefaultArgConflictFailsClosed(t *testing.T) {
	gf := ciWatchDefaultGuardfile(t)
	gf.Actions[0].Poll.Args = append(gf.Actions[0].Poll.Args, guardfile.ArgBind{Name: "page", Value: "$run"})
	_, err := Build(Config{Guardfile: gf, Spec: actionSpec(t)})
	if err == nil {
		t.Fatal("expected a build error for a defaulted input bound as a poll arg, got nil")
	}
	if !strings.Contains(err.Error(), "cannot also bind poll arg") {
		t.Errorf("error = %v, want a circular-binding rejection", err)
	}
}

// TestDescribeShowsDefaults asserts the describe surface and prose document the
// pre-flight default binding so the latest-run capability is discoverable.
func TestDescribeShowsDefaults(t *testing.T) {
	surface, err := Describe(Config{Guardfile: ciWatchDefaultGuardfile(t), Spec: actionSpec(t)})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(surface.Actions) != 1 || len(surface.Actions[0].Defaults) != 1 {
		t.Fatalf("Defaults = %+v, want one", surface.Actions)
	}
	d := surface.Actions[0].Defaults[0]
	if d.Input != "run" || d.JMESPath != "max([].run_number)" {
		t.Errorf("default = %+v, want run <- max([].run_number)", d)
	}
	md := surface.Markdown()
	if !strings.Contains(md, "Pre-flight defaults") || !strings.Contains(md, "max([].run_number)") {
		t.Errorf("prose missing the pre-flight default stanza:\n%s", md)
	}
}
