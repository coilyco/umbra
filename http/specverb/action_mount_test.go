package specverb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// mountActionGuardfile shadows the generated `repo get` leaf with a two-call
// action fetching the repo and its issues (the `issue view` + comments shape).
func mountActionGuardfile(t *testing.T) *guardfile.Guardfile {
	t.Helper()
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		base-url "https://forgejo.example/api/v1"
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can get repo { op "repoGet" }
		can list issue { op "issueListIssues" }
		action get repo {
			describe "View a repo with its open issues."
			input source { positional; required; help "owner/name" }
			call get repo {
				args { owner-repo $source }
				as repo
			}
			call list issue {
				args { owner-repo $source }
				as issues
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parse mount-action guardfile: %v", err)
	}
	return gf
}

// TestMountActionShadowsLeaf proves `repo get` resolves to the action (two GET
// calls, combined render of both `as` bindings), not the bare generated leaf.
func TestMountActionShadowsLeaf(t *testing.T) {
	var repoHits, issuesHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kai/demo":
			atomic.AddInt32(&repoHits, 1)
			_, _ = w.Write([]byte(`{"full_name":"kai/demo"}`))
		case "/repos/kai/demo/issues":
			atomic.AddInt32(&issuesHits, 1)
			_, _ = w.Write([]byte(`[{"number":1,"title":"first"}]`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := Config{Guardfile: mountActionGuardfile(t), Spec: actionSpec(t), BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	out, err := runTree(t, cfg, "forgejo", "repo", "get", "kai/demo", "--output", "json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if repoHits != 1 || issuesHits != 1 {
		t.Fatalf("expected one hit each, got repo=%d issues=%d (the bare leaf would fire only the repo GET)", repoHits, issuesHits)
	}
	var combined map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &combined); err != nil {
		t.Fatalf("combined output is not a json object: %v\n%s", err, out)
	}
	if _, ok := combined["repo"]; !ok {
		t.Errorf("combined output missing `repo` binding:\n%s", out)
	}
	if _, ok := combined["issues"]; !ok {
		t.Errorf("combined output missing `issues` binding:\n%s", out)
	}
}

// TestMountActionQueryProjectsCombinedResponse proves a shadow preserves the
// generated leaf's universal response-projection flag on its combined result.
func TestMountActionQueryProjectsCombinedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kai/demo":
			_, _ = w.Write([]byte(`{"full_name":"kai/demo"}`))
		case "/repos/kai/demo/issues":
			_, _ = w.Write([]byte(`[{"number":1,"title":"first"}]`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := Config{Guardfile: mountActionGuardfile(t), Spec: actionSpec(t), BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	out, err := runTree(t, cfg, "forgejo", "repo", "get", "kai/demo", "--query", "issues[].{number:number}", "--output", "json")
	if err != nil {
		t.Fatalf("run with --query: %v", err)
	}
	var projected []map[string]any
	if err := json.Unmarshal([]byte(out), &projected); err != nil {
		t.Fatalf("projected output is not a JSON array: %v\n%s", err, out)
	}
	if len(projected) != 1 || projected[0]["number"] != float64(1) {
		t.Errorf("projection = %#v, want one issue number", projected)
	}
}

// TestMountActionSuppressesGeneratedLeaf proves the generated `repo get` leaf is
// gone from the describe surface, the action having mounted in its place.
func TestMountActionSuppressesGeneratedLeaf(t *testing.T) {
	surface, err := Describe(Config{Guardfile: mountActionGuardfile(t), Spec: actionSpec(t)})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, v := range surface.Verbs {
		if v.Group == "repo" && v.Leaf == "get" {
			t.Fatalf("generated `repo get` leaf should be suppressed by the mount action, found %+v", v)
		}
	}
	// the action mounted at the shadowed path, carrying the leaf's audit identity.
	var found bool
	for _, a := range surface.Actions {
		if a.Name == "ward.ops.forgejo.repo.get" {
			found = true
		}
	}
	if !found {
		t.Errorf("mount action should carry the shadowed leaf's audit name, actions = %+v", surface.Actions)
	}
	// the prose reads at the shadowed leaf path, not a phantom `action` noun.
	md := surface.Markdown()
	for _, want := range []string{"## ward ops forgejo repo get", "Shadows the generated `repo get` leaf"} {
		if !strings.Contains(md, want) {
			t.Errorf("prose missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "forgejo action get-repo") {
		t.Errorf("prose should not mount the mount action under the `action` noun:\n%s", md)
	}
}

// TestMountActionMountsUnderResourceGroup proves the action command sits at
// `repo get`, not under the `action` noun.
func TestMountActionMountsUnderResourceGroup(t *testing.T) {
	root, err := Build(Config{Guardfile: mountActionGuardfile(t), Spec: actionSpec(t)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	repo := childNamed(root, "repo")
	if repo == nil {
		t.Fatalf("want a `repo` group, got %v", names(root.Commands))
	}
	if childNamed(repo, "get") == nil {
		t.Fatalf("want `repo get` mounted, repo group = %v", names(repo.Commands))
	}
	// no `action` noun: the only action is a mount, so the group is absent.
	if childNamed(root, "action") != nil {
		t.Errorf("a mount-only guardfile should not mount the `action` noun")
	}
}
