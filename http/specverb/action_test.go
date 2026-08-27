package specverb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/cli/verb"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/guardfile"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/audit"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/exitcode"
	"github.com/urfave/cli/v3"
)

// ciWatchGuardfile is the first use case: a poll-until-terminal
// action over the granted ListActionTasks leaf, with a fail-when predicate.
func ciWatchGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	src := []byte("wrap ward ops forgejo {\n" +
		"    spec forgejo.swagger.v1.json\n" +
		"    base-url \"https://forgejo.coilysiren.me/api/v1\"\n" +
		"    auth header-token { header Authorization; prefix \"token \"; value ssm \"/forgejo/api-token\" }\n" +
		"    can list tasks { op \"ListActionTasks\" }\n" +
		"    action ci-watch {\n" +
		"        describe \"Watch a CI run to completion.\"\n" +
		"        input repo { positional; required; help \"owner/name\" }\n" +
		"        input run  { flag; help \"run number\" }\n" +
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
		t.Fatalf("parse ci-watch guardfile: %v", err)
	}
	return gf
}

func actionSpec(t *testing.T) []byte {
	t.Helper()
	spec, err := os.ReadFile(filepath.Join("testdata", "forgejo.swagger.v1.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	return spec
}

// TestActionDryRunIsAPlan asserts --dry-run on an action prints the call
// sequence with bound params and the compiled until, firing nothing.
func TestActionDryRunIsAPlan(t *testing.T) {
	cfg := Config{
		Guardfile:  ciWatchGuardfile(t),
		Spec:       actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}}, // any wire call fails the test
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--run", "5", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	// the owner/repo split filled both path params
	if !strings.Contains(out, "/repos/kai/demo/actions/tasks") {
		t.Errorf("plan missing the bound URL:\n%s", out)
	}
	// the compiled until is shown, not fired
	if !strings.Contains(out, "run_number==$run") {
		t.Errorf("plan missing the until expression:\n%s", out)
	}
	if !strings.Contains(out, "5ms") || !strings.Contains(out, "2s") {
		t.Errorf("plan missing the bounds:\n%s", out)
	}
	// the auth value is redacted in the plan (the JSON renderer escapes the
	// angle brackets of <redacted>, so match the inner word)
	if strings.Contains(out, "token x") || !strings.Contains(out, "redacted") {
		t.Errorf("plan did not redact the auth secret:\n%s", out)
	}
}

// TestActionPollUntilTerminal drives the loop: running on tick one, all-terminal
// on tick two. The action polls until `until` settles, then renders the listing.
func TestActionPollUntilTerminal(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = w.Write([]byte(`[{"run_number":5,"status":"running","name":"build"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"run_number":5,"status":"success","name":"build"}]`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: ciWatchGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "sekret", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--run", "5", "--output", "json")
	if err != nil {
		t.Fatalf("poll run: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Errorf("expected at least 2 poll ticks, got %d", got)
	}
	if !strings.Contains(out, `"status": "success"`) {
		t.Errorf("final listing not rendered:\n%s", out)
	}
}

// TestActionFailWhenSetsExit asserts a matched fail-when predicate is a non-zero
// exit, while the final listing still renders first.
func TestActionFailWhenSetsExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"run_number":5,"status":"failure","name":"build"}]`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: ciWatchGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--run", "5", "--output", "json")
	if err == nil {
		t.Fatal("expected a non-zero exit from the fail-when predicate, got nil")
	}
	if coded := exitcode.From(err); coded == nil || coded.Code() != exitcode.Generic {
		t.Errorf("error = %v, want a coded Generic exit", err)
	}
	if !strings.Contains(out, `"status": "failure"`) {
		t.Errorf("final listing should render before the fail-when exit:\n%s", out)
	}
}

// TestActionTimesOut asserts the timeout firing before until settles is a
// non-zero exit, never an unbounded loop.
func TestActionTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"run_number":5,"status":"running","name":"build"}]`)) // never terminal
	}))
	defer srv.Close()

	gf := ciWatchGuardfile(t)
	gf.Actions[0].Poll.Timeout = "40ms" // tighten so the test is quick
	cfg := Config{
		Guardfile: gf,
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	_, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--run", "5")
	if err == nil {
		t.Fatal("expected a timeout exit, got nil")
	}
	if coded := exitcode.From(err); coded == nil || coded.Code() != exitcode.UpstreamFailed {
		t.Errorf("error = %v, want a coded UpstreamFailed (action_timeout)", err)
	}
}

