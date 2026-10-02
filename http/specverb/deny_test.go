package specverb

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/exitcode"
)

// denyFixture grants repo create + delete but denies create with a teaching
// message, over the shared Swagger 2.0 proving spec.
func denyFixture(t *testing.T) (*guardfile.Guardfile, []byte) {
	t.Helper()
	_, spec := loadFixtures(t)
	gf, err := guardfile.Parse([]byte(`wrap ward ops forgejo {
		spec forgejo.swagger.v1.json
		auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
		can create repo { op "createCurrentUserRepo" }
		can delete repo { op "repoDelete" }
		never delete repo {
			message "repo deletion is irreversible; archive instead"
		}
		never create orgs {
			message "org creation is a human-only operation"
		}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return gf, spec
}

// TestDenyBeatsAllow proves a `never` for a (verb,resource) removes the matching
// `can` leaf and replaces it with a teaching deny leaf.
func TestDenyBeatsAllow(t *testing.T) {
	gf, spec := denyFixture(t)
	root, err := Build(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	repo := childNamed(root, "repo")
	if repo == nil {
		t.Fatal("no repo group")
	}
	del := childNamed(repo, "delete")
	if del == nil {
		t.Fatal("delete leaf should still be mounted (as a deny leaf)")
	}
	if !strings.Contains(del.Description, "irreversible") {
		t.Errorf("delete deny leaf should carry the teaching message, got %q", del.Description)
	}
	// create survives (denied only delete + orgs-create).
	if childNamed(repo, "create") == nil {
		t.Error("create repo should still be allowed")
	}
}

// TestDeniedLeafFailsClosed proves invoking a denied leaf returns a PolicyDenied
// exit carrying the teaching message, never firing the wire.
func TestDeniedLeafFailsClosed(t *testing.T) {
	gf, spec := denyFixture(t)
	cfg := Config{Guardfile: gf, Spec: spec, HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) {
			t.Fatal("a denied leaf must not resolve a secret")
			return "", nil
		}}}
	_, err := runTree(t, cfg, "forgejo", "repo", "delete", "kai", "demo")
	if err == nil {
		t.Fatal("expected a deny error, got nil")
	}
	if coded := exitcode.From(err); coded == nil || coded.Code() != exitcode.PolicyDenied {
		t.Errorf("error = %v, want a coded PolicyDenied exit", err)
	}
	if !strings.Contains(err.Error(), "archive instead") {
		t.Errorf("deny error should carry the teaching message: %v", err)
	}
}

// TestDenyOnlyResourceMountsGroup proves a deny over a resource with no allow
// still mounts a teaching leaf (orgs create), so the operator learns why.
func TestDenyOnlyResourceMountsGroup(t *testing.T) {
	gf, spec := denyFixture(t)
	root, err := Build(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	orgs := childNamed(root, "orgs")
	if orgs == nil {
		t.Fatalf("deny-only resource should mount a group; got %v", names(root.Commands))
	}
	if childNamed(orgs, "create") == nil {
		t.Error("orgs create deny leaf missing")
	}
}

// TestDenyShowsInDescribe proves the describe surface and prose document the
// blocked classes with their teaching messages.
func TestDenyShowsInDescribe(t *testing.T) {
	gf, spec := denyFixture(t)
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(surface.Denied) != 2 {
		t.Fatalf("Denied = %d, want 2: %+v", len(surface.Denied), surface.Denied)
	}
	md := surface.Markdown()
	for _, want := range []string{"Denied operations", "repo delete (denied)", "org creation is a human-only operation"} {
		if !strings.Contains(md, want) {
			t.Errorf("describe prose missing %q:\n%s", want, md)
		}
	}
}
