package specverb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// tierChain writes a read base and a higher tier inheriting it under a temp dir,
// returning the higher tier's parsed guardfile and the shared proving spec.
func tierChain(t *testing.T, base, higher string) (*guardfile.Guardfile, []byte) {
	t.Helper()
	_, spec := loadFixtures(t)
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("read.guardfile.kdl", "wrap ward ops forgejo {\n"+
		"\tspec forgejo.swagger.v1.json\n"+
		"\tauth header-token { header Authorization; prefix \"token \"; value ssm \"/forgejo/api-token\" }\n"+
		base+"\n}\n")
	write("higher.guardfile.kdl", "wrap ward ops forgejo {\n"+
		"\tinherit \"read.guardfile.kdl\"\n"+
		higher+"\n}\n")
	gf, err := guardfile.ParseFile(filepath.Join(dir, "higher.guardfile.kdl"))
	if err != nil {
		t.Fatalf("ParseFile higher: %v", err)
	}
	return gf, spec
}

// TestOverrideCrossesInheritedNever proves `override can delete repo` lifts the
// inherited `never delete "*"` for repo: a live op leaf that fires the wire.
func TestOverrideCrossesInheritedNever(t *testing.T) {
	gf, spec := tierChain(t, "\tcan get \"*\"\n\tnever delete \"*\"", "\toverride can delete repo")
	leaves := leafSet(t, gf, spec)

	if u, ok := leaves["repo/delete"]; !ok || u == "denied by policy" {
		t.Fatalf("repo/delete should be a live override leaf, got %q (ok=%v)", u, ok)
	}

	// The override leaf is real: invoking it reaches the wire (a deny never would).
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	cfg := Config{Guardfile: gf, Spec: spec, BaseURL: srv.URL,
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) { return "x", nil }}}
	if _, err := runTree(t, cfg, "forgejo", "repo", "delete", "coilyco", "demo"); err != nil {
		t.Fatalf("override delete should fire: %v", err)
	}
	if !hit {
		t.Error("override leaf did not reach the wire (still behaving as a deny)")
	}
}

// TestOverrideLiftsNamedResourceOnly proves an override re-grants exactly its named
// resource: under inherited `never create "*"`, only the overridden repo is freed.
func TestOverrideLiftsNamedResourceOnly(t *testing.T) {
	gf, spec := tierChain(t, "\tcan get \"*\"\n\tnever create \"*\"", "\toverride can create repo")
	leaves := leafSet(t, gf, spec)

	if u, ok := leaves["repo/create"]; !ok || u == "denied by policy" {
		t.Errorf("repo/create should be the lifted override leaf, got %q (ok=%v)", u, ok)
	}
	for _, denied := range []string{"issue/create", "label/create"} {
		if u := leaves[denied]; u != "denied by policy" {
			t.Errorf("%s was not overridden and must stay denied, got %q", denied, u)
		}
	}
}

// TestOverrideAbsentFromDeniedSurface proves the describe surface lists an
// overridden leaf as an authorized verb (reading `override can ...`), not denied.
func TestOverrideAbsentFromDeniedSurface(t *testing.T) {
	gf, spec := tierChain(t, "\tcan get \"*\"\n\tnever delete \"*\"", "\toverride can delete repo")
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, d := range surface.Denied {
		if d.Group == "repo" && d.Leaf == "delete" {
			t.Error("repo delete is overridden; it must not appear in the Denied surface")
		}
	}
	var found bool
	for _, v := range surface.Verbs {
		if v.Group == "repo" && v.Leaf == "delete" {
			found = true
			if v.Grant != "override can delete repo" {
				t.Errorf("override leaf grant = %q, want `override can delete repo`", v.Grant)
			}
		}
	}
	if !found {
		t.Error("overridden repo delete missing from the authorized Verbs surface")
	}
}

// TestWildcardCanCannotCrossInheritedNever proves a wildcard `can create "*"` does
// not cross an inherited `never create issue`: issue stays denied, others mount.
func TestWildcardCanCannotCrossInheritedNever(t *testing.T) {
	gf, spec := tierChain(t, "\tnever create issue", "\tcan create \"*\"")
	leaves := leafSet(t, gf, spec)

	if u := leaves["issue/create"]; u != "denied by policy" {
		t.Errorf("inherited `never create issue` must keep issue create denied, got %q", u)
	}
	if u, ok := leaves["repo/create"]; !ok || u == "denied by policy" {
		t.Errorf("repo create should mount under the wildcard, got %q (ok=%v)", u, ok)
	}
}

// TestInheritedRestrictGatesHigherTier proves a base `restrict` now gates an
// un-restating higher tier end to end: an out-of-scope owner is denied.
func TestInheritedRestrictGatesHigherTier(t *testing.T) {
	gf, spec := tierChain(t,
		"\trestrict owner matches \"coilyco-*\"\n\tcan get \"*\"",
		"\tcan edit \"*\"")
	cfg := Config{Guardfile: gf, Spec: spec, HTTPClient: &http.Client{Transport: failingTransport{t}},
		Providers: map[string]Provider{"ssm": func(context.Context, string) (string, error) {
			t.Fatal("an out-of-scope arg must not reach the wire")
			return "", nil
		}}}
	_, err := runTree(t, cfg, "forgejo", "repo", "get", "someone-else", "demo")
	if err == nil {
		t.Fatal("inherited restrict should deny an out-of-scope owner")
	}
	if !strings.Contains(err.Error(), "outside the allowed scope") {
		t.Errorf("deny should cite the inherited restrict: %v", err)
	}
}