// TestDescribeShowsActions asserts the describe surface and prose document the
// complex action: its envelope name, the polled leaf, the bounds, the conditions.
func TestDescribeShowsActions(t *testing.T) {
	surface, err := Describe(Config{Guardfile: ciWatchGuardfile(t), Spec: actionSpec(t)})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(surface.Actions) != 1 {
		t.Fatalf("Actions = %d, want 1", len(surface.Actions))
	}
	a := surface.Actions[0]
	if a.Name != "ward.ops.forgejo.action.ci-watch" || a.Leaf != "ci-watch" {
		t.Errorf("action identity = %q / %q", a.Name, a.Leaf)
	}
	if a.Method != "GET" || a.Path != "/repos/{owner}/{repo}/actions/tasks" {
		t.Errorf("polled leaf = %s %s", a.Method, a.Path)
	}
	if a.Grant != "can list tasks" {
		t.Errorf("grant = %q, want can list tasks", a.Grant)
	}
	if a.FailWhen == "" || a.Until == "" {
		t.Errorf("conditions missing: until=%q fail_when=%q", a.Until, a.FailWhen)
	}
	md := surface.Markdown()
	if !strings.Contains(md, "forgejo action ci-watch") || !strings.Contains(md, "Complex action") {
		t.Errorf("prose missing the action stanza:\n%s", md)
	}
	// the dialect of `until`/`fail-when` must be named where a reader meets it
	if !strings.Contains(md, "Condition language") || !strings.Contains(md, "Community Edition") {
		t.Errorf("prose missing the condition-language note:\n%s", md)
	}
}

// TestActionGrantedOnlyFailsClosed asserts an action that polls an op the
// Guardfile does not `can`-grant fails at Build, not runtime.
func TestActionGrantedOnlyFailsClosed(t *testing.T) {
	gf := ciWatchGuardfile(t)
	// swap the `can list tasks` grant for an unrelated one: the tree still mounts
	// a verb, but the action now polls an op no grant authorizes
	gf.Grants = []guardfile.Grant{{Modal: "can", Verb: "get", Resource: "repo", Op: "repoGet"}}
	_, err := Build(Config{Guardfile: gf, Spec: actionSpec(t)})
	if err == nil {
		t.Fatal("expected a build error for an ungranted poll target, got nil")
	}
	if !strings.Contains(err.Error(), "deny-by-default") {
		t.Errorf("error = %v, want a deny-by-default message", err)
	}
}

// metacharActionGuardfile declares a one-call action whose `repo` binds a path
// (owner-repo) and `title` a body field, to exercise the metachar gate.
func metacharActionGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.coilysiren.me/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can create issue { op "issueCreateIssue" }
		action file {
			describe "File an issue."
			input repo  { positional; required; help "owner/name" }
			input title { flag; required; help "issue title" }
			call create issue {
				args { owner-repo $repo; title $title }
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse metachar-action guardfile: %v", err)
	}
	return gf
}

// TestActionBodyInputExemptFromMetacharGate proves a body-only action input
// (`title`) is gate-exempt while the path-bound `repo` is unaffected.
func TestActionBodyInputExemptFromMetacharGate(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":1}`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: metacharActionGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Wrap:      func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, gateWriter(t)) },
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	if _, err := runTree(t, cfg, "forgejo", "action", "file", "kai/demo",
		"--title", "crash on save (data loss)", "--output", "json"); err != nil {
		t.Fatalf("body-bound input with `(` must not trip the metachar gate: %v", err)
	}
	if !strings.Contains(gotBody, "data loss") {
		t.Errorf("title did not reach the request body: %q", gotBody)
	}
}

