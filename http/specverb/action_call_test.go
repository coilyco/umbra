package specverb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// callActionGuardfile grants issue create + close and declares a two-step call
// action threading $src.number into the close - the move-issue data-flow shape.
func callActionGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.example/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can create issue { op "issueCreateIssue" }
		can close issue { op "issueEditIssue"; body state="closed" }
		action seal {
			describe "Create an issue then immediately close it."
			input repo { positional; required; help "owner/name" }
			call create issue {
				args { owner-repo $repo; title "sealed" }
				as src
			}
			call close issue {
				args { owner-repo $repo; index $src.number }
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse call-action guardfile: %v", err)
	}
	return gf
}

// TestCallActionDataFlow drives the sequence: the create response's number flows
// into the close call's path param, and the fixed body closes the issue.
func TestCallActionDataFlow(t *testing.T) {
	var calls int32
	var createBody, closeMethod, closePath, closeBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		b, _ := io.ReadAll(r.Body)
		if n == 1 { // create
			createBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":42,"title":"sealed"}`))
			return
		}
		closeMethod, closePath, closeBody = r.Method, r.URL.Path, string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"state":"closed"}`))
	}))
	defer srv.Close()

	cfg := Config{Guardfile: callActionGuardfile(t), Spec: actionSpec(t), BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	out, err := runTree(t, cfg, "forgejo", "action", "seal", "kai/demo", "--output", "json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 calls, got %d", got)
	}
	if !strings.Contains(createBody, `"title":"sealed"`) {
		t.Errorf("create body = %q", createBody)
	}
	if closeMethod != "PATCH" || closePath != "/repos/kai/demo/issues/42" {
		t.Errorf("close = %s %s, want PATCH /repos/kai/demo/issues/42 (data-flow from $src.number)", closeMethod, closePath)
	}
	if closeBody != `{"state":"closed"}` {
		t.Errorf("close body = %q, want the fixed state body", closeBody)
	}
	if !strings.Contains(out, `"state": "closed"`) {
		t.Errorf("final response (last call) not rendered:\n%s", out)
	}
}

// TestCallActionDryRunPlan proves --dry-run prints the planned sequence with the
// data-flow reference unresolved, firing nothing.
func TestCallActionDryRunPlan(t *testing.T) {
	cfg := Config{Guardfile: callActionGuardfile(t), Spec: actionSpec(t),
		HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) {
			t.Fatal("dry-run must not fire or resolve a secret")
			return "", nil
		}}}
	out, err := runTree(t, cfg, "forgejo", "action", "seal", "kai/demo", "--dry-run", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	var plan struct {
		Action string           `json:"action"`
		Calls  []map[string]any `json:"calls"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan is not json: %v\n%s", err, out)
	}
	if plan.Action != "seal" || len(plan.Calls) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Calls[0]["url"] != "https://forgejo.example/api/v1/repos/kai/demo/issues" {
		t.Errorf("first call url = %v", plan.Calls[0]["url"])
	}
}

// TestCallActionGrantedOnly proves a call step that names an ungranted op fails
// closed at Build, not at runtime.
func TestCallActionGrantedOnly(t *testing.T) {
	gf := callActionGuardfile(t)
	// drop the close grant: the action's second call now polls an ungranted op
	kept := gf.Grants[:0:0]
	for _, g := range gf.Grants {
		if g.Verb != "close" || g.Resource != "issue" {
			kept = append(kept, g)
		}
	}
	gf.Grants = kept
	_, err := Build(Config{Guardfile: gf, Spec: actionSpec(t)})
	if err == nil {
		t.Fatal("expected a deny-by-default build error for an ungranted call, got nil")
	}
	if !strings.Contains(err.Error(), "deny-by-default") {
		t.Errorf("error = %v, want deny-by-default", err)
	}
}

// TestCallActionDescribed proves the describe surface and prose document the
// call sequence: each step's method/path and its `as` binding.
func TestCallActionDescribed(t *testing.T) {
	surface, err := Describe(Config{Guardfile: callActionGuardfile(t), Spec: actionSpec(t)})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(surface.Actions) != 1 || len(surface.Actions[0].Calls) != 2 {
		t.Fatalf("actions = %+v", surface.Actions)
	}
	if surface.Actions[0].Calls[0].As != "src" {
		t.Errorf("first call as = %q, want src", surface.Actions[0].Calls[0].As)
	}
	md := surface.Markdown()
	for _, want := range []string{"forgejo action seal", "Runs 2 granted calls", "binds the response as `src`"} {
		if !strings.Contains(md, want) {
			t.Errorf("prose missing %q:\n%s", want, md)
		}
	}
}
