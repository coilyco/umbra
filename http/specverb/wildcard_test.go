package specverb

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/exitcode"
)

// wildcardBuild parses body into a Guardfile over the proving slice and builds it.
func wildcardBuild(t *testing.T, body string) (*guardfile.Guardfile, []byte) {
	t.Helper()
	_, spec := loadFixtures(t)
	src := "wrap ward ops forgejo {\n" +
		"\tspec forgejo.swagger.v1.json\n" +
		"\tauth header-token { header Authorization; prefix \"token \"; value ssm \"/forgejo/api-token\" }\n" +
		body + "\n}"
	gf, err := guardfile.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return gf, spec
}

// leafSet returns the set of "group/leaf" names mounted under root, excluding the
// describe verb, so a wildcard's expanded surface can be asserted as a whole.
func leafSet(t *testing.T, gf *guardfile.Guardfile, spec []byte) map[string]string {
	t.Helper()
	root, err := Build(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := map[string]string{}
	for _, g := range root.Commands {
		if g.Name == "describe" {
			continue
		}
		for _, l := range g.Commands {
			out[g.Name+"/"+l.Name] = l.Usage
		}
	}
	return out
}

// TestWildcardReadonlyExample proves the committed readonly example Guardfile
// (testdata/forgejo-readonly.kdl) mounts only read leaves and denies every delete.
func TestWildcardReadonlyExample(t *testing.T) {
	kdl, err := os.ReadFile(filepath.Join("testdata", "forgejo-readonly.kdl"))
	if err != nil {
		t.Fatalf("read readonly guardfile: %v", err)
	}
	gf, err := guardfile.Parse(kdl)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, spec := loadFixtures(t)
	leaves := leafSet(t, gf, spec)

	for _, read := range []string{"repo/get", "issue/list", "task/list"} {
		if _, ok := leaves[read]; !ok {
			t.Errorf("readonly example missing read leaf %q; got %v", read, keysOf(leaves))
		}
	}
	if u := leaves["repo/delete"]; u != "denied by policy" {
		t.Errorf("readonly example should deny repo/delete, got %q", u)
	}
	for name, usage := range leaves {
		if strings.HasSuffix(name, "/create") || strings.HasSuffix(name, "/edit") {
			t.Errorf("readonly example leaked a write leaf %q (%s)", name, usage)
		}
	}
}

// TestWildcardCanMountsEveryReadLeaf proves a readonly guardfile (`can get "*"` +
// `can list "*"`) mounts only the spec's read leaves and no write leaf.
func TestWildcardCanMountsEveryReadLeaf(t *testing.T) {
	gf, spec := wildcardBuild(t, "\tcan get \"*\"\n\tcan list \"*\"")
	leaves := leafSet(t, gf, spec)

	want := []string{"repo/get", "issue/list", "task/list"}
	for _, w := range want {
		if _, ok := leaves[w]; !ok {
			t.Errorf("readonly surface missing %q; got %v", w, keysOf(leaves))
		}
	}
	if len(leaves) != len(want) {
		t.Errorf("readonly mounted %v, want exactly %v", keysOf(leaves), want)
	}
	// No write verb leaked in.
	for name := range leaves {
		switch {
		case strings.HasSuffix(name, "/create"), strings.HasSuffix(name, "/edit"),
			strings.HasSuffix(name, "/delete"):
			t.Errorf("readonly surface leaked a write leaf %q", name)
		}
	}
}

// TestWildcardCanResolvesRealOps proves each wildcard-expanded read leaf drives the
// operation its (verb,resource) resolves to, not a placeholder.
func TestWildcardCanResolvesRealOps(t *testing.T) {
	gf, spec := wildcardBuild(t, "\tcan get \"*\"\n\tcan list \"*\"")
	leaves := leafSet(t, gf, spec)
	wantUsage := map[string]string{
		"repo/get":   "GET /repos/{owner}/{repo}",
		"issue/list": "GET /repos/{owner}/{repo}/issues",
		"task/list":  "GET /repos/{owner}/{repo}/actions/tasks",
	}
	for name, usage := range wantUsage {
		if got := leaves[name]; got != usage {
			t.Errorf("%s usage = %q, want %q", name, got, usage)
		}
	}
}

// TestWildcardGrantReadsAsStarInHelp proves an expanded leaf names the verb-global
// grant that authorized it (`can get "*"`), not the synthesized concrete resource.
func TestWildcardGrantReadsAsStarInHelp(t *testing.T) {
	gf, spec := wildcardBuild(t, "\tcan get \"*\"")
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	var found bool
	for _, v := range surface.Verbs {
		if v.Group == "repo" && v.Leaf == "get" {
			found = true
			if v.Grant != `can get "*"` {
				t.Errorf("grant = %q, want `can get \"*\"`", v.Grant)
			}
		}
	}
	if !found {
		t.Fatal("repo get leaf missing from describe surface")
	}
	if !strings.Contains(surface.Markdown(), `can get "*"`) {
		t.Errorf("describe prose should render the wildcard grant:\n%s", surface.Markdown())
	}
}

// TestWildcardNeverBlocksEveryDeleteLeaf proves `never delete "*"` mounts a teaching
// deny leaf for every resource exposing delete, carrying the wildcard's message.
func TestWildcardNeverBlocksEveryDeleteLeaf(t *testing.T) {
	gf, spec := wildcardBuild(t,
		"\tcan get \"*\"\n\tnever delete \"*\" { message \"deletes are frozen fleet-wide\" }")
	leaves := leafSet(t, gf, spec)

	del, ok := leaves["repo/delete"]
	if !ok {
		t.Fatalf("never delete \"*\" should mount a deny leaf for repo; got %v", keysOf(leaves))
	}
	if del != "denied by policy" {
		t.Errorf("repo/delete usage = %q, want a deny leaf", del)
	}

	// Invoking it fails closed with the wildcard's teaching message, never firing.
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
	if !strings.Contains(err.Error(), "frozen fleet-wide") {
		t.Errorf("deny error should carry the wildcard message: %v", err)
	}
}

// TestWildcardNeverShadowsSpecificCan proves a `never <verb> "*"` overrides a
// specific `can <verb> X`: the can is dropped and a deny leaf stands in its place.
func TestWildcardNeverShadowsSpecificCan(t *testing.T) {
	gf, spec := wildcardBuild(t,
		"\tcan get repo\n\tcan edit issue\n\tnever edit \"*\" { message \"edits are frozen\" }")
	leaves := leafSet(t, gf, spec)

	if u := leaves["issue/edit"]; u != "denied by policy" {
		t.Errorf("issue/edit should be a deny leaf, got %q", u)
	}
	if _, ok := leaves["repo/get"]; !ok {
		t.Errorf("the unrelated `can get repo` must survive; got %v", keysOf(leaves))
	}
}

// TestWildcardCanExceptedBySpecificNever proves a specific `never <verb> X` carves
// an exception out of `can <verb> "*"` (allow-all-except), the inverse direction.
func TestWildcardCanExceptedBySpecificNever(t *testing.T) {
	gf, spec := wildcardBuild(t,
		"\tcan create \"*\"\n\tnever create repo { message \"repo creation is human-only\" }")
	leaves := leafSet(t, gf, spec)

	// repo/create is denied; every other create resource still mounts.
	if u := leaves["repo/create"]; u != "denied by policy" {
		t.Errorf("repo/create should be denied, got %q", u)
	}
	for _, allowed := range []string{"issue/create", "label/create", "asset/create"} {
		u, ok := leaves[allowed]
		if !ok {
			t.Errorf("allow-all-except dropped %q; got %v", allowed, keysOf(leaves))
			continue
		}
		if u == "denied by policy" {
			t.Errorf("%q should be allowed, not denied", allowed)
		}
	}
}

// TestWildcardExplicitGrantWins proves an explicit grant of the same (verb,resource)
// keeps its authored body rather than the wildcard's, and never double-mounts.
func TestWildcardExplicitGrantWins(t *testing.T) {
	gf, spec := wildcardBuild(t,
		"\tcan delete \"*\"\n\tcan delete repo { describe \"explicit override note\" }")
	surface, err := Describe(Config{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	var deletes int
	for _, v := range surface.Verbs {
		if v.Leaf == "delete" && v.Group == "repo" {
			deletes++
			if v.Describe != "explicit override note" {
				t.Errorf("explicit grant body lost: describe = %q", v.Describe)
			}
			if v.Grant != "can delete repo" {
				t.Errorf("explicit grant should read literally, got %q", v.Grant)
			}
		}
	}
	if deletes != 1 {
		t.Errorf("repo delete mounted %d times, want exactly 1 (no wildcard duplicate)", deletes)
	}
}

// TestWildcardUnknownVerbFailsClosed proves a wildcard over a verb with no built-in
// convention is a fail-closed error, not a silent no-op.
func TestWildcardUnknownVerbFailsClosed(t *testing.T) {
	gf, spec := wildcardBuild(t, "\tcan frobnicate \"*\"")
	_, err := Build(Config{Guardfile: gf, Spec: spec})
	if err == nil {
		t.Fatal("expected a fail-closed error for an unknown wildcard verb")
	}
	if !strings.Contains(err.Error(), "no built-in convention") {
		t.Errorf("error should name the missing convention: %v", err)
	}
}

// TestWildcardNoMatchFailsClosed proves a wildcard whose verb no resource exposes
// fails closed, catching a typo'd verb rather than mounting nothing silently.
func TestWildcardNoMatchFailsClosed(t *testing.T) {
	gf, spec := wildcardBuild(t, "\tcan get repo\n\tcan set \"*\"")
	_, err := Build(Config{Guardfile: gf, Spec: spec})
	if err == nil {
		t.Fatal("expected a fail-closed error when no resource exposes the verb")
	}
	if !strings.Contains(err.Error(), "no resource in the spec exposes verb") {
		t.Errorf("error should explain the empty expansion: %v", err)
	}
}

// TestWildcardPruneKeepsExpandedSurface proves Prune expands wildcards too, so the
// committed spec-lock holds exactly the read surface a readonly build mounts.
func TestWildcardPruneKeepsExpandedSurface(t *testing.T) {
	gf, spec := wildcardBuild(t, "\tcan get \"*\"\n\tcan list \"*\"")
	pruned, err := Prune(spec, gf)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// The pruned lock must still mount the same read surface.
	if _, err := Build(Config{Guardfile: gf, Spec: pruned}); err != nil {
		t.Fatalf("Build on pruned wildcard spec: %v", err)
	}
	leaves := leafSet(t, gf, pruned)
	for _, w := range []string{"repo/get", "issue/list", "task/list"} {
		if _, ok := leaves[w]; !ok {
			t.Errorf("pruned readonly surface missing %q; got %v", w, keysOf(leaves))
		}
	}
}