// TestActionPathInputStillGated proves the gate stays armed on the URL surface: a
// metacharacter in the path-bound `repo` input is rejected before any call fires.
func TestActionPathInputStillGated(t *testing.T) {
	cfg := Config{
		Guardfile:  metacharActionGuardfile(t),
		Spec:       actionSpec(t),
		Wrap:       func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, gateWriter(t)) },
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	_, err := runTree(t, cfg, "forgejo", "action", "file", "kai$(whoami)/demo", "--title", "ok")
	if err == nil {
		t.Fatal("a metacharacter in a path-bound action input must still be rejected")
	}
	if !strings.Contains(err.Error(), "shell metacharacter") {
		t.Errorf("want a shell-metacharacter rejection, got: %v", err)
	}
}

// gateWriter is a throwaway audit writer for tests that need the real verb
// pipeline (and thus its argv gate) without inspecting the audit rows.
func gateWriter(t *testing.T) *audit.Writer {
	t.Helper()
	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// TestActionWritesPerCallAndEnvelopeAudit asserts each poll tick writes its own
// leaf audit row and the action writes the envelope row tying them together.
func TestActionWritesPerCallAndEnvelopeAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"run_number":5,"status":"success","name":"build"}]`))
	}))
	defer srv.Close()

	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(func() { _ = w.Close() })

	cfg := Config{
		Guardfile: ciWatchGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Wrap:      func(s verb.Spec) cli.ActionFunc { return verb.Wrap(s, w) },
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	if _, err := runTree(t, cfg, "forgejo", "action", "ci-watch", "kai/demo", "--run", "5", "--output", "json"); err != nil {
		t.Fatalf("audited run: %v", err)
	}
	data, _ := os.ReadFile(w.Path)
	rows := string(data)
	if !strings.Contains(rows, "ward.ops.forgejo.action.ci-watch") {
		t.Errorf("missing the envelope audit row:\n%s", rows)
	}
	if !strings.Contains(rows, "ward.ops.forgejo.tasks.list") {
		t.Errorf("missing the per-call leaf audit row:\n%s", rows)
	}
}

// labelArrayGuardfile is #317's blocked shape: a mount shadow over `create issue`
// whose required `--labels` carries a list through to the create call, so the
// refusal happens before the write instead of being reported after it.
func labelArrayGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.coilysiren.me/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can create issue { op "issueCreateIssue" }
		action create issue {
			describe "File an issue, refusing one that carries no labels."
			input repo   { positional; required; help "owner/name" }
			input title  { flag; required; help "issue title" }
			input labels { flag; required; array; help "label id, repeatable" }
			call create issue {
				args { owner-repo $repo; title $title; labels $labels }
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse label-array guardfile: %v", err)
	}
	return gf
}

// TestActionArrayInputReachesBodyAsJSONArray proves a repeated action flag lands
// as a JSON array typed by the leaf's own schema, not as a flattened string.
func TestActionArrayInputReachesBodyAsJSONArray(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":1}`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: labelArrayGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	if _, err := runTree(t, cfg, "forgejo", "issue", "create", "kai/demo",
		"--title", "t", "--labels", "199", "--labels", "333", "--output", "json"); err != nil {
		t.Fatalf("array input: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body is not JSON: %q", gotBody)
	}
	labels, ok := body["labels"].([]any)
	if !ok {
		t.Fatalf("labels did not reach the body as an array: %q", gotBody)
	}
	if len(labels) != 2 || labels[0] != float64(199) || labels[1] != float64(333) {
		t.Errorf("want [199 333] as JSON numbers, got %#v (body %q)", labels, gotBody)
	}
}

// TestActionArrayInputRefusesWrongElementType proves the array refuses rather
// than degrading: the leaf declares integer items, so a name cannot pass.
func TestActionArrayInputRefusesWrongElementType(t *testing.T) {
	cfg := Config{
		Guardfile:  labelArrayGuardfile(t),
		Spec:       actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}}, // any wire call fails the test
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	_, err := runTree(t, cfg, "forgejo", "issue", "create", "kai/demo", "--title", "t", "--labels", "headless")
	if err == nil {
		t.Fatal("a non-integer element must be refused before the write, not sent")
	}
	if !strings.Contains(err.Error(), "is not an integer") {
		t.Errorf("want an element-type refusal, got: %v", err)
	}
}

// TestActionRequiredFlagRefusesBeforeTheWrite is the control this issue exists
// for: a missing required flag ends the run before any request is built, so the
// hazard is removed rather than reported afterwards.
func TestActionRequiredFlagRefusesBeforeTheWrite(t *testing.T) {
	cfg := Config{
		Guardfile:  labelArrayGuardfile(t),
		Spec:       actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	_, err := runTree(t, cfg, "forgejo", "issue", "create", "kai/demo", "--title", "t")
	if err == nil {
		t.Fatal("a missing required flag must refuse before the write")
	}
	if !strings.Contains(err.Error(), "missing required flag --labels") {
		t.Errorf("want a pre-write refusal naming the flag, got: %v", err)
	}
}

// TestActionArrayInputRejectedOnCollect pins the one form that still cannot
// carry a list, so it fails at Build rather than flattening at call time.
func TestActionArrayInputRejectedOnCollect(t *testing.T) {
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.coilysiren.me/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can list issues { op "issueListIssues" }
		action sweep {
			input repo   { positional; required; help "owner/name" }
			input labels { flag; array; help "labels" }
			collect list issues {
				args { owner-repo $repo; labels $labels }
				page-param page
				limit-param limit
				default-limit "50"
				as issues
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse collect guardfile: %v", err)
	}
	_, berr := Build(Config{
		Guardfile: gf,
		Spec:      actionSpec(t),
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	})
	if berr == nil {
		t.Fatal("an array input bound from a collect step must fail at Build")
	}
	if !strings.Contains(berr.Error(), "collect") {
		t.Errorf("want a message naming the collect limit, got: %v", berr)
	}
}

// labelCompositionGuardfile is agentic-os#1105's shape, checked by name so no
// per-org id table is needed. The leaf's `items: {}` union is what carries names.
func labelCompositionGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.coilysiren.me/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can add issue-label { op "issueAddLabel" }
		action label {
			describe "Apply a label set, refusing one missing a priority or an autonomy label."
			input repo   { positional; required; help "owner/name" }
			input index  { positional; required; help "issue number" }
			input labels {
				flag
				required
				array
				help "label name, repeatable"
				matches "priority/P[0-4]" message="no priority label: pass --labels priority/P2 (P0-P4)"
				matches "autonomy/headless" "autonomy/live-collab" "autonomy/async-consult" "autonomy/epic" message="no autonomy label: pass --labels autonomy/headless"
			}
			call add issue-label {
				args { owner-repo $repo; index $index; labels $labels }
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse label-composition guardfile: %v", err)
	}
	return gf
}

// compositionConfig builds a Config whose transport fails the test, so anything
// reaching the wire is a bug rather than a soft assertion.
func compositionConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Guardfile:  labelCompositionGuardfile(t),
		Spec:       actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
}

// TestActionInputMatchesRefusesMissingPriority is agentic-os#1105's control: the
// refusal lands before the write and names which axis is missing.
func TestActionInputMatchesRefusesMissingPriority(t *testing.T) {
	_, err := runTree(t, compositionConfig(t), "forgejo", "action", "label", "kai/demo", "7",
		"--labels", "autonomy/headless", "--labels", "role/platform")
	if err == nil {
		t.Fatal("a set with no priority label must be refused before the write")
	}
	if !strings.Contains(err.Error(), "no priority label") {
		t.Errorf("the refusal must name the missing axis, got: %v", err)
	}
}

// TestActionInputMatchesRefusesMissingAutonomy is the other half: naming only
// the priority axis must not satisfy the autonomy constraint.
func TestActionInputMatchesRefusesMissingAutonomy(t *testing.T) {
	_, err := runTree(t, compositionConfig(t), "forgejo", "action", "label", "kai/demo", "7",
		"--labels", "priority/P2")
	if err == nil {
		t.Fatal("a set with no autonomy label must be refused before the write")
	}
	if !strings.Contains(err.Error(), "no autonomy label") {
		t.Errorf("the refusal must name the missing axis, got: %v", err)
	}
}

// TestActionInputMatchesAcceptsCompleteSet proves it constrains rather than
// denies: a complete set reaches the wire as names, extras untouched.
func TestActionInputMatchesAcceptsCompleteSet(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	cfg := compositionConfig(t)
	cfg.HTTPClient = nil
	cfg.BaseURL = srv.URL
	if _, err := runTree(t, cfg, "forgejo", "action", "label", "kai/demo", "7",
		"--labels", "priority/P2", "--labels", "autonomy/headless", "--labels", "role/platform",
		"--output", "json"); err != nil {
		t.Fatalf("a complete label set must pass: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body is not JSON: %q", gotBody)
	}
	labels, ok := body["labels"].([]any)
	if !ok || len(labels) != 3 {
		t.Fatalf("want three label names on the wire, got %#v (body %q)", body["labels"], gotBody)
	}
	if labels[0] != "priority/P2" || labels[2] != "role/platform" {
		t.Errorf("names must reach the wire unquoted-to-string, got %#v", labels)
	}
}

// TestActionInputMatchesRefusesANearMiss is why the globs enumerate: the labels
// endpoint drops an unknown name silently, so `priority/*` would apply nothing.
func TestActionInputMatchesRefusesANearMiss(t *testing.T) {
	for _, bad := range []string{"priority/P9", "priority/p2", "priority/NOPE"} {
		_, err := runTree(t, compositionConfig(t), "forgejo", "action", "label", "kai/demo", "7",
			"--labels", bad, "--labels", "autonomy/headless")
		if err == nil {
			t.Errorf("%q is outside the vocabulary and must be refused", bad)
			continue
		}
		if !strings.Contains(err.Error(), "no priority label") {
			t.Errorf("%q: want the priority refusal, got: %v", bad, err)
		}
	}
}

// optionalArgGuardfile shadows `create issue` the way agentic-os#1105 wants to:
// required inputs plus optional ones the caller usually omits.
func optionalArgGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.coilysiren.me/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can create issue { op "issueCreateIssue" }
		action create issue {
			describe "File an issue, carrying the leaf's optional fields."
			input repo      { positional; required; help "owner/name" }
			input title     { flag; required; help "issue title" }
			input body      { flag; help "issue body" }
			input milestone { flag; help "milestone id" }
			call create issue {
				args { owner-repo $repo; title $title; body $body; milestone $milestone }
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse optional-arg guardfile: %v", err)
	}
	return gf
}

// TestActionOmittedOptionalArgIsDroppedNotFailed: an omitted input leaves its
// field absent rather than failing the call, so a shadow can carry optionals.
func TestActionOmittedOptionalArgIsDroppedNotFailed(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":1}`))
	}))
	defer srv.Close()

	cfg := Config{
		Guardfile: optionalArgGuardfile(t),
		Spec:      actionSpec(t),
		BaseURL:   srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	if _, err := runTree(t, cfg, "forgejo", "issue", "create", "kai/demo",
		"--title", "t", "--output", "json"); err != nil {
		t.Fatalf("omitting an optional flag must not fail the call: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body is not JSON: %q", gotBody)
	}
	if body["title"] != "t" {
		t.Errorf("the supplied input must still bind, got %q", gotBody)
	}
	for _, absent := range []string{"body", "milestone"} {
		if _, present := body[absent]; present {
			t.Errorf("omitted %q must be absent, not sent empty or as a placeholder: %q", absent, gotBody)
		}
	}
}

// TestActionOmittedOptionalArgIsDroppedInTheDryPlan keeps the plan honest: no
// ${placeholder} for something the live call drops.
func TestActionOmittedOptionalArgIsDroppedInTheDryPlan(t *testing.T) {
	cfg := Config{
		Guardfile:  optionalArgGuardfile(t),
		Spec:       actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers:  map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }},
	}
	out, err := runTree(t, cfg, "forgejo", "issue", "create", "kai/demo", "--title", "t", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if strings.Contains(out, "${milestone}") || strings.Contains(out, "${body}") {
		t.Errorf("the plan must not carry a placeholder for an omitted optional:\n%s", out)
	}
}
